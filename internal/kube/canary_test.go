package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/coderpunch/warren/internal/backend"
	"github.com/coderpunch/warren/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// canaryEnv makes an environment in which the test pod of the network test
// becomes ready immediately, as on a cluster.
//
// While the test pod has its own network policy, a connection attempt of
// warren is successful: the policy permits warren. After warren deleted that
// policy, dial gives the result of each connection attempt: an error on a
// cluster that applies network policies.
func canaryEnv(t *testing.T, dial func(attempt int32) error) (*testEnv, *atomic.Int32) {
	return canaryEnvWith(t, func() error { return nil }, dial)
}

// canaryEnvWith is canaryEnv with a result for the first step.
func canaryEnvWith(t *testing.T, permitted func() error, dial func(attempt int32) error) (*testEnv, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	var e *testEnv
	e = newTestEnv(t, func(c *Config) {
		c.Dial = func(context.Context, string) error {
			for name := range e.networkPolicies() {
				if strings.HasPrefix(name, "warren-canary-") {
					return permitted()
				}
			}
			return dial(attempts.Add(1))
		}
	})
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if pod.Labels[LabelCanary] == "true" {
			readyStatus(pod)
		}
		return false, nil, nil
	})
	// The test deletes its objects with their UID.
	e.strictDeletes(corev1.SchemeGroupVersion.WithResource("pods"))
	e.strictDeletes(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"))
	// newTestEnv records a successful test. Go back to "no test yet".
	e.m.enforcement.Store(enforcementUnknown)
	return e, &attempts
}

func (e *testEnv) canaries() []corev1.Pod {
	var out []corev1.Pod
	for _, p := range e.pods() {
		if p.Labels[LabelCanary] == "true" {
			out = append(out, p)
		}
	}
	return out
}

var errTimeout = errors.New("i/o timeout")

// On a cluster that applies network policies, warren cannot connect to a
// pod that has no policy of its own.
func TestClusterThatAppliesNetworkPolicies(t *testing.T) {
	e, attempts := canaryEnv(t, func(int32) error { return errTimeout })
	if e.m.Ready() {
		t.Fatal("the manager is ready before the test of the network policies")
	}
	var made *corev1.Pod
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		made = a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		return false, nil, nil
	})
	e.m.examineEnforcement(context.Background())
	if !e.m.Ready() || attempts.Load() != canaryRefusals {
		t.Errorf("ready = %v after %d connection attempts, want ready after %d", e.m.Ready(), attempts.Load(), canaryRefusals)
	}
	if n := len(e.canaries()); n != 0 {
		t.Errorf("test pods after the test = %d, want 0", n)
	}
	if made == nil {
		t.Fatal("warren made no test pod")
	}
	// The test pod must not be a sandbox for the cache, the limits, and the
	// reaper, and it must have no network policy of its own.
	if made.Labels[LabelManagedBy] != "" || made.Labels[LabelCanary] != "true" {
		t.Errorf("labels of the test pod = %v", made.Labels)
	}
	if got := e.networkPolicies(); len(got) != 1 {
		t.Errorf("network policies = %d, want only the policy of the namespace", len(got))
	}
	// It has the security settings of a sandbox: the sandbox namespace
	// accepts no other pod.
	sc, cs := made.Spec.SecurityContext, made.Spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.SeccompProfile == nil || cs == nil ||
		cs.AllowPrivilegeEscalation == nil || *cs.AllowPrivilegeEscalation || cs.Capabilities == nil || len(cs.Capabilities.Drop) != 1 {
		t.Errorf("security settings of the test pod: %+v, %+v", sc, cs)
	}
	if made.Spec.AutomountServiceAccountToken == nil || *made.Spec.AutomountServiceAccountToken || made.Spec.ActiveDeadlineSeconds == nil {
		t.Errorf("the test pod has a service account token, or it has no time limit")
	}
	e.acquire(userRef("alice")).Release()
}

// On a cluster that ignores network policies, the connection is successful.
// Warren then starts no sandbox.
func TestClusterThatIgnoresNetworkPolicies(t *testing.T) {
	e, attempts := canaryEnv(t, func(int32) error { return nil })
	e.m.examineEnforcement(context.Background())
	if e.m.Ready() {
		t.Fatal("the manager is ready on a cluster that ignores network policies")
	}
	if got := attempts.Load(); got != canaryAttempts {
		t.Errorf("connection attempts = %d, want %d: a network plugin can be slow", got, canaryAttempts)
	}
	if _, err := e.m.Acquire(context.Background(), userRef("alice")); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Acquire = %v, want ErrNotReady", err)
	}
	if err := e.m.Start(context.Background(), userRef("alice")); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Start = %v, want ErrNotReady", err)
	}
	for _, p := range e.pods() {
		if p.Labels[LabelManagedBy] == ManagedByValue {
			t.Errorf("warren started the sandbox %s", p.Name)
		}
	}
	if n := len(e.canaries()); n != 0 {
		t.Errorf("test pods after the test = %d, want 0", n)
	}
	// The administrator corrects the network plugin. The next test lets
	// sandboxes start.
	e2, _ := canaryEnv(t, func(int32) error { return errTimeout })
	e2.m.enforcement.Store(enforcementAbsent)
	e2.m.examineEnforcement(context.Background())
	if !e2.m.Ready() {
		t.Error("the manager is not ready after a successful test")
	}
}

