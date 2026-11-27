package kube

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/coderpunch/warren/internal/backend"
)

// reaps returns the number of reaper runs: each run lists the Secrets one
// time.
func (e *testEnv) reaps() int {
	n := 0
	for _, a := range e.client.Actions() {
		if a.Matches("list", "secrets") {
			n++
		}
	}
	return n
}

// run puts the manager back to the state of New and starts Run. Run stops at
// the end of the test.
func (e *testEnv) run() {
	e.t.Helper()
	e.m.enforcement.Store(enforcementUnknown)
	e.m.otherPolicies.Store(-1)
	e.m.networkReady.Store(false)
	if e.m.cfg.CanaryRetry == time.Minute {
		e.m.cfg.CanaryRetry = 20 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.m.Run(ctx)
	}()
	e.t.Cleanup(func() {
		cancel()
		<-done
	})
}

// refuseCanary makes Kubernetes refuse the test pod while on is true.
func (e *testEnv) refuseCanary(on *atomic.Bool, message string) {
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if on.Load() && a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Labels[LabelCanary] == "true" {
			return true, nil, errors.New(message)
		}
		return false, nil, nil
	})
}

// The reaper must continue while the test of the network plugin is not
// possible. With a ResourceQuota that the sandboxes use up, Kubernetes
// refuses the test pod, and only the reaper can make the quota free.
func TestReaperRunsWhileTheNetworkTestIsNotPossible(t *testing.T) {
	e := newTestEnv(t, func(c *Config) {
		c.ReapInterval = 10 * time.Millisecond
		c.Dial = func(context.Context, string) error { return errTimeout }
	})
	req := templateRef("quick", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	var full atomic.Bool
	full.Store(true)
	e.refuseCanary(&full, "exceeded quota")
	before := e.reaps()
	e.run()
	waitFor(t, "first run of the reaper", func() bool { return e.reaps() > before })
	// The sandbox becomes idle after the first run.
	e.clock.Advance(3 * time.Minute)
	waitFor(t, "stop of the idle sandbox while the network test is not possible", func() bool { return e.m.pod(name) == nil })
	if e.m.Ready() {
		t.Error("the manager is ready with no result of the network test")
	}
	if reason := e.m.NotReadyReason(); !strings.Contains(reason, "exceeded quota") {
		t.Errorf("reason = %q, want the cause", reason)
	}
}

// Run makes the namespace policy, examines the namespace, and does the test
// of the network plugin before it is ready. It does the test again after a
// wrong result.
func TestRunIsNotReadyBeforeTheNetworkTest(t *testing.T) {
	var ignores atomic.Bool
	ignores.Store(true)
	e, _ := canaryEnv(t, func(int32) error {
		if ignores.Load() {
			return nil
		}
		return errTimeout
	})
	e.m.cfg.ReapInterval = 10 * time.Millisecond
	if err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Delete(context.Background(), defaultDenyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	e.run()
	waitFor(t, "result of the test on a cluster that ignores network policies", func() bool {
		return e.m.enforcement.Load() == enforcementAbsent
	})
	if _, ok := e.networkPolicies()[defaultDenyName]; !ok {
		t.Error("Run did the test before it made the policy of the namespace")
	}
	if e.m.otherPolicies.Load() != 0 {
		t.Error("Run did the test before the reaper examined the namespace")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := e.m.Acquire(ctx, userRef("alice")); !errors.Is(err, backend.ErrNotReady) || e.m.Ready() {
		t.Errorf("Acquire = %v, ready = %v on a cluster that ignores network policies", err, e.m.Ready())
	}
	for _, p := range e.pods() {
		if p.Labels[LabelManagedBy] == ManagedByValue {
			t.Errorf("warren started the sandbox %s", p.Name)
		}
	}
	// The administrator corrects the network plugin.
	ignores.Store(false)
	waitFor(t, "ready after the next test", e.m.Ready)
	e.acquire(userRef("alice")).Release()
}

// Run tries again when Kubernetes refuses the policy of the namespace.
func TestRunWaitsForTheNamespacePolicy(t *testing.T) {
	e, _ := canaryEnv(t, func(int32) error { return errTimeout })
	e.m.cfg.ReapInterval = 10 * time.Millisecond
	if err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Delete(context.Background(), defaultDenyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	var refused atomic.Bool
	refused.Store(true)
	e.client.PrependReactor("create", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if refused.Load() && a.(k8stesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy).Name == defaultDenyName {
			return true, nil, errors.New("forbidden")
		}
		return false, nil, nil
	})
	before := e.reaps()
	e.run()
	waitFor(t, "some runs of the reaper", func() bool { return e.reaps() >= before+3 })
	if e.m.Ready() || !strings.Contains(e.m.NotReadyReason(), "forbidden") {
		t.Errorf("ready = %v, reason = %q, want not ready with the cause", e.m.Ready(), e.m.NotReadyReason())
	}
	if n := len(e.canaries()); n != 0 || e.m.enforcement.Load() != enforcementUnknown {
		t.Errorf("warren did the network test with no policy for the namespace: test pods = %d", n)
	}
	refused.Store(false)
	waitFor(t, "ready after the policy of the namespace", e.m.Ready)
}

// Run does the test again after the interval of the configuration.
func TestRunRepeatsTheNetworkTest(t *testing.T) {
	var ignores atomic.Bool
	e, _ := canaryEnv(t, func(int32) error {
		if ignores.Load() {
			return nil
		}
		return errTimeout
	})
	e.m.cfg.ReapInterval = 10 * time.Millisecond
	e.m.cfg.CanaryInterval = 30 * time.Millisecond
	e.run()
	waitFor(t, "ready", e.m.Ready)
	// The network plugin stops while warren operates.
	ignores.Store(true)
	waitFor(t, "not ready after a later test", func() bool { return e.m.enforcement.Load() == enforcementAbsent })
	if e.m.Ready() {
		t.Error("the manager is ready after a test that shows no network policies")
	}
}

// The test pod must get the image as a sandbox gets it: with the pull
// secrets, on the nodes of the policy.
func TestTestPodHasThePullSecretsAndTheNodesOfThePolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	e.usePolicies(`
defaults:
  imagePullSecrets: [registry-key]
  nodeSelector: {pool: sandboxes}
  tolerations:
    - {key: dedicated, operator: Equal, value: sandboxes, effect: NoSchedule}
policies:
  - {id: other, name: Other image, scope: user, image: registry.example/other:1, imagePullSecrets: [other-key], nodeSelector: {pool: other}}
  - {id: personal, name: Personal, scope: user}
`)
	pod := e.m.buildCanary("warren-canary-00000000")
	if got := pod.Spec.ImagePullSecrets; len(got) != 1 || got[0].Name != "registry-key" {
		t.Errorf("pull secrets of the test pod = %v, want those of the policy with the default image", got)
	}
	if pod.Spec.NodeSelector["pool"] != "sandboxes" || len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "dedicated" {
		t.Errorf("node selector = %v, tolerations = %v", pod.Spec.NodeSelector, pod.Spec.Tolerations)
	}
	if pod.Spec.Containers[0].Image != "example.test/terminal:1" {
		t.Errorf("image = %q", pod.Spec.Containers[0].Image)
	}
}

// "unreachable" is for a pod that warren cannot use for 60 seconds. Two
// failures with a long time between them do not show that.
func TestOneFailureLongAfterAnotherDoesNotStopThePod(t *testing.T) {
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
	e.clock.Advance(5 * time.Minute)
	if err := try(); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("one failure after 5 minutes: %v, want ErrNotReady", err)
	}
	if n := e.podDeletes(name); n != 0 {
		t.Errorf("delete requests for the pod = %d, want 0", n)
	}
	fail.Store(false)
	e.acquire(req).Release()
	if p := e.m.pod(name); p == nil || p.UID != uid {
		t.Error("warren replaced a pod that it could use")
	}
	// A success ends the sequence of failures.
	e.m.mu.Lock()
	open := e.m.sandboxes[name].unreachable
	e.m.mu.Unlock()
	if open != "" {
		t.Error("a successful key check did not end the record of the failures")
	}
}

// A request that ends in the middle of the key check shows nothing about the
// sandbox. It must not count as a failure.
func TestRequestThatEndsDuringTheKeyCheck(t *testing.T) {
	e := newTestEnv(t, func(c *Config) {
		c.Verify = func(ctx context.Context, _ backend.Target) error {
			<-ctx.Done()
			return ctx.Err()
		}
	})
	req := userRef("alice")
	name := req.Ref.Name()
	for i, wait := range []time.Duration{0, 30 * time.Second, 40 * time.Second} {
		e.clock.Advance(wait)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		res := e.acquireAsync(ctx, e.m, req)
		waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
		if !podReady(e.m.pod(name)) {
			e.makeReady(name)
		}
		err := (<-res).err
		cancel()
		if !errors.Is(err, backend.ErrNotReady) {
			t.Errorf("request %d: %v, want ErrNotReady", i+1, err)
		}
	}
	if n := e.podDeletes(name); n != 0 {
		t.Errorf("delete requests for the pod = %d, want 0", n)
	}
}

// A sandbox that operated and whose server stops two times gets the same
// time as a new sandbox: Kubernetes starts the container again with the
// same home directory.
func TestCrashLoopOfASandboxThatOperated(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	uid := e.m.pod(name).UID
	e.clock.Advance(20 * time.Minute)
	e.setStatus(name, func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(e.clock.Now())}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerName, RestartCount: 2, State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 10s restarting failed container"},
		}}}
	})
	waitFor(t, "status in the cache", func() bool { return len(e.m.pod(name).Status.ContainerStatuses) == 1 })
	e.m.Reap(context.Background())
	time.Sleep(50 * time.Millisecond)
	if p := e.m.pod(name); p == nil || p.UID != uid || e.podDeletes(name) != 0 {
		t.Fatal("warren stopped a sandbox in the first seconds of a CrashLoopBackOff")
	}
	// The container operates again.
	e.setStatus(name, func(p *corev1.Pod) {
		readyStatus(p)
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerName, RestartCount: 3, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	})
	waitFor(t, "pod ready in the cache", func() bool { return podReady(e.m.pod(name)) })
	e.clock.Advance(3 * time.Minute)
	e.m.Reap(context.Background())
	// A later error starts a new time.
	e.setStatus(name, func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(e.clock.Now())}}
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	})
	waitFor(t, "status in the cache", func() bool { return !podReady(e.m.pod(name)) })
	e.m.Reap(context.Background())
	time.Sleep(50 * time.Millisecond)
	if e.podDeletes(name) != 0 {
		t.Fatal("the time of a previous error counted for a new error")
	}
	// The error stays for more than 2 minutes.
	e.clock.Advance(startErrorAfter + time.Second)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the pod that cannot start", func() bool { return e.m.pod(name) == nil })
}

