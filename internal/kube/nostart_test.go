package kube

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/coderpunch/warren/internal/backend"
	"github.com/coderpunch/warren/internal/scope"
)

// These tests are for a request that does not begin a start
// (backend.Request.NoStart). The proxy sets the mark for the WebSocket
// routes on which a browser only watches a sandbox (docs/policies.md,
// section 10.3). The browser opens these WebSockets with no action of a
// person, and it connects again with no limit after most close codes
// (docs/protocol.md, section 4.2.3). Such a request must not start a
// sandbox. It waits for the start of a different request or operation, and
// in that wait it is not a use of the sandbox.

// watcher returns the request of a watcher WebSocket for the sandbox of a
// request.
func watcher(req backend.Request) backend.Request {
	req.WebSocket = true
	req.NoStart = true
	return req
}

// waits returns a call of Acquire that continues until the end of the test,
// or until it gets a result.
func (e *testEnv) waits(req backend.Request) <-chan acquireResult {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	e.t.Cleanup(cancel)
	return e.acquireAsync(ctx, e.m, req)
}

// notReadyAtTheEnd makes sure that a request waits for the full time of its
// context, and then gets the reply of a sandbox that is not ready.
func (e *testEnv) notReadyAtTheEnd(req backend.Request, what string) {
	e.t.Helper()
	const limit = 250 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	start := time.Now()
	lease, err := e.m.Acquire(ctx, req)
	if !errors.Is(err, backend.ErrNotReady) || lease != nil {
		e.t.Errorf("%s: lease = %v, error = %v; want ErrNotReady at the end of the wait", what, lease != nil, err)
	}
	if d := time.Since(start); d < limit {
		e.t.Errorf("%s: the request returned after %s, want a wait of %s", what, d, limit)
	}
}

func TestWatcherDoesNotStartASandbox(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxPerUser = 1 })
	// The policy "kept" has a volume. A start would make four objects.
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	before := e.writes()
	policies := len(e.networkPolicies())
	nothingMade := func(what string) {
		t.Helper()
		if n := e.writes() - before; n != 0 {
			t.Errorf("%s: the watcher made %d objects", what, n)
		}
		if p, s, v, n := len(e.pods()), len(e.secrets()), len(e.volumes()), len(e.networkPolicies()); p != 0 || s != 0 || v != 0 || n != policies {
			t.Errorf("%s: pods = %d, Secrets = %d, volumes = %d, network policies = %d; want 0, 0, 0, %d", what, p, s, v, n, policies)
		}
		if n := e.recordCount(); n != 0 {
			t.Errorf("%s: the watcher left %d records in memory", what, n)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := e.acquireAsync(ctx, e.m, watcher(req))
	stillWaits(t, res, "watcher for a sandbox with no pod")
	nothingMade("in the wait")
	// A run of the reaper finds nothing to do for a watcher that waits.
	e.m.Reap(context.Background())
	stillWaits(t, res, "watcher after a run of the reaper")
	nothingMade("after a run of the reaper")
	// A client that goes away gets its own error.
	cancel()
	if r := result(t, res, "client that went away"); !errors.Is(r.err, context.Canceled) || r.lease != nil {
		t.Errorf("watcher of a client that went away: lease = %v, error = %v; want context.Canceled", r.lease != nil, r.err)
	}
	// At the end of its time, the watcher gets the reply of a sandbox that is
	// not ready. The proxy then closes the WebSocket with the code 1013.
	e.notReadyAtTheEnd(watcher(req), "watcher for a sandbox with no pod")
	nothingMade("after the end of the wait")

	// The mark is not only for a WebSocket.
	plain := req
	plain.NoStart = true
	e.notReadyAtTheEnd(plain, "request that is not a WebSocket and does not begin a start")
	nothingMade("after a request that is not a WebSocket")

	// A sandbox whose pods refused their key waits before its next start. A
	// watcher does not start it, thus it gets no error about the key.
	e.m.mu.Lock()
	e.m.badKeys[name] = badKey{count: maxBadKeys, last: e.clock.Now()}
	e.m.mu.Unlock()
	if _, err := e.m.Acquire(soon(t), socket(req)); err == nil || !strings.Contains(err.Error(), "refused its key") {
		t.Fatalf("the test is wrong: usual WebSocket in the wait time: %v", err)
	}
	e.notReadyAtTheEnd(watcher(req), "watcher in the wait time after refused keys")
	nothingMade("after the wait time of the keys")

	// A user at the limit gets no error of the limit: the watcher starts
	// nothing.
	e.acquire(chatRef("alice", "c1")).Release()
	if _, err := e.m.Acquire(soon(t), socket(chatRef("alice", "c2"))); !errors.Is(err, backend.ErrLimit) {
		t.Fatalf("the test is wrong: second sandbox of alice: %v, want ErrLimit", err)
	}
	e.notReadyAtTheEnd(watcher(chatRef("alice", "c2")), "watcher of a user at the limit")
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want only the sandbox of the chat c1", n)
	}
	if n := e.recordCount(); n != 1 {
		t.Errorf("records in memory = %d, want only the sandbox of the chat c1", n)
	}
}