// Some network plugins apply a policy to a new pod after some seconds.
func TestNetworkPluginThatIsSlow(t *testing.T) {
	e, attempts := canaryEnv(t, func(n int32) error {
		if n < 3 {
			return nil
		}
		return errTimeout
	})
	e.m.examineEnforcement(context.Background())
	// Two successful attempts, then the refused attempts of the proof.
	if want := int32(2 + canaryRefusals); !e.m.Ready() || attempts.Load() != want {
		t.Errorf("ready = %v after %d attempts, want ready after %d", e.m.Ready(), attempts.Load(), want)
	}
}

// If the test is not possible, the result of the previous test stays. With
// no previous test, warren starts no sandbox.
func TestNetworkTestThatIsNotPossible(t *testing.T) {
	e := newTestEnv(t, func(c *Config) {
		c.Dial = func(context.Context, string) error { return errTimeout }
	})
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Labels[LabelCanary] == "true" {
			return true, nil, errors.New("quota exceeded")
		}
		return false, nil, nil
	})
	e.m.enforcement.Store(enforcementUnknown)
	e.m.examineEnforcement(context.Background())
	if e.m.Ready() {
		t.Error("the manager is ready with no result of the network test")
	}
	e.m.enforcement.Store(enforcementProven)
	e.m.examineEnforcement(context.Background())
	if !e.m.Ready() {
		t.Error("a test that was not possible removed the result of the previous test")
	}
}

func TestNetworkTestRemovesOldTestPods(t *testing.T) {
	e, _ := canaryEnv(t, func(int32) error { return errTimeout })
	old := e.m.buildCanary("warren-canary-old")
	if _, err := e.client.CoreV1().Pods(testNamespace).Create(context.Background(), old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// A younger test pod can be the pod of a test in progress.
	e.clock.Advance(canaryMaxTime + time.Second)
	sandbox := e.m.buildPod("wr-sandbox", "wr-sandbox-00000000", userRef("alice"), e.policy(userRef("alice")))
	if _, err := e.client.CoreV1().Pods(testNamespace).Create(context.Background(), sandbox, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.m.examineEnforcement(context.Background())
	if n := len(e.canaries()); n != 0 {
		t.Errorf("test pods = %d, want 0", n)
	}
	left := e.pods()
	if len(left) != 1 || left[0].Name != "wr-sandbox" {
		t.Errorf("pods after the test = %d, want only the sandbox", len(left))
	}
}

func TestNetworkTestCanBeTurnedOff(t *testing.T) {
	e := newTestEnv(t, func(c *Config) {
		c.Network.SkipEnforcementCheck = true
		c.Dial = func(context.Context, string) error { t.Error("warren did the test"); return nil }
	})
	e.m.enforcement.Store(enforcementUnknown)
	e.m.examineEnforcement(context.Background())
	if !e.m.Ready() || len(e.pods()) != 0 {
		t.Errorf("ready = %v, pods = %d", e.m.Ready(), len(e.pods()))
	}
}

// A pod that is ready for Kubernetes, and that warren cannot connect to,
// must not stay with no end. The usual cause is a network policy that does
// not permit warren.
func TestReadyPodThatWarrenCannotUse(t *testing.T) {
	e := newTestEnv(t, func(c *Config) {
		c.Verify = func(context.Context, backend.Target) error { return errTimeout }
	})
	req := userRef("alice")
	name := req.Ref.Name()
	try := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		res := e.acquireAsync(ctx, e.m, req)
		waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
		if !podReady(e.m.pod(name)) {
			e.makeReady(name)
		}
		return (<-res).err
	}
	if err := try(); !errors.Is(err, backend.ErrNotReady) {
		t.Fatalf("first request: %v, want ErrNotReady", err)
	}
	e.clock.Advance(30 * time.Second)
	if err := try(); !errors.Is(err, backend.ErrNotReady) || e.m.pod(name) == nil {
		t.Fatalf("request after 30 seconds: %v, pod present = %v", err, e.m.pod(name) != nil)
	}
	uid := e.m.pod(name).UID
	e.clock.Advance(40 * time.Second)
	if err := try(); err == nil || errors.Is(err, backend.ErrNotReady) {
		t.Errorf("request after 70 seconds: %v, want an error that names the cause", err)
	}
	waitFor(t, "stop of the pod that warren cannot use", func() bool { p := e.m.pod(name); return p == nil || p.UID != uid })
}

// A network policy of a different owner can permit what warren refuses.
func TestNetworkPolicyThatWarrenDidNotMake(t *testing.T) {
	e := newTestEnv(t, nil)
	e.acquire(userRef("alice")).Release()
	other := e.m.buildDefaultDeny()
	other.Name, other.Labels = "allow-same-namespace", nil
	other.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}}}
	// A policy of a different owner that has no rule permits nothing. It
	// does not stop the start of sandboxes.
	harmless := e.m.buildDefaultDeny()
	harmless.Name, harmless.Labels = "deny-all-of-the-platform", nil
	if _, err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Create(context.Background(), harmless, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.m.Reap(context.Background())
	e.acquire(userRef("carol")).Release()
	policies := e.client.NetworkingV1().NetworkPolicies(testNamespace)
	if _, err := policies.Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.m.Reap(context.Background())
	limited, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.m.Acquire(limited, userRef("bob")); err == nil || errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Acquire in a namespace with a network policy of a different owner: %v, want an error that names the cause", err)
	}
	// Warren does not delete or change that policy.
	if _, ok := e.networkPolicies()["allow-same-namespace"]; !ok {
		t.Error("warren deleted a network policy that it did not make")
	}
	// A sandbox that operates stays in operation.
	e.acquire(userRef("alice")).Release()
	if err := policies.Delete(context.Background(), "allow-same-namespace", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	e.m.Reap(context.Background())
	e.acquire(userRef("bob")).Release()

	// An administrator can accept a policy by its name. A policy with a
	// different name still stops the start of sandboxes.
	e2 := newTestEnv(t, func(c *Config) { c.Network.AcceptedPolicies = []string{"allow-same-namespace"} })
	if _, err := e2.client.NetworkingV1().NetworkPolicies(testNamespace).Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e2.m.Reap(context.Background())
	e2.acquire(userRef("alice")).Release()
	other.Name = "a-second-policy"
	if _, err := e2.client.NetworkingV1().NetworkPolicies(testNamespace).Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e2.m.Reap(context.Background())
	if _, err := e2.m.Acquire(limited, userRef("bob")); err == nil || errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Acquire with a network policy that the configuration does not accept: %v, want an error that names the cause", err)
	}
}