// The stop of a sandbox after a removed network permission must not depend
// on one delete request.
func TestRemovedPermissionStopsThePodAfterAFailedDelete(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("quick", "alice")
	name := req.Ref.Name()
	lease := e.acquire(req)
	defer lease.Release()
	policyName := e.policyOf(req).Name
	var failed atomic.Bool
	e.client.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failed.CompareAndSwap(false, true) {
			return true, nil, errors.New("the server was unable to return a response in the time allotted")
		}
		return false, nil, nil
	})
	e.usePolicies("policies:\n  - {id: quick, name: Quick, scope: user, storage: {type: ephemeral}, network: {egress: none}}\n")
	e.m.Reap(context.Background())
	if np := e.networkPolicies()[policyName]; len(np.Spec.Egress) != 0 {
		t.Errorf("egress rules after the change to none = %+v", np.Spec.Egress)
	}
	if !failed.Load() || e.m.pod(name) == nil {
		t.Fatal("the first delete request did not fail")
	}
	e.m.Reap(context.Background())
	waitFor(t, "stop of the sandbox at the next run of the reaper", func() bool { return e.m.pod(name) == nil })
}

// Warren is not ready while the namespace has no policy that refuses all
// connections, also when the policy went away after the start.
func TestNotReadyWhenTheNamespacePolicyCannotComeBack(t *testing.T) {
	e := newTestEnv(t, nil)
	ctx := context.Background()
	// A person starts a pod in the sandbox namespace. The pod cache of the
	// simulated cluster ignores the label selector, thus the pod is only in
	// the answer to a list request.
	var debug atomic.Bool
	debug.Store(true)
	e.client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if debug.Load() {
			return true, &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "debug-0", Namespace: testNamespace}}}}, nil
		}
		return false, nil, nil
	})
	if err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Delete(ctx, defaultDenyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	e.m.Reap(ctx)
	if _, ok := e.networkPolicies()[defaultDenyName]; ok {
		t.Fatal("warren made the policy in a namespace with a pod of a different owner")
	}
	if e.m.Ready() || !strings.Contains(e.m.NotReadyReason(), "debug-0") {
		t.Errorf("ready = %v, reason = %q, want not ready with the name of the pod", e.m.Ready(), e.m.NotReadyReason())
	}
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := e.m.Acquire(short, userRef("alice")); !errors.Is(err, backend.ErrNotReady) || e.creates() != 0 {
		t.Errorf("Acquire with no policy for the namespace: %v, pods made = %d, want ErrNotReady and no sandbox", err, e.creates())
	}
	// The pod goes away, and the reaper makes the policy again.
	debug.Store(false)
	e.m.Reap(ctx)
	if _, ok := e.networkPolicies()[defaultDenyName]; !ok || !e.m.Ready() {
		t.Fatalf("policy made again = %v, not ready: %s", ok, e.m.NotReadyReason())
	}
	// One error of a read request is not the loss of the policy.
	e.client.PrependReactor("get", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcd timeout")
	})
	e.m.Reap(ctx)
	if !e.m.Ready() {
		t.Error("one error of a read request made warren not ready")
	}
}

