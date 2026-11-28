package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/coderpunch/warren/internal/backend"
	"github.com/coderpunch/warren/internal/policy"
	"github.com/coderpunch/warren/internal/scope"
)

const testNamespace = "sandboxes"

// usePolicies replaces the policy file of the test.
func (e *testEnv) usePolicies(file string) {
	e.t.Helper()
	set, err := policy.Parse([]byte(file))
	if err != nil {
		e.t.Fatal(err)
	}
	e.m.cfg.Policies.(*policy.Store).Replace(set)
}

type testEnv struct {
	t      *testing.T
	client *fake.Clientset
	m      *Manager
	clock  *fakeClock

	mu       sync.Mutex
	verified []backend.Target
	// badKeys is the number of key checks that must fail.
	badKeys int
	uid     atomic.Int64
	// watchers is the number of managers that started a pod watch.
	watchers int
	// version is the last resource version that the simulated API server
	// gave to a volume or to a network policy.
	version atomic.Int64
	// conflicts is the number of volume requests that the simulated API
	// server refused with the status 409.
	conflicts atomic.Int64
	// policyConflicts is the number of update requests for a network policy
	// that the simulated API server refused with the status 409.
	policyConflicts atomic.Int64
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestEnv(t *testing.T, mod func(*Config)) *testEnv {
	t.Helper()
	e := &testEnv{t: t, clock: &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	e.client = fake.NewSimpleClientset()
	// The API server gives each object a UID and a creation time.
	stamp := func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject()
		meta := obj.(metav1.Object)
		meta.SetUID(types.UID(fmt.Sprintf("uid-%d", e.uid.Add(1))))
		meta.SetCreationTimestamp(metav1.NewTime(e.clock.Now()))
		return false, nil, nil
	}
	e.client.PrependReactor("create", "pods", stamp)
	e.client.PrependReactor("create", "secrets", stamp)
	e.client.PrependReactor("create", "persistentvolumeclaims", stamp)
	e.client.PrependReactor("create", "networkpolicies", stamp)
	e.strictVolumes()
	e.strictNetworkPolicies()

	cfg := Config{
		Client:    e.client,
		Namespace: testNamespace,
		Image:     "example.test/terminal:1",
		Policies:  policy.NewStore(testPolicies(t)),
		Network: NetworkConfig{
			WarrenNamespace: "warren-system",
			WarrenPodLabels: map[string]string{"app.kubernetes.io/name": "warren"},
		},
		CanaryPoll:   5 * time.Millisecond,
		CanaryPause:  5 * time.Millisecond,
		IdleTimeout:  30 * time.Minute,
		StartTimeout: 5 * time.Minute,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:          e.clock.Now,
		Port:         18000,
		Verify: func(_ context.Context, target backend.Target) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.badKeys > 0 {
				e.badKeys--
				return errBadKey
			}
			e.verified = append(e.verified, target)
			return nil
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	e.m = e.newManager(cfg)
	return e
}

func (e *testEnv) newManager(cfg Config) *Manager {
	e.t.Helper()
	m, err := New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	// Run the pod cache, but not the reaper timer. The tests call Reap.
	m.factory.Start(ctx.Done())
	// Run makes the network policy of the namespace before the first start.
	if err := m.ensureDefaultDeny(ctx); err != nil {
		e.t.Fatal(err)
	}
	// The tests of the pod lifecycle do not do the test of the network
	// plugin. The canary tests do it.
	m.enforcement.Store(enforcementProven)
	m.otherPolicies.Store(0)
	waitFor(e.t, "start of the pod cache", m.Ready)
	// The simulated cluster does not keep events for a watch that starts
	// later. Wait until the watch of this manager operates. A real API
	// server sends all events from the version of the first list.
	e.watchers++
	waitFor(e.t, "start of the pod watch", func() bool {
		n := 0
		for _, a := range e.client.Actions() {
			if a.Matches("watch", "pods") || a.Matches("watch", "persistentvolumeclaims") {
				n++
			}
		}
		return n >= 2*e.watchers
	})
	time.Sleep(20 * time.Millisecond)
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

// testPolicies returns the policies of the tests. The policies "personal"
// and "by-chat" keep no data, thus a test of the pod lifecycle makes no
// volume.
func testPolicies(t *testing.T) *policy.Set {
	t.Helper()
	set, err := policy.Parse([]byte(`
policies:
  - {id: personal, name: Personal, scope: user, storage: {type: ephemeral, size: 3Gi}}
  - {id: by-chat, name: Chat, scope: chat}
  - {id: kept, name: Kept, scope: user, storage: {size: 5Gi, storageClassName: fast, retention: 24h}}
  - {id: kept-forever, name: Kept with no end, scope: user}
  - {id: chat-kept, name: Chat with a volume, scope: chat, storage: {type: persistent, retention: 48h}}
  - {id: model-kept, name: Model with a volume, scope: model, storage: {type: persistent, retention: 48h}}
  - {id: team, name: Team, scope: shared}
  - {id: quick, name: Quick, scope: user, idleTimeout: 2m, storage: {type: ephemeral}}
  - {id: never-idle, name: Never idle, scope: user, idleTimeout: 0, storage: {type: ephemeral}}
  - {id: closed, name: Closed, scope: user, storage: {type: ephemeral}, network: {egress: none}}
  - id: database
    name: Database
    scope: user
    storage: {type: ephemeral}
    network:
      egress: none
      dns: true
      allow:
        - {cidr: 10.30.1.0/24, ports: [5432]}
        - {cidr: "2001:db8::/32"}
  - id: other-image
    name: Other image
    scope: user
    storage: {type: ephemeral}
    command: ["/sbin/tini", "--", "/usr/local/bin/python3", "-m", "open_terminal"]
    runAsUser: 1000
    userNamespace: true
  - id: tuned
    name: Tuned
    scope: user
    image: registry.example/tuned:2
    imagePullPolicy: Always
    imagePullSecrets: [pull-a, pull-b]
    runtimeClassName: gvisor
    resources:
      requests: {cpu: 250m, memory: 512Mi}
      limits: {cpu: 1500m, memory: 3Gi, ephemeral-storage: 1Gi}
    storage: {type: ephemeral, size: 2Gi}
    env: {ZED: last, ALPHA: first, OPEN_TERMINAL_EXECUTE_TIMEOUT: "60"}
    nodeSelector: {pool: sandboxes}
    tolerations:
      - {key: dedicated, operator: Equal, value: sandboxes, effect: NoSchedule}
`))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// policy returns the policy of a request.
func (e *testEnv) policy(req backend.Request) policy.Policy {
	e.t.Helper()
	p, ok := e.m.cfg.Policies.Get(req.Ref.TemplateID)
	if !ok {
		e.t.Fatalf("no policy %q", req.Ref.TemplateID)
	}
	return p
}

// templateRef returns a request for a user-scope template.
func templateRef(template, user string) backend.Request {
	return backend.Request{Ref: scope.Ref{TemplateID: template, Kind: scope.KindUser, Value: user}, UserID: user}
}

func userRef(user string) backend.Request {
	return backend.Request{Ref: scope.Ref{TemplateID: "personal", Kind: scope.KindUser, Value: user}, UserID: user}
}

func chatRef(user, chat string) backend.Request {
	return backend.Request{Ref: scope.Ref{TemplateID: "by-chat", Kind: scope.KindChat, Value: chat}, UserID: user}
}

func (e *testEnv) pods() []corev1.Pod {
	e.t.Helper()
	list, err := e.client.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return list.Items
}

func (e *testEnv) secrets() []corev1.Secret {
	e.t.Helper()
	list, err := e.client.CoreV1().Secrets(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return list.Items
}

// setStatus does the work of the kubelet for one pod.
func (e *testEnv) setStatus(name string, mod func(*corev1.Pod)) {
	e.t.Helper()
	pods := e.client.CoreV1().Pods(testNamespace)
	pod, err := pods.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	mod(pod)
	if _, err := pods.UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

func readyStatus(pod *corev1.Pod) {
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "127.0.0.1"
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
}

func (e *testEnv) makeReady(name string) {
	e.setStatus(name, readyStatus)
	waitFor(e.t, "pod ready in the cache", func() bool {
		p := e.m.pod(name)
		return p != nil && podReady(p)
	})
}

// acquireAsync calls Acquire in a goroutine.
func (e *testEnv) acquireAsync(ctx context.Context, m *Manager, req backend.Request) <-chan acquireResult {
	out := make(chan acquireResult, 1)
	go func() {
		l, err := m.Acquire(ctx, req)
		out <- acquireResult{l, err}
	}()
	return out
}

type acquireResult struct {
	lease backend.Lease
	err   error
}

// acquire starts a sandbox, makes it ready, and returns the lease.
func (e *testEnv) acquire(req backend.Request) backend.Lease {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, req)
	name := req.Ref.Name()
	waitFor(e.t, "pod "+name, func() bool { return e.m.pod(name) != nil })
	if !podReady(e.m.pod(name)) {
		e.makeReady(name)
	}
	r := <-res
	if r.err != nil {
		e.t.Fatalf("Acquire: %v", r.err)
	}
	return r.lease
}

func TestAcquireStartsASandbox(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	lease := e.acquire(req)
	defer lease.Release()

	secrets := e.secrets()
	if len(secrets) != 1 || !strings.HasPrefix(secrets[0].Name, name+"-") || secrets[0].Labels[LabelSandbox] != name {
		t.Fatalf("secrets = %v", secrets)
	}
	key := string(secrets[0].Data[secretKeyName])
	if len(key) < 40 {
		t.Errorf("the sandbox key has %d characters", len(key))
	}
	if refs := secrets[0].OwnerReferences; len(refs) != 1 || refs[0].Kind != "Pod" || refs[0].Name != name || refs[0].UID == "" {
		t.Errorf("the pod does not own its Secret: %v", refs)
	}

	target := lease.Target()
	if target.URL.String() != "http://127.0.0.1:18000" || target.APIKey != key {
		t.Errorf("target = %s, key matches the Secret: %v", target.URL, target.APIKey == key)
	}
	e.mu.Lock()
	if len(e.verified) != 1 || e.verified[0].APIKey != key {
		t.Errorf("key checks = %d, want 1 with the key of the Secret", len(e.verified))
	}
	e.mu.Unlock()

	pods := e.pods()
	if len(pods) != 1 {
		t.Fatalf("pods = %d", len(pods))
	}
	pod := pods[0]
	wantLabels := map[string]string{
		LabelManagedBy: "warren", LabelTemplate: "personal", LabelScope: "user", LabelOwner: ownerDigest("alice"),
	}
	for k, v := range wantLabels {
		if pod.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, pod.Labels[k], v)
		}
	}
	if pod.Annotations[AnnotationContext] != "user:alice" || pod.Annotations[AnnotationCreatedBy] != "alice" {
		t.Errorf("annotations = %v", pod.Annotations)
	}
	// A second lease uses the same pod and does no second key check.
	second, err := e.m.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.verified) != 1 || len(e.pods()) != 1 {
		t.Errorf("second lease: key checks = %d, pods = %d", len(e.verified), len(e.pods()))
	}
}

func TestPodSpecGivesNoAccessToTheCluster(t *testing.T) {
	e := newTestEnv(t, nil)
	e.acquire(userRef("alice")).Release()
	spec := e.pods()[0].Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the pod gets a service account token")
	}
	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Error("the pod gets the addresses of the services")
	}
	c := spec.Containers[0]
	if c.Image != "example.test/terminal:1" {
		t.Errorf("image = %q", c.Image)
	}
	env := map[string]corev1.EnvVar{}
	for _, v := range c.Env {
		env[v.Name] = v
	}
	if ref := env["OPEN_TERMINAL_API_KEY"].ValueFrom; ref == nil || ref.SecretKeyRef == nil || env["OPEN_TERMINAL_API_KEY"].Value != "" {
		t.Error("the key is not a reference to the Secret")
	}
	if env["OPEN_TERMINAL_ISOLATED"].Value != "true" || env["OPEN_TERMINAL_MULTI_USER"].Value != "false" {
		t.Errorf("env = %v", c.Env)
	}
	// The policy "personal" sets no limits, thus it has the limits of warren.
	for name, want := range map[string]string{
		"OPEN_TERMINAL_CPU_LIMIT": "2", "OPEN_TERMINAL_CPU_COUNT": "2",
		"OPEN_TERMINAL_MEMORY_LIMIT": "2Gi", "OPEN_TERMINAL_MEMORY_BYTES": "2147483648",
	} {
		if env[name].Value != want {
			t.Errorf("%s = %q, want %q", name, env[name].Value, want)
		}
	}
	if got := fmt.Sprint(c.Args); got != "[run --host 0.0.0.0 --port 8000]" {
		t.Errorf("args = %s", got)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/health" || c.StartupProbe == nil {
		t.Error("the pod has no readiness gate on the health route")
	}
}