// Before the first run of the reaper, warren does not know the network
// policies of the namespace. It must not start a sandbox in that time.
func TestNoSandboxBeforeWarrenExaminedThePolicies(t *testing.T) {
	e := newTestEnv(t, nil)
	e.m.otherPolicies.Store(-1)
	if e.m.Ready() {
		t.Error("the manager is ready before it examined the network policies")
	}
	if _, err := e.m.Acquire(context.Background(), userRef("alice")); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Acquire = %v, want ErrNotReady", err)
	}
	e.m.Reap(context.Background())
	if !e.m.Ready() {
		t.Errorf("the manager is not ready after the reaper run: %s", e.m.NotReadyReason())
	}
}

// If warren cannot connect to a pod that its policy permits, the pod labels
// or the namespace in the configuration of warren are wrong. Warren must say
// so, and it must not start sandboxes that no request can get to.
func TestWarrenCannotConnectToAPermittedPod(t *testing.T) {
	e, attempts := canaryEnvWith(t, func() error { return errTimeout }, func(int32) error { return errTimeout })
	e.m.examineEnforcement(context.Background())
	if e.m.Ready() {
		t.Fatal("the manager is ready, and warren cannot connect to a permitted pod")
	}
	if attempts.Load() != 0 {
		t.Error("the second step of the test ran after the first step failed")
	}
	if reason := e.m.NotReadyReason(); !strings.Contains(reason, "permits") {
		t.Errorf("reason = %q, want the cause", reason)
	}
	if n := len(e.canaries()); n != 0 {
		t.Errorf("test pods after the test = %d, want 0", n)
	}
	if got := e.networkPolicies(); len(got) != 1 {
		t.Errorf("network policies after the test = %d, want only the policy of the namespace", len(got))
	}
}

// Warren makes the policy that stops all connections of a namespace only if
// the namespace has no pods of a different owner.
func TestNamespaceWithPodsOfADifferentOwner(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "database-0", Namespace: testNamespace}})
	cfg := func() Config {
		return Config{
			Client: client, Namespace: testNamespace, Image: "example.test/terminal:1",
			Policies: policy.NewStore(testPolicies(t)), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Network: NetworkConfig{WarrenNamespace: "warren-system", WarrenPodLabels: map[string]string{"app": "warren"}},
		}
	}
	m, err := New(cfg())
	if err != nil {
		t.Fatal(err)
	}
	err = m.ensureDefaultDeny(context.Background())
	if err == nil || !strings.Contains(err.Error(), "database-0") {
		t.Fatalf("ensureDefaultDeny = %v, want an error that names the pod", err)
	}
	list, _ := client.NetworkingV1().NetworkPolicies(testNamespace).List(context.Background(), metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Error("warren made the policy in a namespace with a pod of a different owner")
	}
	// All other conditions are good: only the policy of the namespace
	// decides.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.factory.Start(ctx.Done())
	waitFor(t, "caches", m.Synced)
	m.enforcement.Store(enforcementProven)
	m.otherPolicies.Store(0)
	if m.Ready() || !strings.Contains(m.NotReadyReason(), "database-0") {
		t.Errorf("ready = %v, reason = %q with no policy for the namespace", m.Ready(), m.NotReadyReason())
	}
	// An administrator can accept it.
	c := cfg()
	c.Network.AllowOtherPods = true
	m2, _ := New(c)
	if err := m2.ensureDefaultDeny(context.Background()); err != nil {
		t.Errorf("with the permission of the administrator: %v", err)
	}
}