// The reaper must not delete the network policy of a network test that is
// in progress: the test of this process, or of a second process.
func TestReaperKeepsThePolicyOfATestInProgress(t *testing.T) {
	e := newTestEnv(t, nil)
	ctx := context.Background()
	name := "warren-canary-0a1b2c3d"
	np := e.m.buildNetworkPolicy(name, map[string]string{LabelSandbox: name}, e.policy(templateRef("closed", "alice")), nil)
	if _, err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Create(ctx, np, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// The image pull of the test pod needs more than 2 minutes.
	e.clock.Advance(orphanAge + time.Second)
	e.m.Reap(ctx)
	if _, ok := e.networkPolicies()[name]; !ok {
		t.Fatal("the reaper deleted the policy of a network test in progress")
	}
	// The policy of a test that did not end goes away later.
	e.clock.Advance(10 * time.Minute)
	e.m.Reap(ctx)
	if _, ok := e.networkPolicies()[name]; ok {
		t.Error("the reaper did not delete the policy of an old network test")
	}
}

// A connection error in the last step is the proof only if the test pod
// still operates. If the pod is gone, each connection fails.
func TestNetworkTestWhoseTestPodGoesAway(t *testing.T) {
	ctx := context.Background()
	ways := map[string]func(e *testEnv, name string){
		// A second warren process deletes the pod.
		"deleted": func(e *testEnv, name string) {
			_ = e.client.CoreV1().Pods(testNamespace).Delete(ctx, name, metav1.DeleteOptions{})
		},
		// The node evicts the pod, or the time limit of the pod ends.
		"evicted": func(e *testEnv, name string) {
			e.setStatus(name, func(p *corev1.Pod) { p.Status.Phase, p.Status.Reason = corev1.PodFailed, "Evicted" })
		},
	}
	for way, remove := range ways {
		var e *testEnv
		e, _ = canaryEnv(t, func(n int32) error {
			if n == 1 {
				// The cluster ignores network policies: the first connection
				// is successful. Then the test pod goes away.
				for _, p := range e.canaries() {
					remove(e, p.Name)
				}
				return nil
			}
			return errTimeout
		})
		e.m.examineEnforcement(ctx)
		if e.m.enforcement.Load() != enforcementUnknown || e.m.Ready() {
			t.Fatalf("%s: a connection error to a test pod that is gone was the proof that the cluster applies network policies", way)
		}
		if reason := e.m.NotReadyReason(); !strings.Contains(reason, "test pod") {
			t.Errorf("%s: reason = %q, want the cause", way, reason)
		}
		if n := len(e.canaries()); n != 0 {
			t.Errorf("%s: test pods after the test = %d, want 0", way, n)
		}
	}
}

// A test must not delete the test pod of a test that a second process
// started a short time before.
func TestNetworkTestKeepsTheTestPodOfASecondProcess(t *testing.T) {
	e, _ := canaryEnv(t, func(int32) error { return errTimeout })
	ctx := context.Background()
	other := e.m.buildCanary("warren-canary-11111111")
	if _, err := e.client.CoreV1().Pods(testNamespace).Create(ctx, other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.m.examineEnforcement(ctx)
	if !e.m.Ready() {
		t.Fatalf("not ready: %s", e.m.NotReadyReason())
	}
	if got := e.canaries(); len(got) != 1 || got[0].Name != "warren-canary-11111111" {
		t.Errorf("test pods = %d, want only the pod of the second process", len(got))
	}
}

// The network test decides the start of sandboxes. It must not stop an
// administrator who wants to stop or delete the sandboxes that operate.
func TestStopAndDeleteWhenTheClusterIgnoresNetworkPolicies(t *testing.T) {
	e := newTestEnv(t, nil)
	alice, bob := userRef("alice"), templateRef("kept", "bob")
	e.acquire(alice).Release()
	e.acquire(bob).Release()
	e.m.enforcement.Store(enforcementAbsent)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !e.m.Synced() || e.m.Ready() {
		t.Fatalf("synced = %v, ready = %v", e.m.Synced(), e.m.Ready())
	}
	if err := e.m.Stop(ctx, alice.Ref); err != nil {
		t.Errorf("Stop = %v", err)
	}
	waitFor(t, "stop of the sandbox", func() bool { return e.m.pod(alice.Ref.Name()) == nil })
	if err := e.m.Delete(ctx, bob.Ref); err != nil {
		t.Errorf("Delete = %v", err)
	}
	if e.m.pod(bob.Ref.Name()) != nil || e.volumeFromAPI(bob.Ref.Name()) != nil {
		t.Error("Delete did not remove the pod and the volume")
	}
	if got := e.m.Instances("personal"); len(got) != 0 {
		t.Errorf("instances = %v", got)
	}
	// No sandbox starts.
	if err := e.m.Start(ctx, alice); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Start = %v, want ErrNotReady", err)
	}
	if err := e.m.Restart(ctx, alice); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Restart = %v, want ErrNotReady", err)
	}
}