func TestConcurrentRequestsStartOnePod(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var results []<-chan acquireResult
	for i := 0; i < 30; i++ {
		results = append(results, e.acquireAsync(ctx, e.m, req))
	}
	waitFor(t, "pod", func() bool { return e.m.pod(req.Ref.Name()) != nil })
	time.Sleep(50 * time.Millisecond)
	e.makeReady(req.Ref.Name())
	for _, res := range results {
		r := <-res
		if r.err != nil {
			t.Fatalf("Acquire: %v", r.err)
		}
		defer r.lease.Release()
	}
	creates := 0
	for _, a := range e.client.Actions() {
		if a.Matches("create", "pods") {
			creates++
		}
	}
	if creates != 1 || len(e.pods()) != 1 {
		t.Errorf("pod create requests = %d, pods = %d; want 1 and 1", creates, len(e.pods()))
	}
}

// A request with a short deadline must not stop the start of the sandbox.
func TestDeadlineDuringStart(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := e.m.Acquire(ctx, req); !errors.Is(err, backend.ErrNotReady) {
		t.Fatalf("Acquire with a short deadline = %v, want ErrNotReady", err)
	}
	if len(e.pods()) != 1 || len(e.secrets()) != 1 {
		t.Fatalf("after the deadline: pods = %d, secrets = %d", len(e.pods()), len(e.secrets()))
	}
	e.makeReady(req.Ref.Name())
	lease, err := e.m.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("Acquire after the start: %v", err)
	}
	lease.Release()

	// A client that goes away gets its own error.
	ctx, cancel = context.WithCancel(context.Background())
	res := e.acquireAsync(ctx, e.m, userRef("bob"))
	waitFor(t, "pod of bob", func() bool { return e.m.pod(userRef("bob").Ref.Name()) != nil })
	cancel()
	if r := <-res; !errors.Is(r.err, context.Canceled) {
		t.Errorf("Acquire after cancel = %v", r.err)
	}
}