// A container that cannot start gets one log record with the cause, and
// warren stops the pod after a short time. The usual cause is an image that
// does not agree with the command of the policy.
func TestContainerThatCannotStart(t *testing.T) {
	var logs syncBuffer
	e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
	req := templateRef("other-image", "alice")
	name := req.Ref.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _ = e.m.Acquire(ctx, req)
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	e.setStatus(name, func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerName, State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "RunContainerError", Message: `exec: "/sbin/tini": no such file or directory`},
		}}}
	})
	waitFor(t, "status in the cache", func() bool { return len(e.m.pod(name).Status.ContainerStatuses) == 1 })
	e.m.Reap(context.Background())
	e.m.Reap(context.Background())
	if n := strings.Count(logs.String(), "cannot start"); n != 1 {
		t.Errorf("log records = %d, want 1: %s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "/sbin/tini") || !strings.Contains(logs.String(), "other-image") {
		t.Errorf("the log record does not have the cause and the policy: %s", logs.String())
	}
	if e.m.pod(name) == nil {
		t.Fatal("warren stopped the pod immediately")
	}
	e.clock.Advance(3 * time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the pod that cannot start", func() bool { return e.m.pod(name) == nil })
}

// The policy of each sandbox does not permit the addresses of the nodes that
// warren found, also when a node has a public address.
func TestNodeAddressesAreBlocked(t *testing.T) {
	e := newTestEnv(t, nil)
	alice := userRef("alice")
	e.acquire(alice).Release()
	e.setStatus(alice.Ref.Name(), func(p *corev1.Pod) {
		p.Status.HostIP = "203.0.113.5"
		p.Status.HostIPs = []corev1.HostIP{{IP: "203.0.113.5"}, {IP: "2001:db8::5"}}
	})
	waitFor(t, "node address in the cache", func() bool { return e.m.pod(alice.Ref.Name()).Status.HostIP != "" })
	e.m.Reap(context.Background())
	np := e.policyOf(alice)
	v4, v6 := np.Spec.Egress[1].To[0].IPBlock.Except, np.Spec.Egress[1].To[1].IPBlock.Except
	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(v4, "203.0.113.5/32") || !has(v6, "2001:db8::5/128") || !has(v6, "64:ff9b::cb00:7105/128") {
		t.Errorf("exceptions after the reaper run: %v and %v", v4, v6)
	}
	// A sandbox that starts later has the address from its start.
	e.acquire(userRef("bob")).Release()
	if got := e.policyOf(userRef("bob")).Spec.Egress[1].To[0].IPBlock.Except; !has(got, "203.0.113.5/32") {
		t.Errorf("exceptions of a new sandbox: %v", got)
	}
}

// If the test pod cannot start, the error must give the cause of Kubernetes
// and the facts that an administrator needs.
func TestNetworkTestPodThatCannotPullItsImage(t *testing.T) {
	e := newTestEnv(t, nil)
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if pod.Labels[LabelCanary] == "true" {
			pod.Status.Phase = corev1.PodPending
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerName, State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull access denied"},
			}}}
		}
		return false, nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := e.m.checkEnforcement(ctx)
	if err == nil {
		t.Fatal("checkEnforcement gave no error")
	}
	for _, want := range []string{"ImagePullBackOff", "pull access denied", "--sandbox-image", "imagePullSecrets", "/usr/local/bin/python3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q in it", err, want)
		}
	}
	if strings.Contains(err.Error(), "default command") {
		t.Errorf("error = %q: the test pod does not use the default command of a sandbox", err)
	}
	if n := len(e.canaries()); n != 0 {
		t.Errorf("test pods after the test = %d, want 0", n)
	}
}

// After one good test, a later test in which warren cannot connect to the
// permitted pod does not stop the start of sandboxes: the result of the
// previous test stays (docs/isolation.md, section 4).
func TestLaterFailureOfTheFirstStepKeepsTheResult(t *testing.T) {
	var fail atomic.Bool
	e, _ := canaryEnvWith(t, func() error {
		if fail.Load() {
			return errTimeout
		}
		return nil
	}, func(int32) error { return errTimeout })
	e.m.examineEnforcement(context.Background())
	if !e.m.Ready() {
		t.Fatalf("not ready after a good test: %s", e.m.NotReadyReason())
	}
	fail.Store(true)
	e.m.examineEnforcement(context.Background())
	if !e.m.Ready() {
		t.Errorf("a later failure of the first step removed the result of the previous test: %s", e.m.NotReadyReason())
	}
}

// After one successful test, a later test in which warren cannot connect to
// the permitted pod does not change the result (docs/isolation.md, section 4).
func TestLaterNetworkTestKeepsThePreviousResult(t *testing.T) {
	e, _ := canaryEnvWith(t, func() error { return errTimeout }, func(int32) error { return errTimeout })
	e.m.enforcement.Store(enforcementProven)
	e.m.examineEnforcement(context.Background())
	if !e.m.Ready() {
		t.Errorf("the manager is not ready after a later test that failed in its first step: %s", e.m.NotReadyReason())
	}
}