// Warren has its maximum number of sandboxes. A usual WebSocket gets the
// error of the capacity. A watcher starts nothing, thus it waits.
func TestWatcherWhenWarrenHasNoCapacity(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxTotal = 1 })
	e.acquire(userRef("alice")).Release()
	bob := userRef("bob")
	if _, err := e.m.Acquire(soon(t), socket(bob)); !errors.Is(err, backend.ErrCapacity) {
		t.Fatalf("the test is wrong: usual WebSocket with no capacity: %v, want ErrCapacity", err)
	}
	e.notReadyAtTheEnd(watcher(bob), "watcher with no capacity")
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1", n)
	}
}

// A watcher waits with no pod. When a different request starts the sandbox,
// the watcher gets the lease of a usual WebSocket on the new pod.
func TestWatcherJoinsTheStartOfADifferentRequest(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	first := e.waits(watcher(req))
	stillWaits(t, first, "watcher for a sandbox with no pod")
	if n := e.creates(); n != 0 {
		t.Fatalf("pod create requests in the wait = %d, want 0", n)
	}

	// A person uses the sandbox.
	usual := e.waits(req)
	waitFor(t, "pod in the cache", func() bool { return e.m.pod(name) != nil })
	created := e.leaseRecord(e.m, name)
	if !created.coldStart || created.leases != 0 {
		t.Fatalf("the test is wrong: record after the start = %+v", created)
	}
	// While the pod is not ready, the watcher has no lease, and it is not a
	// use of the sandbox.
	second := e.waits(watcher(req))
	e.clock.Advance(time.Minute)
	stillWaits(t, first, "watcher for a pod that is not ready")
	stillWaits(t, second, "second watcher for a pod that is not ready")
	if got := e.leaseRecord(e.m, name); got.leases != 0 || !got.coldStart || !got.last.Equal(created.last) {
		t.Errorf("record while two watchers wait = %+v, want %+v", got, created)
	}
	if n := e.socketCount(name); n != 0 {
		t.Errorf("WebSocket leases while two watchers wait = %d, want 0", n)
	}

	e.makeReady(name)
	var leases []backend.Lease
	for what, res := range map[string]<-chan acquireResult{"first watcher": first, "second watcher": second, "usual request": usual} {
		r := result(t, res, what)
		if r.err != nil {
			t.Fatalf("%s after the pod became ready: %v", what, r.err)
		}
		leases = append(leases, r.lease)
		if got := r.lease.Target(); got.URL.String() != "http://127.0.0.1:18000" || got.APIKey != string(e.secrets()[0].Data[secretKeyName]) {
			t.Errorf("%s: target = %s, key agrees with the Secret: %v", what, got.URL, got.APIKey == string(e.secrets()[0].Data[secretKeyName]))
		}
	}
	// The lease of a watcher is the lease of a usual WebSocket: its start is
	// a use of the sandbox, and it gets the stop signal. As each WebSocket
	// lease, it is not in the number of leases that keep the sandbox. Only
	// the usual request is.
	if got := e.leaseRecord(e.m, name); got.leases != 1 || got.coldStart || !got.last.Equal(e.clock.Now()) {
		t.Errorf("record with three open leases = %+v, want one lease that keeps the sandbox, no cold start, and the time of the leases", got)
	}
	if n := e.socketCount(name); n != 2 {
		t.Errorf("WebSocket leases = %d, want 2", n)
	}
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1", n)
	}

	// On a sandbox that operates, a watcher gets its lease immediately.
	e.clock.Advance(time.Minute)
	third, err := e.m.Acquire(soon(t), watcher(req))
	if err != nil {
		t.Fatalf("watcher for a sandbox that operates: %v", err)
	}
	if got := e.leaseRecord(e.m, name); got.leases != 1 || !got.last.Equal(e.clock.Now()) {
		t.Errorf("record after the lease of a new watcher = %+v, want one lease that keeps the sandbox and the time of this lease", got)
	}
	if n := e.socketCount(name); n != 3 {
		t.Errorf("WebSocket leases = %d, want 3", n)
	}
	leases = append(leases, third)

	if err := e.m.Stop(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	if reason := signal(t, third, "stop operation"); reason != backend.StopAPI {
		t.Errorf("stop reason of a watcher lease = %q, want %q", reason, backend.StopAPI)
	}
	for _, l := range leases {
		l.Release()
	}
	if got := e.leaseRecord(e.m, name); got.leases != 0 {
		t.Errorf("leases after the release = %d, want 0", got.leases)
	}
	if n := e.socketCount(name); n != 0 {
		t.Errorf("WebSocket leases after the release = %d, want 0", n)
	}
}