// A policy of a different owner that has only an egress rule also permits
// what warren refuses.
func TestNetworkPolicyOfADifferentOwnerWithAnEgressRule(t *testing.T) {
	e := newTestEnv(t, nil)
	other := e.m.buildDefaultDeny()
	other.Name, other.Labels = "allow-all-egress", nil
	other.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	if _, err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.m.Reap(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := e.m.Acquire(ctx, userRef("alice")); err == nil || e.m.otherPolicies.Load() != 1 {
		t.Errorf("Acquire = %v, policies of a different owner = %d, want an error and 1", err, e.m.otherPolicies.Load())
	}
}

// If the start could not set the owner of a network policy, the reaper sets
// it. Kubernetes then deletes the policy with the pod.
func TestReaperSetsTheOwnerOfANetworkPolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	e.acquire(req).Release()
	ctx := context.Background()
	policies := e.client.NetworkingV1().NetworkPolicies(testNamespace)
	np := e.policyOf(req)
	np.OwnerReferences = nil
	if _, err := policies.Update(ctx, &np, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	uid := e.m.pod(req.Ref.Name()).UID
	e.m.Reap(ctx)
	if got := e.policyOf(req).OwnerReferences; len(got) != 1 || got[0].UID != uid {
		t.Errorf("owner after the reaper run = %+v, want the pod", got)
	}
	if p := e.m.pod(req.Ref.Name()); p == nil || p.UID != uid || e.podDeletes(req.Ref.Name()) != 0 {
		t.Error("the repair of the owner stopped the sandbox")
	}
}

// A start makes the network policy before the pod. The reaper must not
// delete the policy of a start that is in progress.
func TestReaperKeepsThePolicyOfAStartInProgress(t *testing.T) {
	e, waiting, release := newHoldEnv(t, "create", "pods")
	req := userRef("alice")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, req)
	<-waiting
	if n := len(e.networkPolicies()); n != 2 {
		t.Fatalf("network policies before the pod = %d, want 2", n)
	}
	e.clock.Advance(orphanAge + time.Minute)
	e.m.Reap(context.Background())
	if n := len(e.networkPolicies()); n != 2 {
		t.Errorf("network policies after the reaper run = %d: the reaper deleted the policy of a start in progress", n)
	}
	release()
	name := req.Ref.Name()
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	e.makeReady(name)
	if r := <-res; r.err != nil {
		t.Fatal(r.err)
	} else {
		r.lease.Release()
	}
	_ = e.policyOf(req)
}

// The policy of a sandbox must refuse the address of its own node. Warren
// knows the node only after the start of the pod, thus it must not wait for
// the next run of the reaper.
func TestNodeAddressOfANewSandboxIsBlockedImmediately(t *testing.T) {
	e, _ := canaryEnv(t, func(int32) error { return errTimeout })
	e.m.cfg.ReapInterval = time.Hour
	e.run()
	waitFor(t, "ready", e.m.Ready)
	req := userRef("alice")
	e.acquire(req).Release()
	e.setStatus(req.Ref.Name(), func(p *corev1.Pod) {
		p.Status.HostIP = "203.0.113.5"
		p.Status.HostIPs = []corev1.HostIP{{IP: "203.0.113.5"}}
	})
	waitFor(t, "node address in the policy of the sandbox", func() bool {
		for _, cidr := range e.policyOf(req).Spec.Egress[1].To[0].IPBlock.Except {
			if cidr == "203.0.113.5/32" {
				return true
			}
		}
		return false
	})
}

// An API server that does not have the feature UserNamespacesSupport stores
// the pod with no hostUsers field, and gives no error. Such a pod must not
// operate: its policy asks for a user namespace.
func TestUserNamespaceThatTheClusterIgnores(t *testing.T) {
	e := newTestEnv(t, nil)
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Spec.HostUsers = nil
		return false, nil, nil
	})
	req := templateRef("other-image", "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := e.m.Acquire(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "user namespace") {
		t.Errorf("Acquire = %v, want an error that names the user namespace", err)
	}
	if n := len(e.pods()); n != 0 {
		t.Errorf("pods = %d, want 0: the pod operates in the user namespace of the node", n)
	}
	if n, s := len(e.networkPolicies()), len(e.secrets()); n != 1 || s != 0 {
		t.Errorf("network policies = %d, secrets = %d after the failed start, want 1 and 0", n, s)
	}
	// A policy with no userNamespace key starts as before.
	e.acquire(userRef("alice")).Release()
}