// One failure of the key check, a long time before, is not "no connection
// for 60 seconds". One more failure must not stop the sandbox.
func TestOneOldFailureDoesNotStopASandbox(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	e := newTestEnv(t, func(c *Config) {
		c.Verify = func(context.Context, backend.Target) error {
			if fail.Load() {
				return errTimeout
			}
			return nil
		}
	})
	req := userRef("alice")
	name := req.Ref.Name()
	try := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		res := e.acquireAsync(ctx, e.m, req)
		waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
		if !podReady(e.m.pod(name)) {
			e.makeReady(name)
		}
		return (<-res).err
	}
	if err := try(); !errors.Is(err, backend.ErrNotReady) {
		t.Fatalf("first request: %v, want ErrNotReady", err)
	}
	uid := e.m.pod(name).UID
	// The client went away. Ten minutes later, one key check fails again.
	e.clock.Advance(10 * time.Minute)
	if err := try(); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("request after 10 minutes: %v, want ErrNotReady", err)
	}
	time.Sleep(50 * time.Millisecond)
	if p := e.m.pod(name); p == nil || p.UID != uid {
		t.Fatal("warren stopped the sandbox after two failures that are 10 minutes apart")
	}
	fail.Store(false)
	e.acquire(req).Release()
}

// A container that stops again and again, in a sandbox that operated, is not
// a container that cannot start. Warren stops the pod only when the container
// did not operate for the time limit.
func TestContainerThatStopsInASandboxThatOperated(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	e.clock.Advance(10 * time.Minute)
	e.setStatus(name, func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(e.clock.Now()),
		}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerName, RestartCount: 2, State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 10s restarting failed container"},
		}}}
	})
	waitFor(t, "status in the cache", func() bool { return len(e.m.pod(name).Status.ContainerStatuses) == 1 })
	e.clock.Advance(20 * time.Second)
	e.m.Reap(context.Background())
	time.Sleep(50 * time.Millisecond)
	if e.m.pod(name) == nil {
		t.Fatal("warren stopped a sandbox whose container stopped 20 seconds before")
	}
	// The container does not come back.
	e.clock.Advance(3 * time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the pod whose container does not start again", func() bool { return e.m.pod(name) == nil })
}

// The start request makes the network policy before Kubernetes selects a
// node. The first sandbox on a node must not get a lease while its policy
// permits the address of its own node.
func TestFirstSandboxOnANodeWithAPublicAddress(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, req)
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	e.setStatus(name, func(p *corev1.Pod) {
		readyStatus(p)
		p.Status.HostIP = "203.0.113.5"
		p.Status.HostIPs = []corev1.HostIP{{IP: "203.0.113.5"}, {IP: "2001:db8::5"}}
	})
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.lease.Release()
	np := e.policyOf(req)
	v4, v6 := np.Spec.Egress[1].To[0].IPBlock.Except, np.Spec.Egress[1].To[1].IPBlock.Except
	if !slices.Contains(v4, "203.0.113.5/32") || !slices.Contains(v6, "2001:db8::5/128") {
		t.Errorf("exceptions at the first lease: %v and %v, want the addresses of the node", v4, v6)
	}
}

// The test pod must not go to the nodes of one special policy when a policy
// with the same image can use all nodes. The order of the IDs decides only
// between two policies with a node selector of the same size.
func TestTestPodUsesThePolicyWithTheSmallestNodeSelector(t *testing.T) {
	e := newTestEnv(t, nil)
	e.usePolicies(`
policies:
  - id: data-team
    name: Data team
    scope: user
    imagePullSecrets: [data-pull]
    nodeSelector: {pool: data}
    tolerations: [{key: pool, operator: Equal, value: data, effect: NoSchedule}]
  - {id: general, name: General, scope: user, imagePullSecrets: [general-pull]}
`)
	pod := e.m.buildCanary("warren-canary-00000000")
	if len(pod.Spec.NodeSelector) != 0 || len(pod.Spec.Tolerations) != 0 {
		t.Errorf("node selector = %v, tolerations = %v: the policy %q has the same image and no node selector",
			pod.Spec.NodeSelector, pod.Spec.Tolerations, "general")
	}
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "general-pull" {
		t.Errorf("pull secrets = %v, want those of the policy %q", pod.Spec.ImagePullSecrets, "general")
	}
	// With two policies that have a node selector of the same size, the test
	// pod has all settings of the first one.
	e.usePolicies(`
policies:
  - {id: b-team, name: B, scope: user, nodeSelector: {pool: b}}
  - {id: a-team, name: A, scope: user, nodeSelector: {pool: a}, imagePullSecrets: [a-pull]}
`)
	pod = e.m.buildCanary("warren-canary-00000000")
	if pod.Spec.NodeSelector["pool"] != "a" || len(pod.Spec.ImagePullSecrets) != 1 {
		t.Errorf("node selector = %v, pull secrets = %v, want those of the policy %q", pod.Spec.NodeSelector, pod.Spec.ImagePullSecrets, "a-team")
	}
}

