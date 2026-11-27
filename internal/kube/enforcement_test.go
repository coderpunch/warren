package kube

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

var errNoRoute = errors.New("connect: no route to host")

// openCluster makes a cluster that does not apply network policies: a
// connection to a test pod is successful as long as the pod exists. Each
// test pod has its own address. onDial runs before each connection attempt,
// with the number of the attempt.
func openCluster(t *testing.T, mod func(*Config), onDial func(e *testEnv, n int32)) *testEnv {
	t.Helper()
	var e *testEnv
	var dials, pods atomic.Int32
	dial := func(_ context.Context, address string) error {
		if onDial != nil {
			onDial(e, dials.Add(1))
		}
		for _, p := range e.canaries() {
			if strings.HasPrefix(address, p.Status.PodIP+":") && p.DeletionTimestamp == nil {
				return nil
			}
		}
		return errNoRoute
	}
	e = newTestEnv(t, func(c *Config) {
		c.Dial = dial
		if mod != nil {
			mod(c)
		}
	})
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if pod.Labels[LabelCanary] == "true" {
			readyStatus(pod)
			pod.Status.PodIP = fmt.Sprintf("10.9.0.%d", pods.Add(1))
		}
		return false, nil, nil
	})
	e.m.enforcement.Store(enforcementUnknown)
	return e
}

// The test pod can go away during the test: an eviction, a stop of its
// node, a delete by a person. The connection then fails, but that failure
// does not show that the cluster applies network policies.
func TestTestPodThatGoesAwayIsNotAProof(t *testing.T) {
	e := openCluster(t, nil, func(e *testEnv, n int32) {
		// Attempt 1 is the first step. Attempt 2 is the first attempt with
		// no policy: it is successful. Then the pod goes away.
		if n == 3 {
			zero := int64(0)
			for _, p := range e.canaries() {
				_ = e.client.CoreV1().Pods(testNamespace).Delete(context.Background(), p.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
			}
		}
	})
	e.m.examineEnforcement(context.Background())
	if e.m.Ready() {
		t.Fatal("the manager is ready on a cluster that ignores network policies: the test pod went away, and warren read that as a refused connection")
	}
	if got := e.m.enforcement.Load(); got != enforcementUnknown {
		t.Errorf("result = %d, want no result", got)
	}
	if n := len(e.networkPolicies()); n != 1 {
		t.Errorf("network policies after the test = %d, want only the policy of the namespace", n)
	}
}

// A rollout has two warren processes for a short time. The test of the
// second process must not change the result of the first one.
func TestTwoProcessesTestTheNetworkAtTheSameTime(t *testing.T) {
	var second *Manager
	e := openCluster(t, nil, func(e *testEnv, n int32) {
		// The first process is in the second step, and its first attempt
		// was successful. Now the second process does its complete test.
		if n == 3 && second != nil {
			m := second
			second = nil
			m.examineEnforcement(context.Background())
			if m.Ready() {
				t.Error("the second process is ready on a cluster that ignores network policies")
			}
		}
	})
	cfg := e.m.cfg
	second = e.newManager(cfg)
	second.enforcement.Store(enforcementUnknown)
	e.m.examineEnforcement(context.Background())
	if e.m.Ready() {
		t.Fatal("the first process is ready on a cluster that ignores network policies: the second process deleted its test pod")
	}
	if got := e.m.enforcement.Load(); got != enforcementAbsent {
		t.Errorf("result of the first process = %d, want %d: the second process must not stop the test", got, enforcementAbsent)
	}
	if n := len(e.canaries()); n != 0 {
		t.Errorf("test pods after the two tests = %d, want 0", n)
	}
}

// An error of warren itself, for example no free file descriptor, is not a
// refused connection.
func TestLocalConnectionErrorIsNotAProof(t *testing.T) {
	var failing atomic.Bool
	e := openCluster(t, func(c *Config) {
		open := c.Dial
		c.Dial = func(ctx context.Context, address string) error {
			if failing.Load() {
				return errors.New("dial tcp: socket: too many open files")
			}
			return open(ctx, address)
		}
	}, func(_ *testEnv, n int32) { failing.Store(n >= 3) })
	e.m.examineEnforcement(context.Background())
	if e.m.Ready() {
		t.Fatal("the manager is ready on a cluster that ignores network policies, after a local connection error")
	}
}

// While the first test of the network plugin has no result, warren is not
// ready, and no person can stop a sandbox through warren. The reaper must
// operate: if not, a namespace at its quota stays full with no end.
func TestReaperOperatesWhileTheNetworkTestIsNotPossible(t *testing.T) {
	var tests atomic.Int32
	e := newTestEnv(t, func(c *Config) { c.ReapInterval = 10 * time.Millisecond })
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Labels[LabelCanary] == "true" {
			tests.Add(1)
			return true, nil, errors.New(`pods "warren-canary" is forbidden: exceeded quota`)
		}
		return false, nil, nil
	})
	req := templateRef("quick", "alice")
	e.acquire(req).Release()
	// Warren starts again.
	e.m.enforcement.Store(enforcementUnknown)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "first test of the network plugin", func() bool { return tests.Load() >= 1 })
	if e.m.Ready() {
		t.Fatal("the manager is ready with no result of the network test")
	}
	// The sandbox becomes idle after the reaper run of the start.
	e.clock.Advance(time.Hour)
	waitFor(t, "stop of the idle sandbox while warren is not ready", func() bool { return e.m.pod(req.Ref.Name()) == nil })
}