// A watcher that waits for a pod does not keep that pod, and it does not
// start the next pod when warren stops the one for which it waits.
func TestWatcherThatWaitsDoesNotKeepThePod(t *testing.T) {
	logs := &syncBuffer{}
	e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	req := userRef("alice")
	name := req.Ref.Name()
	e.startNotReady(req)
	created := e.leaseRecord(e.m, name)

	res := e.waits(watcher(req))
	stillWaits(t, res, "watcher for a pod that is not ready")
	// The pod is not ready for more than the start time of 5 minutes.
	e.clock.Advance(6 * time.Minute)
	if got := e.leaseRecord(e.m, name); got.leases != 0 || !got.last.Equal(created.last) {
		t.Errorf("record while the watcher waits = %+v, want no lease and the activity time %s", got, created.last)
	}
	e.m.Reap(context.Background())
	waitFor(t, "end of the pod", func() bool { return e.m.pod(name) == nil })
	if !strings.Contains(logs.String(), "sandbox="+name+" reason=not_ready ") {
		t.Errorf("the reason of the stop is not not_ready: %s", logs.String())
	}
	// A usual request that waits starts the next pod. A watcher does not.
	stillWaits(t, res, "watcher after the stop of its pod")
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1: the watcher started a pod", n)
	}
	// The record of the sandbox goes away while the watcher waits.
	e.m.Reap(context.Background())
	if n := e.recordCount(); n != 0 {
		t.Errorf("records in memory while the watcher waits for a sandbox with no pod = %d, want 0", n)
	}
	stillWaits(t, res, "watcher after the second run of the reaper")
}