// An image that Kubernetes cannot pull for some time is not a container that
// cannot start. The 2 minutes of start_error count from the first sight of a
// cause that stops the pod, and the log has the message of that cause. A
// second cause that stops the pod does not start the time again.
func TestStartErrorAfterAFailedImagePull(t *testing.T) {
	var logs syncBuffer
	e := newTestEnv(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		// Only the start_error rule can stop the pod in this test.
		c.StartTimeout = time.Hour
	})
	req := templateRef("other-image", "alice")
	name := req.Ref.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _ = e.m.Acquire(ctx, req)
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	waiting := func(reason, message string) {
		e.setStatus(name, func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerName, State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message},
			}}}
		})
		waitFor(t, "status in the cache", func() bool {
			s := e.m.pod(name).Status.ContainerStatuses
			return len(s) == 1 && s[0].State.Waiting.Reason == reason
		})
	}
	// The registry does not answer for 3 minutes.
	waiting("ImagePullBackOff", "registry timeout")
	e.m.Reap(context.Background())
	e.clock.Advance(3 * time.Minute)
	e.m.Reap(context.Background())
	if n := e.podDeletes(name); n != 0 {
		t.Fatalf("warren stopped a pod that waits for its image: delete requests = %d", n)
	}
	// The image comes, and the container has its first start error.
	waiting("RunContainerError", `exec: "/sbin/tini": no such file or directory`)
	e.m.Reap(context.Background())
	if n := e.podDeletes(name); n != 0 {
		t.Errorf("delete requests at the first sight of the start error = %d, want 0: the condition must stay for 2 minutes", n)
	}
	if !strings.Contains(logs.String(), "/sbin/tini") {
		t.Errorf("the log does not have the message of the cause that stops the pod: %s", logs.String())
	}
	// Kubernetes shows the two causes in turn. Each cause has one log record.
	waiting("CrashLoopBackOff", "back-off 10s restarting failed container")
	e.clock.Advance(time.Minute)
	e.m.Reap(context.Background())
	waiting("RunContainerError", `exec: "/sbin/tini": no such file or directory`)
	e.m.Reap(context.Background())
	if n := strings.Count(logs.String(), "cannot start"); n != 3 {
		t.Errorf("log records = %d, want 3 (one for each cause): %s", n, logs.String())
	}
	if n := e.podDeletes(name); n != 0 {
		t.Errorf("delete requests 1 minute after the first start error = %d, want 0", n)
	}
	// The next run of the reaper is 2 minutes 30 seconds after the first cause
	// that stops the pod. It is 1 minute 30 seconds after the second cause.
	e.clock.Advance(90 * time.Second)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the pod that cannot start", func() bool { return e.m.pod(name) == nil })
	if !strings.Contains(logs.String(), "sandbox="+name+" reason=start_error ") {
		t.Errorf("the reason of the stop is not start_error: %s", logs.String())
	}
}

// strictDeletes makes the simulated cluster apply the UID condition of a
// delete request, as an API server does. Without this rule, a delete request
// with the UID of a different object deletes the object.
func (e *testEnv) strictDeletes(resource schema.GroupVersionResource) {
	e.client.PrependReactor("delete", resource.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		del, ok := a.(k8stesting.DeleteActionImpl)
		if !ok || del.DeleteOptions.Preconditions == nil || del.DeleteOptions.Preconditions.UID == nil {
			return false, nil, nil
		}
		obj, err := e.client.Tracker().Get(resource, del.Namespace, del.Name)
		if err != nil {
			return false, nil, nil
		}
		if meta, ok := obj.(metav1.Object); ok && meta.GetUID() != *del.DeleteOptions.Preconditions.UID {
			return true, nil, apierrors.NewConflict(resource.GroupResource(), del.Name, errors.New("the UID of the request is not the UID of the object"))
		}
		return false, nil, nil
	})
}

// One connection attempt can fail for a cause that goes away: a lost packet,
// a busy node, a fault of warren. On a cluster that ignores network
// policies, the attempts after it are successful again. One failure, or two,
// must not be the proof that the cluster applies network policies.
func TestConnectionThatFailsOneTimeIsNotAProof(t *testing.T) {
	cases := []struct {
		// failed has the numbers of the attempts that fail in the second
		// step. attempts is the number of attempts that warren must make.
		failed   []int32
		attempts int32
	}{
		{[]int32{1}, canaryAttempts},
		{[]int32{2}, canaryAttempts},
		{[]int32{2, 3}, canaryAttempts},
		// Only failures one after the other count.
		{[]int32{1, 3, 5}, canaryAttempts},
		{[]int32{canaryAttempts}, canaryAttempts + 1},
		{[]int32{canaryAttempts, canaryAttempts + 1}, canaryAttempts + 2},
	}
	for _, c := range cases {
		e, attempts := canaryEnv(t, func(n int32) error {
			if slices.Contains(c.failed, n) {
				return errTimeout
			}
			return nil
		})
		e.m.examineEnforcement(context.Background())
		if e.m.Ready() {
			t.Errorf("failed attempts %v: the manager is ready on a cluster that ignores network policies", c.failed)
		}
		if got := e.m.enforcement.Load(); got != enforcementAbsent {
			t.Errorf("failed attempts %v: result = %d, want %d", c.failed, got, enforcementAbsent)
		}
		if got := attempts.Load(); got != c.attempts {
			t.Errorf("failed attempts %v: connection attempts = %d, want %d", c.failed, got, c.attempts)
		}
	}
	// A slow network plugin refuses the connection only at the last regular
	// attempt. Warren then makes the attempts that the proof needs.
	e, attempts := canaryEnv(t, func(n int32) error {
		if n < canaryAttempts {
			return nil
		}
		return errTimeout
	})
	e.m.examineEnforcement(context.Background())
	if want := int32(canaryAttempts + canaryRefusals - 1); !e.m.Ready() || attempts.Load() != want {
		t.Errorf("slow plugin: ready = %v after %d attempts, want ready after %d", e.m.Ready(), attempts.Load(), want)
	}
}