// The policy of the namespace can go away after the start, and warren can be
// unable to make it again. Warren must then start no sandbox.
func TestNamespacePolicyThatWarrenCannotMakeAgain(t *testing.T) {
	e := newTestEnv(t, nil)
	e.acquire(userRef("alice")).Release()
	ctx := context.Background()
	// A pod of a different owner. It is only in the list that warren asks
	// for: the pod cache of the simulated cluster ignores the label
	// selector, and the reaper would read the pod as a pod of warren.
	var other atomic.Bool
	e.client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !other.Load() {
			return false, nil, nil
		}
		return true, &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "database-0", Namespace: testNamespace}}}}, nil
	})
	other.Store(true)
	if err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Delete(ctx, defaultDenyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	e.m.Reap(ctx)
	if _, ok := e.networkPolicies()[defaultDenyName]; ok {
		t.Fatal("warren made the policy in a namespace with a pod of a different owner")
	}
	if e.m.Ready() {
		t.Error("the manager is ready, and the sandbox namespace has no policy that refuses all connections")
	}
	if reason := e.m.NotReadyReason(); !strings.Contains(reason, "database-0") {
		t.Errorf("reason = %q, want the cause", reason)
	}
	if err := e.m.Start(ctx, userRef("bob")); err == nil {
		t.Error("a sandbox started in a namespace with no policy that refuses all connections")
	}
	// The administrator removes the pod. The next reaper run makes the
	// policy, and warren is ready again.
	other.Store(false)
	e.m.Reap(ctx)
	if _, ok := e.networkPolicies()[defaultDenyName]; !ok || !e.m.Ready() {
		t.Errorf("policy made = %v, not ready: %q", ok, e.m.NotReadyReason())
	}
}

// A removed network permission must stop the sandbox, also when the first
// delete request fails.
func TestRemovedPermissionStopsTheSandboxAfterAFailedDelete(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("quick", "alice")
	e.acquire(req).Release()
	uid := e.m.pod(req.Ref.Name()).UID
	var failures atomic.Int32
	e.client.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failures.Add(1) == 1 {
			return true, nil, errors.New("the server was unable to return a response in the time allotted")
		}
		return false, nil, nil
	})
	e.usePolicies("policies:\n  - {id: quick, name: Quick, scope: user, storage: {type: ephemeral}, network: {egress: none}}\n")
	e.m.Reap(context.Background())
	if failures.Load() != 1 {
		t.Fatalf("delete requests = %d, want 1", failures.Load())
	}
	// The policy has the new rules immediately.
	if np := e.policyOf(req); len(np.Spec.Egress) != 0 {
		t.Errorf("egress rules after the change to none = %+v", np.Spec.Egress)
	}
	e.m.Reap(context.Background())
	waitFor(t, "stop of the sandbox in the reaper run after the failed delete", func() bool {
		p := e.m.pod(req.Ref.Name())
		return p == nil || p.UID != uid
	})
}