// A usual request deletes a pod in a final phase, as the first step of the
// next start. A watcher sends no delete request: the reaper deletes the pod.
func TestWatcherLeavesAFailedPodToTheReaper(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			logs := &syncBuffer{}
			e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
			req := userRef("alice")
			name := req.Ref.Name()
			e.startNotReady(req)
			uid := e.m.pod(name).UID
			e.setStatus(name, func(p *corev1.Pod) { p.Status.Phase = phase })
			waitFor(t, "final phase in the cache", func() bool { p := e.m.pod(name); return p != nil && podTerminal(p) })

			res := e.waits(watcher(req))
			stillWaits(t, res, "watcher for a pod in a final phase")
			if n := e.podDeletes(name); n != 0 {
				t.Fatalf("pod delete requests of the watcher = %d, want 0", n)
			}
			if p := e.pods(); len(p) != 1 || p[0].UID != uid {
				t.Fatalf("pods = %d, want the pod in the final phase", len(p))
			}
			if strings.Contains(logs.String(), "sandbox deleted") {
				t.Errorf("warren wrote the record of a deleted sandbox: %s", logs.String())
			}

			// The reaper deletes the pod. The watcher continues to wait.
			e.m.Reap(context.Background())
			waitFor(t, "end of the pod", func() bool { return e.m.pod(name) == nil })
			if !strings.Contains(logs.String(), "sandbox="+name+" reason=failed ") {
				t.Errorf("the reason of the stop is not failed: %s", logs.String())
			}
			stillWaits(t, res, "watcher after the end of the pod")
			if c, d := e.creates(), e.podDeletes(name); c != 1 || d != 1 {
				t.Errorf("pod create requests = %d, pod delete requests = %d; want 1 and 1", c, d)
			}
		})
	}

	// The control: a usual WebSocket deletes the pod and starts the next one.
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.startNotReady(req)
	uid := e.m.pod(name).UID
	e.setStatus(name, func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
	waitFor(t, "final phase in the cache", func() bool { p := e.m.pod(name); return p != nil && podTerminal(p) })
	res := e.waits(socket(req))
	e.newPod(name, uid)
	if c, d := e.creates(), e.podDeletes(name); c != 2 || d != 1 {
		t.Errorf("usual WebSocket: pod create requests = %d, pod delete requests = %d; want 2 and 1", c, d)
	}
	e.makeReady(name)
	if r := result(t, res, "usual WebSocket after the new pod"); r.err == nil {
		r.lease.Release()
	}
}

// After a stop operation, the watchers of an open page connect again. They
// wait in the termination time of the pod, as each request. When the pod is
// gone, a usual request starts the sandbox again
// (TestRequestInTheTerminationTimeOfAStopStartsANewPod). A watcher does not.
func TestWatcherAfterAStopDoesNotStartTheSandboxAgain(t *testing.T) {
	e := newTestEnv(t, nil)
	e.terminationTime()
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	open, err := e.m.Acquire(soon(t), watcher(req))
	if err != nil {
		t.Fatal(err)
	}
	uid := e.m.pod(name).UID
	if err := e.m.Stop(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	if reason := signal(t, open, "stop operation"); reason != backend.StopAPI {
		t.Fatalf("stop reason = %q, want %q", reason, backend.StopAPI)
	}
	open.Release()
	e.inTermination(name)

	// The client connects again after 1 second.
	again := e.waits(watcher(req))
	stillWaits(t, again, "watcher in the termination time of a stop")
	e.endTermination(name)
	waitFor(t, "end of the pod", func() bool { return e.m.pod(name) == nil })
	stillWaits(t, again, "watcher after the end of the stopped pod")
	if n := e.creates(); n != 1 {
		t.Fatalf("pod create requests = %d, want 1: the watcher started the sandbox again", n)
	}
	e.m.Reap(context.Background())
	if n := e.recordCount(); n != 0 {
		t.Errorf("records in memory while the watcher waits = %d, want 0", n)
	}
	if n := len(e.pods()); n != 0 {
		t.Errorf("pods after the stop = %d, want 0", n)
	}

	// A person uses the sandbox. The watcher that waits gets its lease on the
	// new pod.
	usual := e.waits(req)
	e.newPod(name, uid)
	e.makeReady(name)
	for what, res := range map[string]<-chan acquireResult{"watcher": again, "usual request": usual} {
		r := result(t, res, what)
		if r.err != nil {
			t.Fatalf("%s after the new start: %v", what, r.err)
		}
		defer r.lease.Release()
		if hasSignal(r.lease) {
			t.Errorf("%s: the lease on the new pod has the stop signal of the previous pod", what)
		}
	}
	if n := e.creates(); n != 2 {
		t.Errorf("pod create requests = %d, want 2", n)
	}
}

// A reset or a delete operation removes the pod and the volume. A watcher
// waits during the operation, as each request, and it does not start the
// sandbox after the operation.
func TestWatcherDuringAReset(t *testing.T) {
	e := newTestEnv(t, nil)
	req := chatKeptRef("alice", "c1")
	name := req.Ref.Name()
	e.acquire(req).Release()
	uid := e.m.pod(name).UID
	e.waitVolumeVersion(name)
	old := e.volumeFromAPI(name).UID
	// Kubernetes keeps the volume until the test removes it.
	e.protect(name)

	done := make(chan error, 1)
	go func() { done <- e.m.Reset(context.Background(), req.Ref) }()
	waitFor(t, "volume in deletion", func() bool { v := e.volumeFromAPI(name); return v != nil && v.DeletionTimestamp != nil })
	res := e.waits(watcher(req))
	stillWaits(t, res, "watcher during a reset")
	if err := e.client.Tracker().Delete(volumeResource, testNamespace, name); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Reset = %v", err)
	}
	if v := e.volumeFromAPI(name); v == nil || v.UID == old {
		t.Fatalf("volume after the reset = %+v, want a new volume", v)
	}
	// The reset does not start the sandbox, and the watcher does not.
	stillWaits(t, res, "watcher after the reset")
	if n := e.creates(); n != 1 {
		t.Fatalf("pod create requests = %d, want 1: the watcher started the sandbox after the reset", n)
	}
	if n := len(e.pods()); n != 0 {
		t.Errorf("pods after the reset = %d, want 0", n)
	}

	// The next request of a person starts the sandbox with the empty volume.
	usual := e.waits(req)
	e.newPod(name, uid)
	e.makeReady(name)
	for what, res := range map[string]<-chan acquireResult{"watcher": res, "usual request": usual} {
		r := result(t, res, what)
		if r.err != nil {
			t.Fatalf("%s after the start: %v", what, r.err)
		}
		r.lease.Release()
	}
	if n := len(e.volumes()); n != 1 {
		t.Errorf("volumes = %d, want 1", n)
	}
}