func TestLimitForEachUser(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxPerUser = 2 })
	e.acquire(chatRef("alice", "c1")).Release()
	e.acquire(chatRef("alice", "c2")).Release()
	if _, err := e.m.Acquire(context.Background(), chatRef("alice", "c3")); !errors.Is(err, backend.ErrLimit) {
		t.Fatalf("third sandbox of alice: %v, want ErrLimit", err)
	}
	// A sandbox that exists is not a new sandbox.
	l, err := e.m.Acquire(context.Background(), chatRef("alice", "c1"))
	if err != nil {
		t.Fatalf("sandbox that exists: %v", err)
	}
	l.Release()
	// The limit is for each user.
	e.acquire(chatRef("bob", "c9")).Release()
	if len(e.pods()) != 3 {
		t.Errorf("pods = %d, want 3", len(e.pods()))
	}
	// A sandbox that stopped does not count.
	_ = e.m.Stop(context.Background(), chatRef("alice", "c2").Ref)
	waitFor(t, "stop of c2", func() bool { return e.m.pod(chatRef("alice", "c2").Ref.Name()) == nil })
	e.acquire(chatRef("alice", "c3")).Release()
}

// Start requests that the cache does not show must count for the limit.
func TestLimitCountsStartsInProgress(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxPerUser = 3 })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var results []<-chan acquireResult
	for i := 0; i < 20; i++ {
		results = append(results, e.acquireAsync(ctx, e.m, chatRef("alice", fmt.Sprintf("chat-%d", i))))
	}
	limited := 0
	for _, res := range results {
		if r := <-res; errors.Is(r.err, backend.ErrLimit) {
			limited++
		}
	}
	if n := len(e.pods()); n != 3 || limited != 17 {
		t.Errorf("pods = %d, refused = %d; want 3 and 17", n, limited)
	}
}