// A node address in a network that the policy refuses already must not be
// in the policy. If it was, each change of the set of nodes would change
// the policies of all sandboxes.
func TestPrivateNodeAddressesDoNotChangeThePolicies(t *testing.T) {
	e := newTestEnv(t, nil)
	onNode := func(user, ip string) {
		req := userRef(user)
		e.acquire(req).Release()
		e.setStatus(req.Ref.Name(), func(p *corev1.Pod) {
			p.Status.HostIP = ip
			p.Status.HostIPs = []corev1.HostIP{{IP: ip}}
		})
		waitFor(t, "node address in the cache", func() bool { return e.m.pod(req.Ref.Name()).Status.HostIP == ip })
	}
	for _, user := range []string{"u1", "u2", "u3", "u4", "u5"} {
		onNode(user, "10.0.0.1")
	}
	e.m.Reap(context.Background())
	updates := func() int {
		n := 0
		for _, a := range e.client.Actions() {
			if a.Matches("update", "networkpolicies") {
				n++
			}
		}
		return n
	}
	before := updates()
	onNode("u6", "10.0.0.2")
	e.m.Reap(context.Background())
	if n := updates() - before; n != 0 {
		t.Errorf("a sandbox on a second node with a private address changed %d network policies, want 0", n)
	}
	if got := e.policyOf(userRef("u1")).Spec.Egress[1].To[0].IPBlock.Except; len(got) != len(blockedIPv4) {
		t.Errorf("exceptions = %v, want only the %d networks of the configuration", got, len(blockedIPv4))
	}
}

// slowWarning does one test of the network plugin on a cluster that applies
// the delete of a policy after the given number of connection attempts. The
// clock of the test moves as the clock of a cluster does: 4 seconds for each
// pause, 3 seconds for a connection that the cluster refuses.
func slowWarning(t *testing.T, successful int32) bool {
	t.Helper()
	var logs syncBuffer
	var e *testEnv
	var attempts atomic.Int32
	e = newTestEnv(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		c.Dial = func(context.Context, string) error {
			for name := range e.networkPolicies() {
				if strings.HasPrefix(name, "warren-canary-") {
					return nil
				}
			}
			if n := attempts.Add(1); n > 1 {
				e.clock.Advance(canaryPause)
				if n > successful {
					e.clock.Advance(3 * time.Second)
					return errTimeout
				}
			}
			e.clock.Advance(time.Millisecond)
			return nil
		}
	})
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod); pod.Labels[LabelCanary] == "true" {
			readyStatus(pod)
		}
		return false, nil, nil
	})
	e.m.enforcement.Store(enforcementUnknown)
	e.m.examineEnforcement(context.Background())
	if !e.m.Ready() {
		t.Fatalf("the manager is not ready: %s", e.m.NotReadyReason())
	}
	return strings.Contains(logs.String(), "needed some seconds")
}

// Warren sends the first connection attempt immediately after the delete of
// the policy. No network plugin is that fast, thus the attempt is usually
// successful. That is not a slow plugin.
func TestWarningForASlowNetworkPlugin(t *testing.T) {
	if slowWarning(t, 1) {
		t.Error("warren wrote the warning for a network plugin that refused the second attempt: the last successful connection was immediately after the delete")
	}
	if !slowWarning(t, 2) {
		t.Error("warren wrote no warning for a network plugin that permitted a connection 4 seconds after the delete")
	}
}

// A later test in which warren cannot connect to a permitted pod does not
// remove the result of the previous test (docs/isolation.md, section 4).
func TestLaterTestThatCannotConnectKeepsThePreviousResult(t *testing.T) {
	e, _ := canaryEnvWith(t, func() error { return errTimeout }, func(int32) error { return errTimeout })
	e.m.enforcement.Store(enforcementProven)
	e.m.examineEnforcement(context.Background())
	if !e.m.Ready() {
		t.Errorf("the manager is not ready: %s", e.m.NotReadyReason())
	}
}