// While a restart operation is in progress, a watcher waits for the start of
// the operation, as each request. It gets its lease on the new pod.
func TestWatcherDuringARestart(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxPerUser = 1 })
	e.terminationTime()
	team := scope.Ref{TemplateID: "team", Kind: scope.KindShared}
	alice := backend.Request{Ref: team, UserID: "alice"}
	bob := backend.Request{Ref: team, UserID: "bob"}
	name := team.Name()
	e.acquire(alice).Release()
	first := e.m.pod(name).UID
	e.acquire(userRef("bob")).Release() // bob is at his limit
	open, err := e.m.Acquire(soon(t), watcher(bob))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	// Open WebUI sends the ID of the manager for a shared sandbox.
	go func() { done <- e.m.Restart(ctx, backend.Request{Ref: team, UserID: "manager"}) }()
	if reason := signal(t, open, "restart operation"); reason != backend.StopAPI {
		t.Fatalf("stop reason = %q, want %q", reason, backend.StopAPI)
	}
	open.Release()
	e.inTermination(name)
	// The watcher of bob connects again.
	res := e.waits(watcher(bob))
	stillWaits(t, res, "watcher in the termination time of a restart")
	e.endTermination(name)
	if err := <-done; err != nil {
		t.Fatalf("Restart = %v", err)
	}
	e.newPod(name, first)
	stillWaits(t, res, "watcher for a new pod that is not ready")
	e.makeReady(name)
	r := result(t, res, "watcher after the restart")
	if r.err != nil {
		t.Fatalf("watcher after the restart: %v, want a lease", r.err)
	}
	defer r.lease.Release()
	if hasSignal(r.lease) {
		t.Error("the lease on the new pod has the stop signal of the previous pod")
	}
	pod := e.m.pod(name)
	if pod.Annotations[AnnotationCreatedBy] != "alice" || pod.Labels[LabelOwner] != ownerDigest("alice") {
		t.Errorf("after the restart, the sandbox is on the account of %q", pod.Annotations[AnnotationCreatedBy])
	}
	if n := e.creates(); n != 3 {
		t.Errorf("pod create requests = %d, want 3: two sandboxes and one restart", n)
	}
}