// Two warren processes operate at the same time in a rollout. On a cluster
// that ignores network policies, the test of one process must not make the
// test of the other process successful: each process removed the test pods
// of the other, and a connection to a pod that is gone fails.
func TestTwoProcessesOnAClusterThatIgnoresNetworkPolicies(t *testing.T) {
	ctx := context.Background()
	var e *testEnv
	var address atomic.Int32
	var dials atomic.Int32
	// The cluster applies no policy: each connection to a pod that exists is
	// successful.
	dial := func(_ context.Context, addr string) error {
		for _, p := range e.canaries() {
			if strings.HasPrefix(addr, p.Status.PodIP+":") {
				dials.Add(1)
				return nil
			}
		}
		return errTimeout
	}
	e = newTestEnv(t, func(c *Config) { c.Dial = dial })
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if pod.Labels[LabelCanary] == "true" {
			readyStatus(pod)
			pod.Status.PodIP = "10.0.0." + strconv.Itoa(int(address.Add(1)))
		}
		return false, nil, nil
	})
	first := e.m
	first.enforcement.Store(enforcementUnknown)
	cfg := first.cfg
	second := e.newManager(cfg)
	second.enforcement.Store(enforcementUnknown)

	done := make(chan struct{})
	go func() {
		defer close(done)
		first.examineEnforcement(ctx)
	}()
	// The first process is in the last step of its test: its connections are
	// successful. Now the second process starts its test.
	waitFor(t, "last step of the first test", func() bool { return dials.Load() >= 3 })
	second.examineEnforcement(ctx)
	<-done
	if first.enforcement.Load() == enforcementProven || second.enforcement.Load() == enforcementProven {
		t.Errorf("results = %d and %d: a process found that the cluster applies network policies", first.enforcement.Load(), second.enforcement.Load())
	}
	if first.Ready() || second.Ready() {
		t.Error("a process is ready on a cluster that ignores network policies")
	}
}