// The pod of a sandbox must not count for the limit of its own start. The
// cache can show the pod after the caller looked and before the start.
func TestLimitDoesNotCountTheSandboxItself(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxPerUser = 1 })
	req := userRef("alice")
	e.acquire(req).Release()
	if err := e.m.start(context.Background(), req, false); err != nil {
		t.Errorf("start of a sandbox that exists = %v, want no error", err)
	}
	if len(e.pods()) != 1 {
		t.Errorf("pods = %d, want 1", len(e.pods()))
	}
}

func TestTotalLimit(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxTotal = 1 })
	e.acquire(userRef("alice")).Release()
	// The user cannot correct this limit, thus the error is different.
	if _, err := e.m.Acquire(context.Background(), userRef("bob")); !errors.Is(err, backend.ErrCapacity) {
		t.Errorf("second sandbox: %v, want ErrCapacity", err)
	}
}

func TestReaperStopsIdleSandboxes(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	lease := e.acquire(req)

	// An open lease is use. The sandbox stays.
	e.clock.Advance(2 * time.Hour)
	e.m.Reap(context.Background())
	if len(e.pods()) != 1 {
		t.Fatal("the reaper stopped a sandbox that has an open lease")
	}
	lease.Release()
	lease.Release() // a second call has no effect

	e.clock.Advance(29 * time.Minute)
	e.m.Reap(context.Background())
	if len(e.pods()) != 1 {
		t.Fatal("the reaper stopped a sandbox before the idle timeout")
	}
	e.clock.Advance(2 * time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the idle sandbox", func() bool { return e.m.pod(name) == nil })

	// The fake cluster has no garbage collector. The reaper deletes the
	// Secret that has no pod.
	e.clock.Advance(3 * time.Minute)
	e.m.Reap(context.Background())
	if n := len(e.secrets()); n != 0 {
		t.Errorf("secrets after the stop = %d, want 0", n)
	}

	// The next request starts a new pod with a new key.
	e.acquire(req).Release()
	if len(e.pods()) != 1 {
		t.Error("no new pod after the stop")
	}
}

func TestReaperNeverStopsWithNoIdleTimeout(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.IdleTimeout = 0 })
	e.acquire(userRef("alice")).Release()
	e.clock.Advance(1000 * time.Hour)
	e.m.Reap(context.Background())
	if len(e.pods()) != 1 {
		t.Error("the reaper stopped a sandbox, but the idle timeout is off")
	}
}