// A restart operation can end with no start, for example when its context
// ends. A usual request that waits then starts the sandbox
// (TestRequestDoesNotStartASandboxDuringARestart). A watcher does not.
func TestWatcherAfterARestartWithNoStart(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	// The restart operation deleted the pod, and it did not send its start
	// request.
	e.setRestarting(name, 1)
	res := e.waits(watcher(req))
	stillWaits(t, res, "watcher during a restart")
	e.setRestarting(name, 0)
	stillWaits(t, res, "watcher after a restart with no start")
	if n := e.creates(); n != 0 {
		t.Errorf("pod create requests = %d, want 0", n)
	}
	if _, ok := e.restartCount(name); ok {
		t.Error("the record of the sandbox stays after the restart, while only a watcher waits")
	}
}

// The first request for a pod makes the checks of a first lease, also when
// it is a watcher: after a restart of warren, the watchers of the open pages
// connect again first. A pod that refuses its key stops, as for each request.
// The watcher does not start the next pod.
func TestWatcherChecksTheKeyOfANewPod(t *testing.T) {
	logs := &syncBuffer{}
	e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	req := userRef("alice")
	name := req.Ref.Name()
	first := e.acquire(req)
	key := first.Target().APIKey
	first.Release()
	checks := func() int {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.verified)
	}
	if n := checks(); n != 1 {
		t.Fatalf("the test is wrong: key checks after the first lease = %d", n)
	}

	// A new process, as after a restart of warren.
	second := e.newManager(e.m.cfg)
	lease, err := second.Acquire(soon(t), watcher(req))
	if err != nil {
		t.Fatalf("watcher as the first request to a new process: %v", err)
	}
	if lease.Target().APIKey != key {
		t.Error("the lease does not have the key of the pod")
	}
	if n := checks(); n != 2 {
		t.Errorf("key checks = %d, want 2", n)
	}
	lease.Release()

	// The pod refuses its key.
	third := e.newManager(e.m.cfg)
	e.mu.Lock()
	e.badKeys = 1
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, third, watcher(req))
	waitFor(t, "stop of the pod that refuses its key", func() bool { return len(e.pods()) == 0 })
	if !strings.Contains(logs.String(), "sandbox="+name+" reason=bad_key ") {
		t.Errorf("the reason of the stop is not bad_key: %s", logs.String())
	}
	stillWaits(t, res, "watcher after the stop of a pod that refuses its key")
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1: the watcher started a pod", n)
	}
}

// While warren is not ready, a watcher gets the reply of a sandbox that is
// not ready immediately, with a pod and with no pod. It does not wait.
func TestWatcherWhenWarrenIsNotReady(t *testing.T) {
	for state, set := range notReadyStates {
		t.Run(state, func(t *testing.T) {
			e := newTestEnv(t, nil)
			alice, bob := userRef("alice"), userRef("bob")
			e.acquire(alice).Release()
			// A watcher of bob waits at the time at which warren becomes not
			// ready.
			res := e.waits(watcher(bob))
			stillWaits(t, res, "watcher for a sandbox with no pod")
			set(e.m)
			if r := result(t, res, "watcher that waits when warren becomes not ready"); !errors.Is(r.err, backend.ErrNotReady) {
				t.Errorf("watcher that waits when warren becomes not ready: %v, want ErrNotReady", r.err)
			}
			for what, req := range map[string]backend.Request{"sandbox that operates": alice, "sandbox with no pod": bob} {
				start := time.Now()
				if _, err := e.m.Acquire(soon(t), watcher(req)); !errors.Is(err, backend.ErrNotReady) {
					t.Errorf("%s: watcher while warren is not ready: %v, want ErrNotReady", what, err)
				}
				if d := time.Since(start); d > time.Second {
					t.Errorf("%s: the watcher waited %s while warren is not ready", what, d)
				}
			}
			if got := e.leaseRecord(e.m, alice.Ref.Name()); got.leases != 0 {
				t.Errorf("leases = %d, want 0", got.leases)
			}
			if n := e.creates(); n != 1 {
				t.Errorf("pod create requests = %d, want 1", n)
			}
		})
	}
}
