package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/policy"
)

// canaryEnv makes an environment in which the test pod of the network test
// becomes ready immediately, as on a cluster.
//
// The test of warren has two steps. While the test pod has its own network
// policy, a connection attempt of warren is successful: the policy permits
// warren. After warren deleted that policy, dial gives the result of each
// connection attempt: an error on a cluster that applies network policies.
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
	if !e.m.Ready() || attempts.Load() != 1 {
		t.Errorf("ready = %v after %d connection attempts, want ready after 1", e.m.Ready(), attempts.Load())
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
	if !e.m.Ready() || attempts.Load() != 3 {
		t.Errorf("ready = %v after %d attempts, want ready after 3", e.m.Ready(), attempts.Load())
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
	if _, err := e.m.Acquire(context.Background(), userRef("bob")); err == nil {
		t.Error("a sandbox started in a namespace with a network policy of a different owner")
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
	if _, err := e2.m.Acquire(context.Background(), userRef("bob")); err == nil {
		t.Error("a sandbox started with a network policy that the configuration does not accept")
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
	if m.Ready() {
		t.Error("the manager is ready with no policy for the namespace")
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