func TestReaperDeletesPodsThatDoNotStart(t *testing.T) {
	e := newTestEnv(t, nil)
	// A pod that never becomes ready, for example with a bad image.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, _ = e.m.Acquire(ctx, userRef("alice"))
	cancel()
	e.clock.Advance(4 * time.Minute)
	e.m.Reap(context.Background())
	if len(e.pods()) != 1 {
		t.Fatal("the reaper deleted a pod before the start timeout")
	}
	e.clock.Advance(2 * time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "delete of the pod that did not start", func() bool { return len(e.pods()) == 0 })

	// A pod in a final phase.
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, _ = e.m.Acquire(ctx, userRef("bob"))
	cancel()
	bob := userRef("bob").Ref.Name()
	e.setStatus(bob, func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
	waitFor(t, "failed phase in the cache", func() bool { p := e.m.pod(bob); return p != nil && podTerminal(p) })
	e.m.Reap(context.Background())
	waitFor(t, "delete of the failed pod", func() bool { return len(e.pods()) == 0 })
}

// After a restart, warren must find its sandboxes and their keys again.
func TestNewManagerFindsSandboxes(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	first := e.acquire(req)
	key := first.Target().APIKey
	first.Release()

	e.clock.Advance(10 * time.Hour) // the first warren was stopped for a long time
	cfg := e.m.cfg
	second := e.newManager(cfg)
	lease, err := second.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Target().APIKey != key {
		t.Error("the new manager has a different key")
	}
	if len(e.pods()) != 1 {
		t.Errorf("pods = %d, want 1", len(e.pods()))
	}
	// The new manager must not stop a sandbox immediately after its start.
	lease.Release()
	second.Reap(context.Background())
	if len(e.pods()) != 1 {
		t.Error("the new manager stopped the sandbox immediately")
	}
	e.clock.Advance(31 * time.Minute)
	second.Reap(context.Background())
	waitFor(t, "stop by the new manager", func() bool { return len(e.pods()) == 0 })
}

// A pod that refuses its key has no use. Warren replaces it.
func TestPodWithWrongKeyIsReplaced(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.mu.Lock()
	e.badKeys = 1
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, req)
	waitFor(t, "first pod", func() bool { return e.m.pod(name) != nil })
	firstUID := e.m.pod(name).UID
	// The manager deletes the first pod immediately after the key check.
	e.setStatus(name, readyStatus)
	waitFor(t, "second pod", func() bool { p := e.m.pod(name); return p != nil && p.UID != firstUID })
	e.makeReady(name)
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.lease.Release()
	current, err := e.client.CoreV1().Secrets(testNamespace).Get(context.Background(), podSecretName(e.m.pod(name)), metav1.GetOptions{})
	if err != nil || string(current.Data[secretKeyName]) != r.lease.Target().APIKey {
		t.Errorf("the lease does not have the key of the new pod (%v)", err)
	}
}

func TestSandboxWithNoSecretIsReplaced(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	// A new manager has no key in memory, and the Secret is gone.
	if err := e.client.CoreV1().Secrets(testNamespace).Delete(context.Background(), e.secrets()[0].Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	second := e.newManager(e.m.cfg)
	firstUID := second.pod(name).UID
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, second, req)
	waitFor(t, "new pod", func() bool { p := second.pod(name); return p != nil && p.UID != firstUID })
	e.setStatus(name, readyStatus)
	if r := <-res; r.err != nil {
		t.Fatal(r.err)
	} else {
		r.lease.Release()
	}
}