// An image that Kubernetes cannot pull at this time can come later, thus
// warren does not stop the pod for that cause. The time of that wait must
// not count for a later error of the container: the 2 minutes start at the
// first sight of a cause that stops the pod.
func TestContainerErrorAfterASlowImagePull(t *testing.T) {
	var logs syncBuffer
	e := newTestEnv(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		// Only the start_error rule can stop the pod in this test.
		c.StartTimeout = time.Hour
	})
	req := templateRef("other-image", "alice")
	name := req.Ref.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _ = e.m.Acquire(ctx, req)
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	waiting := func(reason, message string) {
		t.Helper()
		e.setStatus(name, func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerName, State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message},
			}}}
		})
		waitFor(t, "status in the cache", func() bool {
			st := e.m.pod(name).Status.ContainerStatuses
			return len(st) == 1 && st[0].State.Waiting.Reason == reason
		})
	}
	waiting("ImagePullBackOff", "back-off pulling image")
	e.m.Reap(context.Background())
	e.clock.Advance(startErrorAfter + time.Minute)
	e.m.Reap(context.Background())
	if e.m.pod(name) == nil || e.podDeletes(name) != 0 {
		t.Fatal("warren stopped a pod that waits for its image")
	}
	// The image came. The first start of the container gives one error.
	waiting("CreateContainerError", "context deadline exceeded")
	e.m.Reap(context.Background())
	if n := e.podDeletes(name); n != 0 {
		t.Fatalf("delete requests at the first sight of CreateContainerError = %d, want 0: the time of the image pull counted", n)
	}
	// The second cause has its own log record.
	if n := strings.Count(logs.String(), "cannot start"); n != 2 || !strings.Contains(logs.String(), "context deadline exceeded") {
		t.Errorf("log records = %d, want one for each of the two causes: %s", n, logs.String())
	}
	// The image pull fails again for a short time (imagePullPolicy: Always).
	// That does not start the time again.
	e.clock.Advance(startErrorAfter / 2)
	waiting("ErrImagePull", "connection refused")
	e.m.Reap(context.Background())
	waiting("CreateContainerError", "context deadline exceeded")
	e.m.Reap(context.Background())
	if n := e.podDeletes(name); n != 0 {
		t.Fatalf("delete requests after 1 minute of CreateContainerError = %d, want 0", n)
	}
	// The error stays for more than 2 minutes.
	e.clock.Advance(startErrorAfter/2 + time.Second)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the pod that cannot start", func() bool { return e.m.pod(name) == nil })
	// One record for each of the three causes, and no record for a cause
	// that comes again.
	if n := strings.Count(logs.String(), "cannot start"); n != 3 {
		t.Errorf("log records = %d, want 3: %s", n, logs.String())
	}
}

// A sandbox must get no lease while warren cannot put the address of the
// node in the network policy of the pod: Kubernetes refuses the read or the
// change of the policy. A program of the user could then connect to the
// node.
func TestNoLeaseWhileTheNodeAddressIsNotInThePolicy(t *testing.T) {
	for i, verb := range []string{"get", "update"} {
		e := newTestEnv(t, nil)
		req := userRef("user" + strconv.Itoa(i))
		name := req.Ref.Name()
		var refuse atomic.Bool
		refuse.Store(true)
		e.client.PrependReactor(verb, "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
			if refuse.Load() {
				return true, nil, errors.New("the server is not available")
			}
			return false, nil, nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		res := e.acquireAsync(ctx, e.m, req)
		waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
		e.setStatus(name, func(p *corev1.Pod) {
			readyStatus(p)
			p.Status.HostIP = "203.0.113.5"
		})
		r := <-res
		cancel()
		if r.err == nil {
			r.lease.Release()
			t.Fatalf("%s refused: lease for a sandbox whose policy permits the address of its node", verb)
		}
		if !errors.Is(r.err, backend.ErrNotReady) {
			t.Errorf("%s refused: Acquire = %v, want ErrNotReady", verb, r.err)
		}
		// Warren did not connect to the sandbox.
		e.mu.Lock()
		verified := len(e.verified)
		e.mu.Unlock()
		if verified != 0 {
			t.Errorf("%s refused: warren did %d key checks before the policy had the address of the node", verb, verified)
		}
		// Kubernetes accepts the request again.
		refuse.Store(false)
		e.acquire(req).Release()
		if v4 := e.policyOf(req).Spec.Egress[1].To[0].IPBlock.Except; !slices.Contains(v4, "203.0.113.5/32") {
			t.Errorf("%s permitted again: exceptions at the first lease = %v, want the address of the node", verb, v4)
		}
	}
}

// The results of the connection attempts count only for the test pod that
// warren made, while it operates. A different pod can have the name or the
// address of a test pod that went away, and a pod in deletion stops its
// server. The cleanup of the test deletes only the pod that warren made.
func TestNetworkTestWhoseTestPodChanged(t *testing.T) {
	ctx := context.Background()
	// A change returns the UID of a pod that warren did not make and that
	// has the name of the test pod. Without such a pod, it returns no UID.
	changes := map[string]func(e *testEnv, pod corev1.Pod) types.UID{
		"a new pod with the same name": func(e *testEnv, pod corev1.Pod) types.UID {
			pods := e.client.CoreV1().Pods(testNamespace)
			if err := pods.Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
				t.Error(err)
			}
			other, err := pods.Create(ctx, e.m.buildCanary(pod.Name), metav1.CreateOptions{})
			if err != nil {
				t.Error(err)
				return ""
			}
			if other.UID == "" || other.UID == pod.UID {
				t.Errorf("UID of the new pod = %q, UID of the test pod = %q, want two different UIDs", other.UID, pod.UID)
			}
			return other.UID
		},
		"a pod in deletion": func(e *testEnv, pod corev1.Pod) types.UID {
			now := metav1.NewTime(e.clock.Now())
			pod.DeletionTimestamp = &now
			if _, err := e.client.CoreV1().Pods(testNamespace).Update(ctx, &pod, metav1.UpdateOptions{}); err != nil {
				t.Error(err)
			}
			return ""
		},
		"a different address": func(e *testEnv, pod corev1.Pod) types.UID {
			e.setStatus(pod.Name, func(p *corev1.Pod) { p.Status.PodIP = "127.0.0.2" })
			return ""
		},
	}
	for what, change := range changes {
		var e *testEnv
		// want has the UIDs of the pods that must stay after the test.
		var want []types.UID
		e, _ = canaryEnv(t, func(n int32) error {
			if n == 1 {
				for _, p := range e.canaries() {
					if other := change(e, p); other != "" {
						want = append(want, other)
					}
				}
			}
			return errTimeout
		})
		e.m.examineEnforcement(ctx)
		if e.m.enforcement.Load() != enforcementUnknown || e.m.Ready() {
			t.Errorf("%s: result = %d, want no result: the refused connections were not connections to the test pod", what, e.m.enforcement.Load())
		}
		if reason := e.m.NotReadyReason(); !strings.Contains(reason, "test pod") {
			t.Errorf("%s: reason = %q, want the cause", what, reason)
		}
		// The delete request of the cleanup has the UID of the pod that
		// warren made. Thus a different pod with the same name stays.
		var left []types.UID
		for _, p := range e.canaries() {
			left = append(left, p.UID)
		}
		if !slices.Equal(left, want) {
			t.Errorf("%s: UIDs of the pods with the label of a test pod after the test = %v, want %v: the cleanup must delete the pod that warren made, and no other pod",
				what, left, want)
		}
	}
}