// A node address that comes to the status of a pod after the first lease of
// the pod is not in the policy of the pod. The next reaper run adds the
// address, and it does not stop the sandbox (docs/isolation.md, section 3.3).
func TestOwnNodeAddressComesWithTheNextReaperRun(t *testing.T) {
	e := newTestEnv(t, nil)
	alice := userRef("alice")
	e.acquire(alice).Release()
	e.setStatus(alice.Ref.Name(), func(p *corev1.Pod) {
		p.Status.HostIP = "203.0.113.5"
		p.Status.HostIPs = []corev1.HostIP{{IP: "203.0.113.5"}}
	})
	waitFor(t, "node address in the cache", func() bool { return e.m.pod(alice.Ref.Name()).Status.HostIP != "" })
	has := func() bool {
		for _, s := range e.policyOf(alice).Spec.Egress[1].To[0].IPBlock.Except {
			if s == "203.0.113.5/32" {
				return true
			}
		}
		return false
	}
	if has() {
		t.Error("the policy has the node address before the reaper run")
	}
	uid := e.m.pod(alice.Ref.Name()).UID
	e.m.Reap(context.Background())
	if !has() {
		t.Error("the policy does not have the node address after the reaper run")
	}
	if p := e.m.pod(alice.Ref.Name()); p == nil || p.UID != uid {
		t.Error("the change stopped the sandbox")
	}
}

// enforcingCluster makes a cluster whose network plugin applies network
// policies correctly, with the ingress rules of the NetworkPolicy API:
//
//   - A pod that no policy with the type Ingress selects is not isolated: each
//     connection to it is successful.
//   - A pod that such a policy selects accepts a connection only if one of
//     these policies has an ingress rule. The cluster does not compare the
//     peers of a rule.
//
// Each write that changes a network policy gives it a new resource version,
// as an API server does: refer to strictNetworkPolicies. afterDial runs
// after the cluster decided on a connection attempt. ownPolicy tells it
// whether the test pod had a policy of its own at that moment: it has none
// in the second step of the test.
func enforcingCluster(t *testing.T, afterDial func(e *testEnv, ownPolicy bool)) *testEnv {
	t.Helper()
	var e *testEnv
	var pods atomic.Int32
	podsResource := corev1.SchemeGroupVersion.WithResource("pods")
	policiesResource := networkingv1.SchemeGroupVersion.WithResource("networkpolicies")
	// Read the objects from the store of the simulated cluster, not with a
	// list request: a test adds a reactor to the list requests of the pods.
	target := func(address string) *corev1.Pod {
		obj, err := e.client.Tracker().List(podsResource, corev1.SchemeGroupVersion.WithKind("Pod"), testNamespace)
		if err != nil {
			t.Fatal(err)
		}
		list := obj.(*corev1.PodList)
		for i := range list.Items {
			p := &list.Items[i]
			if p.Labels[LabelCanary] == "true" && strings.HasPrefix(address, p.Status.PodIP+":") && p.DeletionTimestamp == nil {
				return p
			}
		}
		return nil
	}
	selecting := func(pod *corev1.Pod) (isolated, permitted, own bool) {
		obj, err := e.client.Tracker().List(policiesResource, networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"), testNamespace)
		if err != nil {
			t.Fatal(err)
		}
		list := obj.(*networkingv1.NetworkPolicyList)
		for i := range list.Items {
			np := &list.Items[i]
			sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
			if err != nil || !sel.Matches(k8slabels.Set(pod.Labels)) || !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) {
				continue
			}
			isolated = true
			permitted = permitted || len(np.Spec.Ingress) > 0
			own = own || np.Name == pod.Name
		}
		return isolated, permitted, own
	}
	dial := func(_ context.Context, address string) error {
		pod := target(address)
		if pod == nil {
			return errNoRoute
		}
		isolated, permitted, own := selecting(pod)
		if afterDial != nil {
			afterDial(e, own)
		}
		if isolated && !permitted {
			return errTimeout
		}
		return nil
	}
	e = newTestEnv(t, func(c *Config) { c.Dial = dial })
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if pod.Labels[LabelCanary] == "true" {
			readyStatus(pod)
			pod.Status.PodIP = fmt.Sprintf("10.9.0.%d", pods.Add(1))
		}
		return false, nil, nil
	})
	// The test deletes its objects with their UID.
	e.strictDeletes(podsResource)
	e.strictDeletes(policiesResource)
	e.m.enforcement.Store(enforcementUnknown)
	return e
}