// The reaper stops a pod of a previous version of warren because the policy
// of the namespace refuses all its connections. While that policy does not
// exist, this process starts no sandbox, and the pod can operate for a
// process of the previous version: in a rollout, that process has the users
// until this process is ready. Run must not stop such a pod in that time.
func TestSandboxOfAPreviousVersionWhileTheNamespaceHasNoPolicy(t *testing.T) {
	e, _ := canaryEnv(t, func(int32) error { return errTimeout })
	e.m.cfg.ReapInterval = 10 * time.Millisecond
	ctx := context.Background()
	// The namespace has a pod of a different owner, thus warren does not
	// make the policy of the namespace.
	var other atomic.Bool
	other.Store(true)
	e.client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !other.Load() {
			return false, nil, nil
		}
		return true, &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "database-0", Namespace: testNamespace}}}}, nil
	})
	if err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Delete(ctx, defaultDenyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	req := userRef("alice")
	name := req.Ref.Name()
	old := e.m.buildPod(name, name+"-00000000", req, e.policy(req))
	delete(old.Labels, LabelStart)
	readyStatus(old)
	if _, err := e.client.CoreV1().Pods(testNamespace).Create(ctx, old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod in the cache", func() bool { return e.m.pod(name) != nil })

	before := e.reaps()
	e.run()
	waitFor(t, "three runs of the reaper", func() bool { return e.reaps() >= before+3 })
	if e.m.Ready() {
		t.Fatal("ready in a namespace with a pod of a different owner and no policy")
	}
	if n := e.podDeletes(name); n != 0 {
		t.Errorf("warren is not ready (%s), and it stopped a sandbox of the previous version: delete requests = %d, want 0", e.m.NotReadyReason(), n)
	}
	// The administrator removes the other pod. Warren makes the policy of
	// the namespace, and that policy refuses all connections of the old pod.
	other.Store(false)
	waitFor(t, "stop of the sandbox of the previous version", func() bool { return e.podDeletes(name) > 0 })
	if _, ok := e.networkPolicies()[defaultDenyName]; !ok {
		t.Error("warren stopped the sandbox of the previous version before the namespace had its policy")
	}
	waitFor(t, "ready", e.m.Ready)
}

// startPrevious makes a pod as a version of warren before the network
// policies made it: a pod of warren with no label for a network policy. It
// returns the name of the pod.
func (e *testEnv) startPrevious(req backend.Request, status func(*corev1.Pod)) string {
	e.t.Helper()
	name := req.Ref.Name()
	old := e.m.buildPod(name, name+"-00000000", req, e.policy(req))
	delete(old.Labels, LabelStart)
	status(old)
	if _, err := e.client.CoreV1().Pods(testNamespace).Create(context.Background(), old, metav1.CreateOptions{}); err != nil {
		e.t.Fatal(err)
	}
	waitFor(e.t, "pod in the cache", func() bool { return e.m.pod(name) != nil })
	return name
}

// On a cluster that ignores network policies, the policy of the namespace
// refuses no connection, and this process is never ready. A pod of a
// previous version can operate for a process of that version. The reaper
// must not stop such a pod before the network test showed that the cluster
// applies network policies: not before the first test, and not after a test
// with the result that the cluster ignores them.
func TestSandboxOfAPreviousVersionOnAClusterThatIgnoresNetworkPolicies(t *testing.T) {
	var ignores atomic.Bool
	ignores.Store(true)
	e, _ := canaryEnv(t, func(int32) error {
		if ignores.Load() {
			return nil
		}
		return errTimeout
	})
	e.m.cfg.ReapInterval = 10 * time.Millisecond
	// The state before the upgrade: the namespace has no policy.
	if err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Delete(context.Background(), defaultDenyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	name := e.startPrevious(userRef("alice"), readyStatus)

	e.run()
	waitFor(t, "result of the test on a cluster that ignores network policies", func() bool {
		return e.m.enforcement.Load() == enforcementAbsent
	})
	before := e.reaps()
	waitFor(t, "three runs of the reaper after the test", func() bool { return e.reaps() >= before+3 })
	if e.m.Ready() {
		t.Fatal("ready on a cluster that ignores network policies")
	}
	if _, ok := e.networkPolicies()[defaultDenyName]; !ok {
		t.Fatal("warren did not make the policy of the namespace")
	}
	if n := e.podDeletes(name); n != 0 {
		t.Errorf("warren is not ready (%s), and it stopped a sandbox of the previous version: delete requests = %d, want 0", e.m.NotReadyReason(), n)
	}
	// The administrator corrects the network plugin. The policy of the
	// namespace now refuses all connections of the old pod.
	ignores.Store(false)
	waitFor(t, "stop of the sandbox of the previous version", func() bool { return e.podDeletes(name) > 0 })
	if e.m.enforcement.Load() != enforcementProven {
		t.Error("warren stopped the sandbox of the previous version before the proof that the cluster applies network policies")
	}
	waitFor(t, "ready", e.m.Ready)
}

// With the network test turned off, warren has no result of a test. The
// reaper must still stop a pod of a previous version when the namespace has
// its policy.
func TestSandboxOfAPreviousVersionWithNoNetworkTest(t *testing.T) {
	e := newTestEnv(t, func(c *Config) {
		c.ReapInterval = 10 * time.Millisecond
		c.Network.SkipEnforcementCheck = true
		c.Dial = func(context.Context, string) error { t.Error("warren did the test"); return nil }
	})
	if err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Delete(context.Background(), defaultDenyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	name := e.startPrevious(userRef("alice"), readyStatus)

	e.run()
	waitFor(t, "stop of the sandbox of the previous version", func() bool { return e.podDeletes(name) > 0 })
	if _, ok := e.networkPolicies()[defaultDenyName]; !ok {
		t.Error("warren stopped the sandbox of the previous version before the namespace had its policy")
	}
	waitFor(t, "ready", e.m.Ready)
}

// While the reaper does not stop a pod of a previous version because of its
// version, the other rules of the reaper apply to the pod. Possibly no
// process of the previous version operates, and then only this process can
// stop the pod (docs/isolation.md, sections 4 and 8).
func TestReaperRulesForASandboxOfAPreviousVersion(t *testing.T) {
	states := map[string]func(t *testing.T, e *testEnv){
		"the namespace has no policy": func(t *testing.T, e *testEnv) {
			// The namespace has a pod of a different owner, thus warren does
			// not make the policy of the namespace.
			e.client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "database-0", Namespace: testNamespace}}}}, nil
			})
			if err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Delete(context.Background(), defaultDenyName, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		},
		"the network test has no result":       func(_ *testing.T, e *testEnv) { e.m.enforcement.Store(enforcementUnknown) },
		"the cluster ignores network policies": func(_ *testing.T, e *testEnv) { e.m.enforcement.Store(enforcementAbsent) },
	}
	for state, prepare := range states {
		t.Run(state, func(t *testing.T) {
			logs := &syncBuffer{}
			e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
			e.strictDeletes(corev1.SchemeGroupVersion.WithResource("pods"))
			ctx := context.Background()
			// The control: a sandbox of this version, with no open lease.
			current := userRef("erin")
			e.acquire(current).Release()
			prepare(t, e)

			idle := e.startPrevious(userRef("alice"), readyStatus)
			slow := e.startPrevious(userRef("bob"), func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending })
			failed := e.startPrevious(userRef("carol"), func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
			orphan := e.startPrevious(userRef("dave"), func(p *corev1.Pod) { readyStatus(p); p.Labels[LabelTemplate] = "removed-policy" })
			stopped := func(name, reason string) bool {
				return strings.Contains(logs.String(), "sandbox="+name+" reason="+reason+" ")
			}

			// The first run: no time is gone.
			e.m.Reap(ctx)
			if e.m.Ready() {
				t.Fatal("warren is ready")
			}
			if !stopped(failed, "failed") {
				t.Errorf("warren is not ready (%s), and it did not remove the failed sandbox of the previous version", e.m.NotReadyReason())
			}
			if !stopped(orphan, "no_policy") {
				t.Errorf("warren is not ready (%s), and it did not stop the sandbox of the previous version whose policy is not in the file", e.m.NotReadyReason())
			}
			if a, b := e.podDeletes(idle), e.podDeletes(slow); a != 0 || b != 0 {
				t.Errorf("warren is not ready (%s), and it stopped sandboxes of the previous version that are not idle and not late: delete requests = %d and %d, want 0",
					e.m.NotReadyReason(), a, b)
			}
			waitFor(t, "end of the stopped pods", func() bool { return e.m.pod(failed) == nil && e.m.pod(orphan) == nil })

			// The idle time of the policy is 30 minutes, and the start time
			// is 5 minutes.
			e.clock.Advance(10 * time.Hour)
			e.m.Reap(ctx)
			if e.m.Ready() {
				t.Fatal("warren is ready")
			}
			if !stopped(current.Ref.Name(), "idle") {
				t.Errorf("control: warren did not stop the idle sandbox of this version")
			}
			if !stopped(idle, "idle") {
				t.Errorf("warren is not ready (%s), and it did not stop the idle sandbox of the previous version", e.m.NotReadyReason())
			}
			if !stopped(slow, "not_ready") {
				t.Errorf("warren is not ready (%s), and it did not stop the sandbox of the previous version that is not ready after 10 hours", e.m.NotReadyReason())
			}
			if strings.Contains(logs.String(), "reason=no_network_policy") {
				t.Errorf("warren is not ready (%s), and it stopped a sandbox because of its version", e.m.NotReadyReason())
			}
			for _, name := range []string{idle, slow, failed, orphan} {
				if n := e.podDeletes(name); n != 1 {
					t.Errorf("delete requests for the sandbox %s = %d, want 1", name, n)
				}
			}
		})
	}
}