// On a cluster that applies network policies, a different pod can get the
// address of a test pod that went away, and its policy can permit warren.
// Successful connections then do not show a cluster that ignores network
// policies.
func TestSuccessfulConnectionsAfterTheTestPodWentAway(t *testing.T) {
	var e *testEnv
	e, attempts := canaryEnv(t, func(n int32) error {
		if n == 1 {
			for _, p := range e.canaries() {
				_ = e.client.CoreV1().Pods(testNamespace).Delete(context.Background(), p.Name, metav1.DeleteOptions{})
			}
		}
		return nil
	})
	e.m.examineEnforcement(context.Background())
	if got := e.m.enforcement.Load(); got != enforcementUnknown {
		t.Errorf("result = %d, want no result: the connections were not connections to the test pod", got)
	}
	if got := attempts.Load(); got != canaryAttempts {
		t.Errorf("connection attempts = %d, want %d", got, canaryAttempts)
	}
	if reason := e.m.NotReadyReason(); !strings.Contains(reason, "test pod") {
		t.Errorf("reason = %q, want the cause", reason)
	}
}

// If the test pod goes away in the first step, the failed connections do not
// show a fault of the configuration of warren.
func TestFirstStepWhoseTestPodWentAway(t *testing.T) {
	var e *testEnv
	e, attempts := canaryEnvWith(t, func() error {
		for _, p := range e.canaries() {
			_ = e.client.CoreV1().Pods(testNamespace).Delete(context.Background(), p.Name, metav1.DeleteOptions{})
		}
		return errTimeout
	}, func(int32) error { return errTimeout })
	e.m.examineEnforcement(context.Background())
	if e.m.Ready() || attempts.Load() != 0 {
		t.Errorf("ready = %v, attempts of the second step = %d", e.m.Ready(), attempts.Load())
	}
	reason := e.m.NotReadyReason()
	if !strings.Contains(reason, "test pod") || strings.Contains(reason, errUnreachable.Error()) {
		t.Errorf("reason = %q, want the test pod as the cause, and not the configuration of warren", reason)
	}
}

// The last step of the test makes the policy of the test pod a second time.
// The second policy has a new UID, and warren deletes its objects with their
// UID. No policy of the test must stay.
func TestNetworkTestRemovesItsSecondPolicy(t *testing.T) {
	e, _ := canaryEnv(t, func(int32) error { return errTimeout })
	var made atomic.Int32
	e.client.PrependReactor("create", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if strings.HasPrefix(a.(k8stesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy).Name, canaryPrefix) {
			made.Add(1)
		}
		return false, nil, nil
	})
	e.m.examineEnforcement(context.Background())
	if !e.m.Ready() || made.Load() != 2 {
		t.Fatalf("ready = %v, policies of the test = %d, want ready with 2: %s", e.m.Ready(), made.Load(), e.m.NotReadyReason())
	}
	for name := range e.networkPolicies() {
		if name != defaultDenyName {
			t.Errorf("the network policy %s stays after the test", name)
		}
	}
	if n := len(e.canaries()); n != 0 {
		t.Errorf("test pods after the test = %d, want 0", n)
	}
}