// The second step of the test shows a cluster that ignores network policies
// only while the policy of the namespace applies to the test pod. A person
// can delete or change that policy during the step. The network plugin is
// correct, thus warren must not record "the cluster does not apply network
// policies", and the proof of the previous test must stay.
func TestNamespacePolicyThatChangesDuringTheNetworkTest(t *testing.T) {
	ctx := context.Background()
	remove := func(e *testEnv) {
		if err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Delete(ctx, defaultDenyName, metav1.DeleteOptions{}); err != nil {
			t.Error(err)
		}
	}
	// permit adds a rule that permits each connection to each pod.
	permit := func(e *testEnv) {
		policies := e.client.NetworkingV1().NetworkPolicies(testNamespace)
		np, err := policies.Get(ctx, defaultDenyName, metav1.GetOptions{})
		if err != nil {
			t.Error(err)
			return
		}
		np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{}}
		if _, err := policies.Update(ctx, np, metav1.UpdateOptions{}); err != nil {
			t.Error(err)
		}
	}
	// The reaper makes or repairs the policy of the namespace.
	reap := func(e *testEnv) { e.m.Reap(ctx) }
	cases := []struct {
		name string
		// first runs after the first connection attempt of the second step.
		// last runs after the attempt that gives the result of the step.
		first, last func(e *testEnv)
	}{
		{"deleted", remove, nil},
		{"deleted, and made again before the end of the step", remove, reap},
		{"changed", permit, nil},
		{"changed, and repaired before the end of the step", permit, reap},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var armed atomic.Bool
			var attempts atomic.Int32
			e := enforcingCluster(t, func(e *testEnv, ownPolicy bool) {
				if !armed.Load() || ownPolicy {
					return
				}
				switch n := attempts.Add(1); {
				case n == 1:
					c.first(e)
				case n == canaryAttempts && c.last != nil:
					c.last(e)
				}
			})
			var logs syncBuffer
			e.m.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))

			// The first test: the simulated network plugin is correct.
			e.m.examineEnforcement(ctx)
			if !e.m.Ready() || e.m.enforcement.Load() != enforcementProven {
				t.Fatalf("the simulated cluster applies network policies, but warren is not ready: %q", e.m.NotReadyReason())
			}

			// The test of the next hour.
			armed.Store(true)
			e.m.examineEnforcement(ctx)
			armed.Store(false)
			if got := attempts.Load(); got != canaryAttempts {
				t.Fatalf("connection attempts in the second step = %d, want %d", got, canaryAttempts)
			}
			if got := e.m.enforcement.Load(); got != enforcementProven {
				t.Errorf("result = %d, want %d: the network plugin is correct, and the policy of the namespace did not apply during the second step", got, enforcementProven)
			}
			if strings.Contains(logs.String(), "THE CLUSTER DOES NOT APPLY NETWORK POLICIES") {
				t.Error("warren wrote the audit record that tells the administrator to install a network plugin")
			}
			if problem, _ := e.m.testProblem.Load().(string); !strings.Contains(problem, defaultDenyName) {
				t.Errorf("problem of the test = %q, want the policy of the namespace as the cause", problem)
			}
			// The next run of the reaper makes sure that the policy is correct.
			e.m.Reap(ctx)
			if np, ok := e.networkPolicies()[defaultDenyName]; !ok || len(np.Spec.Ingress) != 0 || !e.m.networkReady.Load() {
				t.Fatal("the reaper did not make or repair the policy of the namespace")
			}
			if reason := e.m.NotReadyReason(); reason != "" {
				t.Errorf("not ready after the policy of the namespace is correct again: %q", reason)
			}
			// The next complete test shows again that the plugin is correct.
			e.m.examineEnforcement(ctx)
			if !e.m.Ready() {
				t.Errorf("not ready after the next test: %q", e.m.NotReadyReason())
			}
		})
	}
}