// The reaper writes the cause of a problem with the policy of the namespace,
// and the network test writes the cause of a test that is not possible. The
// two operate at the same time. The readiness report must give each
// condition its own cause, and not the cause of a problem that is gone.
func TestReadinessReportHasTheCauseOfItsCondition(t *testing.T) {
	e, _ := canaryEnv(t, func(int32) error { return errTimeout })
	ctx := context.Background()
	if err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Delete(ctx, defaultDenyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	var refuse, full atomic.Bool
	refuse.Store(true)
	full.Store(true)
	e.client.PrependReactor("create", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if refuse.Load() && a.(k8stesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy).Name == defaultDenyName {
			return true, nil, errors.New("admission webhook denied the request")
		}
		return false, nil, nil
	})
	e.refuseCanary(&full, "exceeded quota")
	e.m.Reap(ctx)
	// A network test ends after the reaper found the problem.
	e.m.examineEnforcement(ctx)
	reason := e.m.NotReadyReason()
	if !strings.Contains(reason, "has no network policy") || !strings.Contains(reason, "admission webhook") {
		t.Fatalf("reason = %q, want the policy of the namespace and its cause", reason)
	}
	if strings.Contains(reason, "exceeded quota") {
		t.Errorf("reason = %q: it gives the cause of the network test for the policy of the namespace", reason)
	}
	// The administrator corrects the webhook. The policy of the namespace
	// exists again, and only the network test has no result.
	refuse.Store(false)
	e.m.Reap(ctx)
	reason = e.m.NotReadyReason()
	if !strings.Contains(reason, "test of the network policies is not complete") || !strings.Contains(reason, "exceeded quota") {
		t.Fatalf("reason = %q, want the network test and its cause", reason)
	}
	if strings.Contains(reason, "admission webhook") || strings.Contains(reason, defaultDenyName) {
		t.Errorf("reason = %q: it gives the cause of a problem that is gone", reason)
	}
}

// Two warren processes can repair the policy of the namespace at the same
// time, for example in a rollout. One of them gets a conflict, and the
// policy in the cluster is correct then. That process must read the policy
// again. It must not refuse all requests until its next reaper run.
func TestConflictAtTheRepairOfTheNamespacePolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	ctx := context.Background()
	cur, err := e.client.NetworkingV1().NetworkPolicies(testNamespace).Get(ctx, defaultDenyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// stale is the number of read requests that give the policy as a person
	// changed it. The cluster itself always has the correct policy: the
	// second process repaired it.
	var stale, repairs atomic.Int32
	e.client.PrependReactor("get", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() != defaultDenyName || stale.Add(-1) < 0 {
			return false, nil, nil
		}
		changed := cur.DeepCopy()
		changed.Spec.PolicyTypes = nil
		return true, changed, nil
	})
	e.client.PrependReactor("update", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		repairs.Add(1)
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"},
			defaultDenyName, errors.New("the object has been modified"))
	})
	stale.Store(1)
	e.m.Reap(ctx)
	if !e.m.Ready() {
		t.Errorf("not ready after one conflict: %s (the policy in the cluster is correct)", e.m.NotReadyReason())
	}
	if n := repairs.Load(); n != 1 {
		t.Errorf("repair requests = %d, want 1", n)
	}
	// A policy that stays wrong, with a conflict at each repair: warren does
	// not try with no end, and it starts no sandbox.
	repairs.Store(0)
	stale.Store(100)
	e.m.Reap(ctx)
	if e.m.Ready() {
		t.Error("ready although warren could not repair the policy of the namespace")
	}
	if n := repairs.Load(); n != defaultDenyAttempts {
		t.Errorf("repair requests = %d, want %d", n, defaultDenyAttempts)
	}
}

// After one successful test, a test that is not possible keeps the result,
// and the next test comes after the usual interval, not after the short
// time of a warren that is not ready (docs/isolation.md, section 4).
func TestNextTestAfterATestThatIsNotPossible(t *testing.T) {
	e, _ := canaryEnv(t, func(int32) error { return errTimeout })
	e.m.cfg.ReapInterval = 10 * time.Millisecond
	e.m.cfg.CanaryInterval = 500 * time.Millisecond
	e.m.cfg.CanaryRetry = 5 * time.Millisecond
	var full atomic.Bool
	var refused atomic.Int32
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if full.Load() && a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Labels[LabelCanary] == "true" {
			refused.Add(1)
			return true, nil, errors.New("exceeded quota")
		}
		return false, nil, nil
	})
	e.run()
	waitFor(t, "ready", e.m.Ready)
	full.Store(true)
	waitFor(t, "a test that is not possible", func() bool { return refused.Load() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if n := refused.Load(); n != 1 {
		t.Errorf("tests in 100 ms after a test that was not possible = %d, want 1: the interval is 500 ms", n)
	}
	if !e.m.Ready() {
		t.Errorf("not ready after a test that was not possible: %s", e.m.NotReadyReason())
	}
}

// While warren is not ready, it refuses each new request to a sandbox, also
// to a sandbox that operates. A terminal that is open stays open
// (docs/isolation.md, section 4).
func TestNotReadyRefusesEachNewRequest(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	open := e.acquire(req)
	defer open.Release()
	e.m.enforcement.Store(enforcementAbsent)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := e.m.Acquire(ctx, req); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Acquire for a sandbox that operates = %v, want %v", err, backend.ErrNotReady)
	}
	if err := e.m.Restart(ctx, req); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Restart = %v, want %v", err, backend.ErrNotReady)
	}
	e.m.Reap(ctx)
	if n := e.podDeletes(req.Ref.Name()); n != 0 {
		t.Errorf("warren stopped a sandbox with an open terminal: delete requests = %d", n)
	}
}