func TestActivityIsWrittenToThePod(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	lease := e.acquire(req)
	e.clock.Advance(5 * time.Minute)
	e.m.Reap(context.Background())
	want := e.clock.Now().Format(timeFormat)
	waitFor(t, "activity annotation", func() bool {
		p, err := e.client.CoreV1().Pods(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
		return err == nil && p.Annotations[AnnotationActivity] == want
	})
	lease.Release()
}

func TestInstanceOperations(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	if s := e.m.InstanceStatus(req.Ref); s != StatusStopped {
		t.Errorf("status with no pod = %s", s)
	}
	if err := e.m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	if s := e.m.InstanceStatus(req.Ref); s != StatusStarting {
		t.Errorf("status before ready = %s", s)
	}
	e.makeReady(name)
	if s := e.m.InstanceStatus(req.Ref); s != StatusRunning {
		t.Errorf("status after ready = %s", s)
	}
	// A second start request has no effect.
	if err := e.m.Start(context.Background(), req); err != nil || len(e.pods()) != 1 {
		t.Errorf("second Start: %v, pods = %d", err, len(e.pods()))
	}
	e.acquire(chatRef("bob", "c1")).Release()
	list := e.m.Instances("personal")
	if len(list) != 1 || list[0].ID != name || list[0].ContextID != "user:alice" || list[0].Status != StatusRunning || list[0].CreatedBy != "alice" {
		t.Errorf("Instances(personal) = %+v", list)
	}

	firstUID := e.m.pod(name).UID
	if err := e.m.Restart(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "new pod after restart", func() bool { p := e.m.pod(name); return p != nil && p.UID != firstUID })

	if err := e.m.Stop(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stop", func() bool { return e.m.pod(name) == nil })
	if s := e.m.InstanceStatus(req.Ref); s != StatusStopped {
		t.Errorf("status after stop = %s", s)
	}
}

// holdPodEvent makes the simulated API server accept each delete request for
// the pod of a sandbox with no change of the pod. The request of warren is
// then complete, and the cache does not have the deletion time of the pod: on
// a cluster, the event with that time comes a short time after the reply.
// The function that it returns sends the event: the pod gets its deletion
// time. After that, the simulated API server handles a delete request for the
// pod as usual, and endTermination removes the pod.
func (e *testEnv) holdPodEvent(name string) (send func()) {
	var sent atomic.Bool
	e.client.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return !sent.Load() && a.(k8stesting.DeleteAction).GetName() == name, nil, nil
	})
	return func() {
		e.t.Helper()
		sent.Store(true)
		obj, err := e.client.Tracker().Get(podResource, testNamespace, name)
		if err != nil {
			e.t.Fatal(err)
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		now := metav1.NewTime(e.clock.Now())
		pod.DeletionTimestamp = &now
		if err := e.client.Tracker().Update(podResource, pod, testNamespace); err != nil {
			e.t.Fatal(err)
		}
		e.inTermination(name)
	}
}

// A stop operation sends the delete request for the pod. The cache gets the
// deletion time of the pod only with a later event. In that time the sandbox
// must have the status of a stopped sandbox: the management API reads the
// status immediately after the operation, and it answered "running" for a
// sandbox that it stopped.
func TestStatusAfterTheDeleteRequestOfWarren(t *testing.T) {
	cases := []struct {
		name string
		req  backend.Request
		// listed tells if the sandbox is in the instance list after the stop.
		// A stopped sandbox is in the list only if it has a volume.
		listed bool
	}{
		{"sandbox with a volume", templateRef("kept", "alice"), true},
		{"sandbox with no volume", userRef("alice"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t, nil)
			name := c.req.Ref.Name()
			e.acquire(c.req).Release()
			uid := e.m.pod(name).UID
			// stopped examines the two readers of the status.
			stopped := func(when string) {
				t.Helper()
				if s := e.m.InstanceStatus(c.req.Ref); s != StatusStopped {
					t.Errorf("%s: status = %q, want %q", when, s, StatusStopped)
				}
				list := e.m.Instances(c.req.Ref.TemplateID)
				switch {
				case c.listed && (len(list) != 1 || list[0].ID != name || list[0].Status != StatusStopped):
					t.Errorf("%s: instance list = %+v, want the sandbox with the status %q", when, list, StatusStopped)
				case !c.listed && len(list) != 0:
					t.Errorf("%s: instance list = %+v, want no instance", when, list)
				}
			}
			if s := e.m.InstanceStatus(c.req.Ref); s != StatusRunning {
				t.Fatalf("status before the stop = %q, want %q", s, StatusRunning)
			}
			if list := e.m.Instances(c.req.Ref.TemplateID); len(list) != 1 || list[0].Status != StatusRunning {
				t.Fatalf("instance list before the stop = %+v, want one instance with the status %q", list, StatusRunning)
			}

			send := e.holdPodEvent(name)
			if err := e.m.Stop(context.Background(), c.req.Ref); err != nil {
				t.Fatal(err)
			}
			if p := e.m.pod(name); p == nil || p.UID != uid || p.DeletionTimestamp != nil || !podReady(p) {
				t.Fatal("the test is wrong: the cache does not show the pod as it was before the delete request")
			}
			stopped("after the delete request, before its event")
			send()
			stopped("in the termination time")
			e.endTermination(name)
			waitFor(t, "end of the pod", func() bool { return e.m.pod(name) == nil })
			stopped("after the end of the pod")

			// The next pod of the sandbox does not have the status of the
			// previous one.
			e.acquire(c.req).Release()
			if s := e.m.InstanceStatus(c.req.Ref); s != StatusRunning {
				t.Errorf("status of the next pod = %q, want %q", s, StatusRunning)
			}
			if list := e.m.Instances(c.req.Ref.TemplateID); len(list) != 1 || list[0].Status != StatusRunning {
				t.Errorf("instance list with the next pod = %+v, want one instance with the status %q", list, StatusRunning)
			}
		})
	}

	// Kubernetes refuses the delete request. The pod continues, thus the
	// sandbox is not a stopped sandbox.
	t.Run("delete request that fails", func(t *testing.T) {
		e := newTestEnv(t, nil)
		req := userRef("alice")
		e.acquire(req).Release()
		e.client.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			return forbidden(a)
		})
		if err := e.m.Stop(context.Background(), req.Ref); err == nil {
			t.Fatal("Stop = nil, want the error of the API")
		}
		if s := e.m.InstanceStatus(req.Ref); s != StatusRunning {
			t.Errorf("status = %q, want %q", s, StatusRunning)
		}
		if list := e.m.Instances("personal"); len(list) != 1 || list[0].Status != StatusRunning {
			t.Errorf("instance list = %+v, want one instance with the status %q", list, StatusRunning)
		}
	})
}

// A start operation that comes after the delete request of warren, and before
// the event with the deletion time, must start the sandbox. The pod in the
// cache shows a sandbox that operates, but that pod stops. The operation waits
// for the end of the pod and then starts a new pod, as it does for a pod in
// deletion. Before this rule, the operation did nothing, and the sandbox
// stayed stopped.
func TestStartAfterTheDeleteRequestOfWarren(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	uid := e.m.pod(name).UID
	send := e.holdPodEvent(name)
	if err := e.m.Stop(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	if p := e.m.pod(name); p == nil || p.UID != uid || p.DeletionTimestamp != nil {
		t.Fatal("the test is wrong: the cache does not show the pod as it was before the delete request")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- e.m.Start(ctx, req) }()
	select {
	case err := <-started:
		t.Fatalf("Start returned before the end of the pod that stops: error = %v, pod create requests = %d", err, e.creates())
	case <-time.After(100 * time.Millisecond):
	}
	if n := e.creates(); n != 1 {
		t.Fatalf("pod create requests before the end of the pod = %d, want 1", n)
	}
	send()
	e.endTermination(name)
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the end of the pod")
	}
	e.newPod(name, uid)
	if n := e.creates(); n != 2 {
		t.Errorf("pod create requests = %d, want 2", n)
	}
	// The new pod stays on the account of the user that started the sandbox.
	if by := e.m.pod(name).Annotations[AnnotationCreatedBy]; by != "alice" {
		t.Errorf("user of the new pod = %q, want alice", by)
	}
}