// The same condition when the reaper cannot make the policy again, because
// the namespace has a pod of a different owner: the reaper records in the
// same seconds that the policy is absent. The test in progress must not
// record a result about the network plugin. After the administrator removed
// the pod and the policy is back, the readiness report must not name the
// network plugin.
func TestNamespacePolicyThatCannotComeBackDuringTheNetworkTest(t *testing.T) {
	ctx := context.Background()
	var armed, deleted, other atomic.Bool
	e := enforcingCluster(t, func(e *testEnv, ownPolicy bool) {
		if !armed.Load() || ownPolicy || !deleted.CompareAndSwap(false, true) {
			return
		}
		if err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Delete(ctx, defaultDenyName, metav1.DeleteOptions{}); err != nil {
			t.Error(err)
		}
		// The run of the reaper in the same seconds: it finds the pod of the
		// different owner and does not make the policy.
		other.Store(true)
		e.m.Reap(ctx)
		if e.m.networkReady.Load() {
			t.Error("the reaper made the policy in a namespace with a pod of a different owner")
		}
	})
	// The pod of the different owner is only in the list that warren asks
	// for, as in TestNamespacePolicyThatWarrenCannotMakeAgain.
	e.client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !other.Load() {
			return false, nil, nil
		}
		return true, &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "database-0", Namespace: testNamespace}}}}, nil
	})

	e.m.examineEnforcement(ctx)
	if !e.m.Ready() {
		t.Fatalf("the simulated cluster applies network policies, but warren is not ready: %q", e.m.NotReadyReason())
	}
	armed.Store(true)
	e.m.examineEnforcement(ctx)
	if !deleted.Load() {
		t.Fatal("the test did not get to its second step")
	}
	armed.Store(false)
	if reason := e.m.NotReadyReason(); !strings.Contains(reason, "database-0") {
		t.Errorf("reason = %q, want the pod of the different owner", reason)
	}
	if got := e.m.enforcement.Load(); got != enforcementProven {
		t.Errorf("result = %d, want %d: the reaper knows that the policy of the namespace is absent", got, enforcementProven)
	}

	// The administrator removes the pod. The reaper makes the policy.
	other.Store(false)
	e.m.Reap(ctx)
	if _, ok := e.networkPolicies()[defaultDenyName]; !ok || !e.m.networkReady.Load() {
		t.Fatal("the reaper did not make the policy of the namespace again")
	}
	if reason := e.m.NotReadyReason(); reason != "" {
		t.Errorf("not ready after the policy of the namespace is back: %q", reason)
	}
}

// meshProxy is a transparent proxy on the pod of warren, for example the
// sidecar of a service mesh. It accepts each TCP connection of warren on the
// pod itself. Only then does it connect to the destination.
//
// While reachable gives true, the proxy sends the request to the test pod,
// and warren gets the answer of the HTTP server of the test pod. If not, the
// network plugin drops the packets of the proxy. With ownAnswer, the proxy
// then sends an HTTP answer of its own. Without it, the proxy resets the
// connection of warren and sends no byte.
type meshProxy struct {
	listener  net.Listener
	reachable func() bool
	ownAnswer bool
	// forwarded is the number of connections that got to the test pod.
	forwarded atomic.Int32
}

func (p *meshProxy) serve() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			answer := "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
			switch {
			case p.reachable():
				p.forwarded.Add(1)
				// The answer of "python3 -m http.server" in the test pod.
				answer = "HTTP/1.0 200 OK\r\nServer: SimpleHTTP/0.6 Python/3.12.3\r\nContent-type: text/html; charset=utf-8\r\nContent-Length: 0\r\n\r\n"
			case !p.ownAnswer:
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0)
				}
				return
			}
			// A client that only opens and closes the connection sends no
			// request.
			if _, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
				_, _ = io.WriteString(conn, answer)
			}
		}()
	}
}