// notReadyStates has two conditions in which warren is not ready after it
// was ready (docs/isolation.md, sections 3.1 and 4).
var notReadyStates = map[string]func(m *Manager){
	// A test of the network plugin showed that the cluster does not apply
	// network policies.
	"the cluster ignores network policies": func(m *Manager) { m.enforcement.Store(enforcementAbsent) },
	// The reaper could not repair the policy of the namespace.
	"the namespace has no policy": func(m *Manager) { m.networkReady.Store(false) },
}

// previousPodInDeletion puts a pod of the sandbox into the cluster that is
// in deletion, as in the termination time of a pod. It returns the UID.
func (e *testEnv) previousPodInDeletion(req backend.Request) types.UID {
	e.t.Helper()
	name := req.Ref.Name()
	deleted := metav1.NewTime(e.clock.Now())
	old := e.m.buildPod(name, name+"-00000000", req, e.policy(req))
	old.DeletionTimestamp = &deleted
	old.Finalizers = []string{"example.test/termination"}
	made, err := e.client.CoreV1().Pods(testNamespace).Create(context.Background(), old, metav1.CreateOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	waitFor(e.t, "previous pod in the cache", func() bool { return e.m.pod(name) != nil })
	return made.UID
}

// endTermination does the work of the kubelet at the end of the termination
// time: the pod goes away.
func (e *testEnv) endTermination(name string) {
	e.t.Helper()
	if err := e.client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), testNamespace, name); err != nil {
		e.t.Fatal(err)
	}
}

// startedAfter reports whether warren started a pod of the sandbox after
// the pod with the UID previous.
func (e *testEnv) startedAfter(name string, previous types.UID) bool {
	e.t.Helper()
	for _, p := range e.pods() {
		if p.Name == name && p.UID != previous {
			return true
		}
	}
	return false
}

// A request can wait for the end of the previous pod of its sandbox. If
// warren becomes not ready in that time, the request must not start the
// sandbox (docs/isolation.md, section 4).
func TestRequestThatWaitsStartsNoSandboxAfterNotReady(t *testing.T) {
	for state, set := range notReadyStates {
		t.Run(state, func(t *testing.T) {
			e := newTestEnv(t, nil)
			req := userRef("alice")
			name := req.Ref.Name()
			previous := e.previousPodInDeletion(req)

			// The request comes while warren is ready.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res := e.acquireAsync(ctx, e.m, req)
			select {
			case r := <-res:
				t.Fatalf("Acquire did not wait for the end of the previous pod: %v", r.err)
			case <-time.After(100 * time.Millisecond):
			}

			set(e.m)
			e.endTermination(name)
			select {
			case r := <-res:
				if r.err == nil {
					r.lease.Release()
				}
				if !errors.Is(r.err, backend.ErrNotReady) {
					t.Errorf("Acquire that waited = %v, want ErrNotReady", r.err)
				}
			case <-time.After(3 * time.Second):
				t.Error("the request that waited did not end after warren became not ready")
			}
			if e.startedAfter(name, previous) {
				t.Errorf("warren started a sandbox while it was not ready: %s", e.m.NotReadyReason())
			}
		})
	}
}

// A start of the management API can also wait for the end of the previous
// pod. If warren becomes not ready in that time, the operation must not
// start the sandbox.
func TestManagementStartThatWaitsStartsNoSandboxAfterNotReady(t *testing.T) {
	for state, set := range notReadyStates {
		t.Run(state, func(t *testing.T) {
			e := newTestEnv(t, nil)
			req := userRef("alice")
			name := req.Ref.Name()
			previous := e.previousPodInDeletion(req)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- e.m.Start(ctx, req) }()
			select {
			case err := <-done:
				t.Fatalf("Start did not wait for the end of the previous pod: %v", err)
			case <-time.After(100 * time.Millisecond):
			}

			set(e.m)
			e.endTermination(name)
			if err := <-done; !errors.Is(err, backend.ErrNotReady) {
				t.Errorf("Start that waited = %v, want ErrNotReady", err)
			}
			if e.startedAfter(name, previous) {
				t.Errorf("warren started a sandbox while it was not ready: %s", e.m.NotReadyReason())
			}
		})
	}
}

// A request can wait for a pod that starts. If warren becomes not ready in
// that time, the request must get no lease when the pod is ready. It must
// not wait until its time limit.
func TestRequestThatWaitsGetsNoLeaseAfterNotReady(t *testing.T) {
	for state, set := range notReadyStates {
		t.Run(state, func(t *testing.T) {
			e := newTestEnv(t, nil)
			req := userRef("alice")
			name := req.Ref.Name()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res := e.acquireAsync(ctx, e.m, req)
			waitFor(t, "pod "+name, func() bool { return e.m.pod(name) != nil })

			set(e.m)
			e.makeReady(name)
			select {
			case r := <-res:
				if r.err == nil {
					r.lease.Release()
					t.Fatalf("Acquire gave a lease while warren was not ready: %s", e.m.NotReadyReason())
				}
				if !errors.Is(r.err, backend.ErrNotReady) {
					t.Errorf("Acquire that waited = %v, want ErrNotReady", r.err)
				}
			case <-time.After(3 * time.Second):
				t.Error("the request that waited did not end after warren became not ready")
			}
		})
	}
}
