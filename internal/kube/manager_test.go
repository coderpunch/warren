package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/scope"
)

const testNamespace = "sandboxes"

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

	cfg := Config{
		Client:       e.client,
		Namespace:    testNamespace,
		Image:        "example.test/terminal:1",
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
	waitFor(e.t, "start of the pod cache", m.Ready)
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
	if len(secrets) != 1 || secrets[0].Name != name {
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
	e.m.Stop(context.Background(), chatRef("alice", "c2").Ref)
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

func TestTotalLimit(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxTotal = 1 })
	e.acquire(userRef("alice")).Release()
	if _, err := e.m.Acquire(context.Background(), userRef("bob")); !errors.Is(err, backend.ErrLimit) {
		t.Errorf("second sandbox: %v, want ErrLimit", err)
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
	secrets := e.secrets()
	if len(secrets) != 1 || string(secrets[0].Data[secretKeyName]) != r.lease.Target().APIKey {
		t.Error("the lease does not have the key of the new pod")
	}
}

func TestSandboxWithNoSecretIsReplaced(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	// A new manager has no key in memory, and the Secret is gone.
	if err := e.client.CoreV1().Secrets(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
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
	if err := e.m.Start(req); err != nil {
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
	if err := e.m.Start(req); err != nil || len(e.pods()) != 1 {
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

	e.m.Stop(context.Background(), req.Ref)
	waitFor(t, "stop", func() bool { return e.m.pod(name) == nil })
	if s := e.m.InstanceStatus(req.Ref); s != StatusStopped {
		t.Errorf("status after stop = %s", s)
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