// A transparent proxy on the pod of warren accepts each TCP connection. The
// cluster applies network policies: without the policy of the test pod, no
// connection gets to the test pod. An open TCP connection is thus not a
// connection to the test pod, and an HTTP answer of the proxy is not the
// answer of the test pod. The result of the test must be "the cluster
// applies network policies".
func TestNetworkTestThroughATransparentProxy(t *testing.T) {
	ctx := context.Background()
	for name, ownAnswer := range map[string]bool{"the proxy resets the connection": false, "the proxy sends an answer of its own": true} {
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			var e *testEnv
			proxy := &meshProxy{listener: listener, ownAnswer: ownAnswer, reachable: func() bool {
				// The policy of the test pod permits warren. The policy of
				// the namespace refuses each connection.
				list, err := e.client.NetworkingV1().NetworkPolicies(testNamespace).List(ctx, metav1.ListOptions{})
				if err != nil {
					return false
				}
				for _, np := range list.Items {
					if strings.HasPrefix(np.Name, canaryPrefix) {
						return true
					}
				}
				return false
			}}
			// The environment does not replace Dial: warren uses the function
			// that it uses on a cluster.
			e = newTestEnv(t, func(c *Config) {
				// readyStatus gives the test pod the address 127.0.0.1.
				c.Port = listener.Addr().(*net.TCPAddr).Port
			})
			go proxy.serve()
			e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod); pod.Labels[LabelCanary] == "true" {
					readyStatus(pod)
				}
				return false, nil, nil
			})
			e.strictDeletes(corev1.SchemeGroupVersion.WithResource("pods"))
			e.strictDeletes(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"))
			e.m.enforcement.Store(enforcementUnknown)

			if err := e.m.checkEnforcement(ctx); err != nil {
				t.Errorf("checkEnforcement = %v on a cluster that applies network policies; %d connections got to the test pod", err, proxy.forwarded.Load())
			}
			// The first step and the last step each need one answer of the
			// test pod.
			if got := proxy.forwarded.Load(); got != 2 {
				t.Errorf("connections that got to the test pod = %d, want 2", got)
			}
			e.m.examineEnforcement(ctx)
			if !e.m.Ready() {
				t.Errorf("not ready: %q", e.m.NotReadyReason())
			}
		})
	}
}

// A connection attempt of the test is successful only when the HTTP server
// of the test pod answers. A different program can accept the connection: a
// proxy, or a pod that got the address of the test pod.
func TestConnectionAttemptNeedsTheAnswerOfTheTestPod(t *testing.T) {
	e := newTestEnv(t, nil)
	cases := []struct {
		name string
		// answer is the data that the server sends after the request. An
		// empty text means that the server closes the connection.
		answer string
		ok     bool
	}{
		{"the HTTP server of the test pod", "HTTP/1.0 200 OK\r\nServer: SimpleHTTP/0.6 Python/3.12.3\r\nContent-Length: 0\r\n\r\n", true},
		{"a server that sends no answer", "", false},
		{"a server with a different protocol", "SSH-2.0-OpenSSH_9.6\r\n", false},
		{"a proxy that cannot connect to the test pod", "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n", false},
	}
	for _, c := range cases {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
					_, _ = io.WriteString(conn, c.answer)
				}
				_ = conn.Close()
			}
		}()
		err = e.m.cfg.Dial(context.Background(), listener.Addr().String())
		if (err == nil) != c.ok {
			t.Errorf("%s: connection attempt = %v, want successful = %v", c.name, err, c.ok)
		}
		_ = listener.Close()
		// No program listens on the address now.
		if err := e.m.cfg.Dial(context.Background(), listener.Addr().String()); err == nil {
			t.Errorf("%s: connection attempt to a closed port was successful", c.name)
		}
	}
}