// refusePodDelete stops the first delete request for the pod of a sandbox in
// the simulated API server: refer to holdPodDelete. When the test calls
// refuse, the simulated API server refuses that request. It handles each
// subsequent delete request as usual.
func (e *testEnv) refusePodDelete(name string) (entered <-chan struct{}, refuse func()) {
	var first atomic.Bool
	e.client.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.DeleteAction).GetName() == name && first.CompareAndSwap(false, true) {
			return forbidden(a)
		}
		return false, nil, nil
	})
	// This reactor is before the reactor above in the sequence.
	return e.holdPodDelete(name)
}

// A start operation that comes while the delete request of warren is in the
// API server waits for the end of the pod. If Kubernetes then refuses that
// request, the pod continues, and no event comes for it. The operation must
// see that the delete mark is gone, and it must return: the sandbox operates.
// Before this rule, the operation examined only the pod in the cache, and it
// waited until its context ended.
func TestStartWhenTheDeleteRequestOfWarrenFails(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	uid := e.m.pod(name).UID
	in, refuse := e.refusePodDelete(name)
	stopped := make(chan error, 1)
	go func() { stopped <- e.m.Stop(context.Background(), req.Ref) }()
	<-in

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- e.m.Start(ctx, req) }()
	select {
	case err := <-started:
		t.Fatalf("Start returned while the delete request was in the API server: error = %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	refuse()
	if err := <-stopped; err == nil {
		t.Fatal("Stop = nil, want the error of the API")
	}
	if err := <-started; err != nil {
		t.Fatalf("Start = %v for a sandbox whose pod continues", err)
	}
	if p := e.m.pod(name); p == nil || p.UID != uid || p.DeletionTimestamp != nil {
		t.Errorf("pod after the start operation = %+v, want the pod that continues", p)
	}
	if s := e.m.InstanceStatus(req.Ref); s != StatusRunning {
		t.Errorf("status = %q, want %q", s, StatusRunning)
	}
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1", n)
	}
	// The start operation does not send a delete request for a pod that
	// operates.
	if n := e.podDeletes(name); n != 1 {
		t.Errorf("pod delete requests = %d, want 1", n)
	}
}

// A restart operation that comes while the delete request of a different
// caller is in the API server sends no request, and it waits for the end of
// the pod. If Kubernetes then refuses that request, the pod continues, and no
// event comes for it. The operation must see that the delete mark is gone. It
// then sends its own delete request, and it starts the sandbox again. Before
// this rule, the operation examined only the pod in the cache: it waited
// until its context ended, and it stopped and started nothing.
func TestRestartWhenTheDeleteRequestOfADifferentCallerFails(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	uid := e.m.pod(name).UID
	in, refuse := e.refusePodDelete(name)
	stopped := make(chan error, 1)
	go func() { stopped <- e.m.Stop(context.Background(), req.Ref) }()
	<-in

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	restarted := make(chan error, 1)
	go func() { restarted <- e.m.Restart(ctx, req) }()
	select {
	case err := <-restarted:
		t.Fatalf("Restart returned while the delete request was in the API server: error = %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	refuse()
	if err := <-stopped; err == nil {
		t.Fatal("Stop = nil, want the error of the API")
	}
	if err := <-restarted; err != nil {
		t.Fatalf("Restart = %v after the delete request of the stop operation failed", err)
	}
	// The first delete request is that of the stop operation. The second
	// one is that of the restart operation.
	if n := e.podDeletes(name); n != 2 {
		t.Errorf("pod delete requests = %d, want 2", n)
	}
	if n := e.creates(); n != 2 {
		t.Errorf("pod create requests = %d, want 2", n)
	}
	e.newPod(name, uid)
	if by := e.m.pod(name).Annotations[AnnotationCreatedBy]; by != "alice" {
		t.Errorf("user of the new pod = %q, want alice", by)
	}
}

// Kubernetes refuses each delete request for the pod. A restart operation
// that waited for the request of a different caller sends its own request.
// That request fails also: the operation gets the error, it starts nothing,
// and the pod continues.
func TestRestartWhenEachDeleteRequestFails(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	uid := e.m.pod(name).UID
	e.client.PrependReactor("delete", "pods", forbidden)
	// This reactor is before the reactor above in the sequence.
	in, release := e.holdPodDelete(name)
	stopped := make(chan error, 1)
	go func() { stopped <- e.m.Stop(context.Background(), req.Ref) }()
	<-in

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	restarted := make(chan error, 1)
	go func() { restarted <- e.m.Restart(ctx, req) }()
	select {
	case err := <-restarted:
		t.Fatalf("Restart returned while the delete request was in the API server: error = %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-stopped; err == nil {
		t.Fatal("Stop = nil, want the error of the API")
	}
	if err := <-restarted; err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Restart = %v, want the error of the API", err)
	}
	if n := e.podDeletes(name); n != 2 {
		t.Errorf("pod delete requests = %d, want 2", n)
	}
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1", n)
	}
	if p := e.m.pod(name); p == nil || p.UID != uid || p.DeletionTimestamp != nil {
		t.Error("the old pod does not continue")
	}
}

// A restart operation sends the delete request for the pod. If the API server
// has no such pod, the cache is late. The operation waits for the cache, and
// it does not send the request again and again.
func TestRestartWhenTheCacheIsLate(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	e.client.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := e.m.Restart(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Restart = %v, want the end of its context", err)
	}
	if n := e.podDeletes(name); n != 1 {
		t.Errorf("pod delete requests = %d, want 1", n)
	}
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1", n)
	}
}

// A start operation sends the delete request for a pod in a final phase. If
// the API server has no such pod, the cache is late. The operation waits for
// the cache, and it does not send the request again and again.
func TestStartOfAFailedPodWhenTheCacheIsLate(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	e.setStatus(name, func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
	waitFor(t, "failed pod in the cache", func() bool { p := e.m.pod(name); return p != nil && podTerminal(p) })
	e.client.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := e.m.Start(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Start = %v, want the end of its context", err)
	}
	if n := e.podDeletes(name); n != 1 {
		t.Errorf("pod delete requests = %d, want 1", n)
	}
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1", n)
	}
}

// A request that waits in Acquire sends the delete request for a pod in a
// final phase. If that request fails, the request waits for the poll interval
// before the next step. The first sandbox of the test gets a refusal. The
// second one gets the reply "not found": the cache is late. A delete request
// that fails must not wake the callers that wait: the caller that sent it is
// one of them, and it then sends the request again with no pause.
func TestAcquireOfAFailedPodWhenTheDeleteRequestFails(t *testing.T) {
	e := newTestEnv(t, nil)
	refused, late := userRef("alice"), userRef("bob")
	for _, req := range []backend.Request{refused, late} {
		name := req.Ref.Name()
		e.acquire(req).Release()
		e.setStatus(name, func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
		waitFor(t, "failed pod in the cache", func() bool { p := e.m.pod(name); return p != nil && podTerminal(p) })
	}
	// The cache has each pod before the handler of its event runs, and each
	// handler wakes the requests that wait. A handler that runs after the
	// start of the two requests gives each of them one more step and one more
	// delete request. The handlers run in sequence. Thus wait for the handler
	// of a later event: the pod of a third sandbox goes away, and only that
	// handler gives the stop signal to the WebSocket lease on that pod.
	last := userRef("carol")
	l := e.acquire(socket(last))
	e.endTermination(last.Ref.Name())
	if reason := signal(t, l, "end of the pod of the third sandbox"); reason != backend.StopExternal {
		t.Fatalf("stop reason of the third sandbox = %q, want %q", reason, backend.StopExternal)
	}
	l.Release()
	e.client.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if name := a.(k8stesting.DeleteAction).GetName(); name == late.Ref.Name() {
			return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
		}
		return forbidden(a)
	})
	// The time of the wait has space for one poll interval. Each request
	// sends one delete request at its start and one after that interval.
	const wait = pollInterval + pollInterval/2
	limit := int(wait/pollInterval) + 1
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	results := []<-chan acquireResult{e.acquireAsync(ctx, e.m, refused), e.acquireAsync(ctx, e.m, late)}
	for i, req := range []backend.Request{refused, late} {
		if r := <-results[i]; !errors.Is(r.err, backend.ErrNotReady) {
			t.Errorf("Acquire for the sandbox of %s = %v, want ErrNotReady at the end of its context", req.UserID, r.err)
		}
		if n := e.podDeletes(req.Ref.Name()); n < 1 || n > limit {
			t.Errorf("pod delete requests for the sandbox of %s in %s = %d, want 1 to %d: one for each poll interval",
				req.UserID, wait, n, limit)
		}
	}
	// One pod for each of the three sandboxes: no request started a pod again.
	if n := e.creates(); n != 3 {
		t.Errorf("pod create requests = %d, want 3", n)
	}
}

func TestStartFailureIsReported(t *testing.T) {
	e := newTestEnv(t, nil)
	e.client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("pods"), "x", errors.New("quota"))
	})
	_, err := e.m.Acquire(context.Background(), userRef("alice"))
	if err == nil || errors.Is(err, backend.ErrNotReady) {
		t.Fatalf("Acquire = %v, want the API error", err)
	}
	// The Secret of the start that failed does not stay.
	if n := len(e.secrets()); n != 0 {
		t.Errorf("secrets = %d, want 0", n)
	}
}

func TestCounts(t *testing.T) {
	e := newTestEnv(t, nil)
	e.acquire(userRef("alice")).Release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, _ = e.m.Acquire(ctx, userRef("bob"))
	cancel()
	if ready, starting := e.m.counts(); ready != 1 || starting != 1 {
		t.Errorf("counts = %d ready, %d starting; want 1 and 1", ready, starting)
	}
}
