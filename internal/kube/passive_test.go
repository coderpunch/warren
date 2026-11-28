package kube

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/coderpunch/warren/internal/backend"
)

// These tests are for a passive request (backend.Request.Passive). Open
// WebUI reads the system information of a sandbox again each 5 seconds while
// a person has it open (docs/protocol.md, section 4.1.1). Such a request
// must get data only from a sandbox that operates. It must not start a
// sandbox, and it must not be a use of the sandbox.

// passive returns the passive request for the sandbox of a request.
func passive(req backend.Request) backend.Request {
	req.Passive = true
	return req
}

// soon returns a context with a time that is much longer than one step of
// Acquire. A request that must get its result immediately gets ErrNotReady
// with this context only if it waits.
func soon(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// leaseRecord is the lease data that warren keeps for one sandbox.
type leaseRecord struct {
	exists    bool
	leases    int
	last      time.Time
	coldStart bool
}

func (e *testEnv) leaseRecord(m *Manager, name string) leaseRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.sandboxes[name]
	if st == nil {
		return leaseRecord{}
	}
	return leaseRecord{exists: true, leases: st.leases, last: st.lastActivity, coldStart: st.coldStart}
}

// writes returns the number of requests that make an object.
func (e *testEnv) writes() int {
	n := 0
	for _, a := range e.client.Actions() {
		if a.GetVerb() == "create" {
			n++
		}
	}
	return n
}

// startNotReady starts a sandbox with a usual request and does not make its
// pod ready.
func (e *testEnv) startNotReady(req backend.Request) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := e.m.Acquire(ctx, req); !errors.Is(err, backend.ErrNotReady) {
		e.t.Fatalf("start of a sandbox whose pod is not ready: %v, want ErrNotReady", err)
	}
	name := req.Ref.Name()
	waitFor(e.t, "pod in the cache", func() bool { return e.m.pod(name) != nil })
}

// stillWaits makes sure that a call of Acquire did not return.
func stillWaits(t *testing.T, res <-chan acquireResult, what string) {
	t.Helper()
	select {
	case r := <-res:
		t.Fatalf("%s: the request did not wait: lease = %v, error = %v", what, r.lease != nil, r.err)
	case <-time.After(100 * time.Millisecond):
	}
}

// result returns the result of a call of Acquire that must end in a short
// time.
func result(t *testing.T, res <-chan acquireResult, what string) acquireResult {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: the request did not return", what)
	}
	return acquireResult{}
}

func TestPassiveRequestDoesNotStartASandbox(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxPerUser = 1 })
	// The policy "kept" has a volume. A start would make four objects.
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	before := e.writes()
	policies := len(e.networkPolicies())

	start := time.Now()
	lease, err := e.m.Acquire(soon(t), passive(req))
	if !errors.Is(err, backend.ErrStopped) || lease != nil {
		t.Fatalf("passive request with no pod: lease = %v, error = %v; want ErrStopped", lease != nil, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the request waited %s for a sandbox that does not start", d)
	}
	if n := e.writes() - before; n != 0 {
		t.Errorf("the passive request made %d objects", n)
	}
	if p, s, v, n := len(e.pods()), len(e.secrets()), len(e.volumes()), len(e.networkPolicies()); p != 0 || s != 0 || v != 0 || n != policies {
		t.Errorf("after the passive request: pods = %d, Secrets = %d, volumes = %d, network policies = %d; want 0, 0, 0, %d", p, s, v, n, policies)
	}
	if n := e.recordCount(); n != 0 {
		t.Errorf("the passive request left %d records in memory", n)
	}

	// A user at the limit gets the same reply, and not the error of the
	// limit: the request starts nothing.
	e.acquire(chatRef("alice", "c1")).Release()
	if _, err := e.m.Acquire(soon(t), chatRef("alice", "c2")); !errors.Is(err, backend.ErrLimit) {
		t.Fatalf("the test is wrong: second sandbox of alice: %v, want ErrLimit", err)
	}
	if _, err := e.m.Acquire(soon(t), passive(chatRef("alice", "c2"))); !errors.Is(err, backend.ErrStopped) {
		t.Errorf("passive request of a user at the limit: %v, want ErrStopped", err)
	}

	// A sandbox whose pods refused their key waits before its next start. A
	// passive request does not start it, thus it gets no error about the key.
	e.m.mu.Lock()
	e.m.badKeys[name] = badKey{count: maxBadKeys, last: e.clock.Now()}
	e.m.mu.Unlock()
	if _, err := e.m.Acquire(soon(t), req); err == nil || !strings.Contains(err.Error(), "refused its key") {
		t.Fatalf("the test is wrong: usual request in the wait time: %v", err)
	}
	if _, err := e.m.Acquire(soon(t), passive(req)); !errors.Is(err, backend.ErrStopped) {
		t.Errorf("passive request in the wait time after refused keys: %v, want ErrStopped", err)
	}
	if n := len(e.pods()); n != 1 {
		t.Errorf("pods = %d, want only the sandbox of the chat c1", n)
	}
}

// A passive request waits for a start that a different request began. Open
// WebUI stops its timer at each error status, thus an immediate error would
// stay on the screen after the sandbox is ready.
func TestPassiveRequestWhileThePodStarts(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.startNotReady(req)
	created := e.leaseRecord(e.m, name)
	if !created.coldStart || created.leases != 0 {
		t.Fatalf("the test is wrong: record after the start = %+v", created)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, passive(req))
	stillWaits(t, res, "pod that is not ready")
	e.makeReady(name)
	r := result(t, res, "pod that became ready")
	if r.err != nil {
		t.Fatalf("passive request after the pod became ready: %v", r.err)
	}
	if got := r.lease.Target(); got.URL.String() != "http://127.0.0.1:18000" || got.APIKey != string(e.secrets()[0].Data[secretKeyName]) {
		t.Errorf("target = %s, key agrees with the Secret: %v", got.URL, got.APIKey == string(e.secrets()[0].Data[secretKeyName]))
	}
	// The lease is not the first lease of the start, and it is not in the
	// lease count.
	if got := e.leaseRecord(e.m, name); got.leases != 0 || !got.coldStart || !got.last.Equal(created.last) {
		t.Errorf("record with an open passive lease = %+v, want %+v", got, created)
	}
	r.lease.Release()
	r.lease.Release() // a second call has no effect
	if got := e.leaseRecord(e.m, name); got.leases != 0 || !got.coldStart || !got.last.Equal(created.last) {
		t.Errorf("record after the passive lease = %+v, want %+v", got, created)
	}
	// The first usual lease is the first lease of the start.
	e.clock.Advance(time.Second)
	usual, err := e.m.Acquire(soon(t), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.leaseRecord(e.m, name); got.leases != 1 || got.coldStart || !got.last.Equal(e.clock.Now()) {
		t.Errorf("record with an open usual lease = %+v, want one lease, no cold start, and the time of this lease", got)
	}
	usual.Release()

	// At the end of the wait, the reply is that of a sandbox that is not
	// ready.
	bob := userRef("bob")
	e.startNotReady(bob)
	short, cancelShort := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelShort()
	if _, err := e.m.Acquire(short, passive(bob)); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("passive request at the end of the wait: %v, want ErrNotReady", err)
	}
	// A client that goes away gets its own error.
	gone, cancelGone := context.WithCancel(context.Background())
	res = e.acquireAsync(gone, e.m, passive(bob))
	stillWaits(t, res, "pod of bob that is not ready")
	cancelGone()
	if r := result(t, res, "client that went away"); !errors.Is(r.err, context.Canceled) {
		t.Errorf("passive request of a client that went away: %v, want context.Canceled", r.err)
	}
	if n := e.creates(); n != 2 {
		t.Errorf("pod create requests = %d, want 2: the passive requests start nothing", n)
	}
}

// The start request of a different caller can be in progress before the
// cache shows a pod. A passive request waits for it and sends no request of
// its own. When the start has no result, the sandbox is stopped.
func TestPassiveRequestWhileAStartRequestRuns(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	mark := func(change func(*state)) {
		e.m.mu.Lock()
		change(e.m.stateLocked(name))
		e.m.wakeLocked()
		e.m.mu.Unlock()
	}
	before := e.writes()

	// The start request runs.
	mark(func(st *state) { st.starting = true })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, passive(req))
	stillWaits(t, res, "start request in progress")
	// The start request fails. No pod comes.
	mark(func(st *state) { st.starting = false })
	if r := result(t, res, "start request that failed"); !errors.Is(r.err, backend.ErrStopped) {
		t.Errorf("passive request after a start that failed: %v, want ErrStopped", r.err)
	}

	// The start request is complete, and the cache does not show the pod.
	mark(func(st *state) { st.creating, st.createStart = true, e.clock.Now() })
	res = e.acquireAsync(ctx, e.m, passive(req))
	stillWaits(t, res, "pod that the cache does not show")
	// Warren waits for such a pod for a limited time. After that time, a
	// usual request starts the sandbox again, and a passive request gets the
	// reply of a stopped sandbox.
	e.clock.Advance(createStale)
	e.m.mu.Lock()
	e.m.wakeLocked()
	e.m.mu.Unlock()
	if r := result(t, res, "mark of a start that is too old"); !errors.Is(r.err, backend.ErrStopped) {
		t.Errorf("passive request with an old mark of a start: %v, want ErrStopped", r.err)
	}
	// A new request with the old mark gets the reply immediately.
	start := time.Now()
	if _, err := e.m.Acquire(soon(t), passive(req)); !errors.Is(err, backend.ErrStopped) || time.Since(start) > time.Second {
		t.Errorf("new passive request with an old mark of a start: %v after %s, want ErrStopped immediately", err, time.Since(start))
	}

	if n := e.writes() - before; n != 0 {
		t.Errorf("the passive requests made %d objects", n)
	}
	if n := len(e.pods()); n != 0 {
		t.Errorf("pods = %d, want 0", n)
	}
}

// A passive request that waits does not keep the pod, and it does not start
// the next pod when warren stops the one for which it waits.
func TestPassiveRequestThatWaitsDoesNotKeepThePod(t *testing.T) {
	logs := &syncBuffer{}
	e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	req := userRef("alice")
	name := req.Ref.Name()
	e.startNotReady(req)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, passive(req))
	stillWaits(t, res, "pod that is not ready")
	// The pod is not ready for more than the start time of 5 minutes.
	e.clock.Advance(6 * time.Minute)
	e.m.Reap(context.Background())
	if r := result(t, res, "stop of the pod"); !errors.Is(r.err, backend.ErrStopped) {
		t.Errorf("passive request after the stop of its pod: %v, want ErrStopped", r.err)
	}
	if !strings.Contains(logs.String(), "sandbox="+name+" reason=not_ready ") {
		t.Errorf("the reason of the stop is not not_ready: %s", logs.String())
	}
	waitFor(t, "end of the pod", func() bool { return e.m.pod(name) == nil })
	// A usual request that waits starts the next pod. A passive request does
	// not.
	time.Sleep(50 * time.Millisecond)
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1: the passive request started a pod", n)
	}
}

func TestPassiveRequestForAPodThatStops(t *testing.T) {
	stopped := func(t *testing.T, e *testEnv, req backend.Request, what string) {
		t.Helper()
		creates, deletes := e.creates(), e.podDeletes(req.Ref.Name())
		start := time.Now()
		lease, err := e.m.Acquire(soon(t), passive(req))
		if !errors.Is(err, backend.ErrStopped) || lease != nil {
			t.Errorf("%s: lease = %v, error = %v; want ErrStopped", what, lease != nil, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: the request waited %s", what, d)
		}
		if c, d := e.creates()-creates, e.podDeletes(req.Ref.Name())-deletes; c != 0 || d != 0 {
			t.Errorf("%s: the passive request sent %d create requests and %d delete requests for the pod", what, c, d)
		}
	}

	t.Run("pod in deletion", func(t *testing.T) {
		e := newTestEnv(t, nil)
		req := userRef("alice")
		e.previousPodInDeletion(req)
		stopped(t, e, req, "pod in its termination time")
	})
	t.Run("delete request of warren", func(t *testing.T) {
		e := newTestEnv(t, nil)
		req := userRef("alice")
		name := req.Ref.Name()
		e.acquire(req).Release()
		// Warren sent the delete request, and the cache does not show the
		// deletion time.
		e.m.mu.Lock()
		e.m.stateLocked(name).deleting = e.m.pod(name).UID
		e.m.mu.Unlock()
		stopped(t, e, req, "pod for which warren sent the delete request")
	})
	t.Run("final phase", func(t *testing.T) {
		for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
			e := newTestEnv(t, nil)
			req := userRef("alice")
			name := req.Ref.Name()
			e.startNotReady(req)
			e.setStatus(name, func(p *corev1.Pod) { p.Status.Phase = phase })
			waitFor(t, "final phase in the cache", func() bool { p := e.m.pod(name); return p != nil && podTerminal(p) })
			stopped(t, e, req, "pod in the phase "+string(phase))
			// The pod stays for the reaper.
			if n := len(e.pods()); n != 1 {
				t.Errorf("phase %s: pods = %d, want 1", phase, n)
			}
		}
	})
	t.Run("reset", func(t *testing.T) {
		e := newTestEnv(t, nil)
		req := userRef("alice")
		name := req.Ref.Name()
		e.acquire(req).Release()
		e.m.mu.Lock()
		e.m.stateLocked(name).resetting = true
		e.m.mu.Unlock()
		stopped(t, e, req, "reset in progress with a pod")
		// The control: with no reset, the same request gets a lease.
		e.m.mu.Lock()
		e.m.stateLocked(name).resetting = false
		e.m.mu.Unlock()
		lease, err := e.m.Acquire(soon(t), passive(req))
		if err != nil {
			t.Fatalf("passive request after the reset: %v", err)
		}
		lease.Release()

		bob := userRef("bob")
		e.m.mu.Lock()
		e.m.stateLocked(bob.Ref.Name()).resetting = true
		e.m.mu.Unlock()
		stopped(t, e, bob, "reset in progress with no pod")
	})
	t.Run("stop in the wait", func(t *testing.T) {
		e := newTestEnv(t, nil)
		req := userRef("alice")
		name := req.Ref.Name()
		e.startNotReady(req)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res := e.acquireAsync(ctx, e.m, passive(req))
		stillWaits(t, res, "pod that is not ready")
		if err := e.m.Stop(context.Background(), req.Ref); err != nil {
			t.Fatal(err)
		}
		if r := result(t, res, "stop operation"); !errors.Is(r.err, backend.ErrStopped) {
			t.Errorf("passive request after a stop operation: %v, want ErrStopped", r.err)
		}
		waitFor(t, "end of the pod", func() bool { return e.m.pod(name) == nil })
		time.Sleep(50 * time.Millisecond)
		if n := e.creates(); n != 1 {
			t.Errorf("pod create requests = %d, want 1", n)
		}
	})
}

// A passive request each 5 seconds, for more than the idle time, must not
// keep a sandbox. Before this rule, an open system information view kept
// its sandbox with no end.
func TestPassiveLeaseIsNotActivity(t *testing.T) {
	logs := &syncBuffer{}
	e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	ctx := context.Background()
	req := userRef("alice")
	name := req.Ref.Name()
	lease := e.acquire(req)
	e.clock.Advance(2 * time.Minute)
	lease.Release()
	used := e.clock.Now()
	// The reaper writes the time of the last use to the pod.
	e.m.Reap(ctx)
	want := used.Format(timeFormat)
	waitFor(t, "activity annotation", func() bool {
		p := e.m.pod(name)
		return p != nil && p.Annotations[AnnotationActivity] == want
	})
	patches := func() int {
		n := 0
		for _, a := range e.client.Actions() {
			if a.Matches("patch", "pods") {
				n++
			}
		}
		return n
	}
	written := patches()
	checks := func() int {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.verified)
	}
	keyChecks := checks()

	// The idle time is 30 minutes. One passive request each 5 seconds, and
	// one run of the reaper each 30 seconds. Each run has an open passive
	// lease.
	var open backend.Lease
	for step := 1; open == nil; step++ {
		e.clock.Advance(5 * time.Second)
		l, err := e.m.Acquire(ctx, passive(req))
		if err != nil {
			t.Fatalf("passive request %s after the last use: %v", e.clock.Now().Sub(used), err)
		}
		if got := e.leaseRecord(e.m, name); got.leases != 0 || !got.last.Equal(used) {
			t.Fatalf("record with an open passive lease = %+v, want no lease and the time %s", got, used)
		}
		if step%6 != 0 {
			l.Release()
			continue
		}
		if e.clock.Now().Sub(used) > 30*time.Minute {
			// The last run. The lease stays open in it.
			open = l
			continue
		}
		e.m.Reap(ctx)
		l.Release()
		if p := e.m.pod(name); p == nil || p.DeletionTimestamp != nil {
			t.Fatalf("the reaper stopped the sandbox %s after its last use, before the idle time", e.clock.Now().Sub(used))
		}
		if got := e.m.pod(name).Annotations[AnnotationActivity]; got != want {
			t.Fatalf("activity annotation = %s, want %s: a passive request changed the time of the last use", got, want)
		}
	}
	if n := patches() - written; n != 0 {
		t.Errorf("the reaper wrote the activity time %d times in the idle time", n)
	}
	if n := checks() - keyChecks; n != 0 {
		t.Errorf("the passive requests made %d key checks on a pod that warren examined before", n)
	}
	e.m.Reap(ctx)
	waitFor(t, "stop of the idle sandbox", func() bool { return e.m.pod(name) == nil })
	if !strings.Contains(logs.String(), "sandbox="+name+" reason=idle ") {
		t.Errorf("the reason of the stop is not idle: %s", logs.String())
	}
	// The lease of a sandbox that is gone.
	open.Release()
	e.m.Reap(ctx)
	if n := e.recordCount(); n != 0 {
		t.Errorf("records in memory after the stop = %d, want 0", n)
	}
	if _, err := e.m.Acquire(soon(t), passive(req)); !errors.Is(err, backend.ErrStopped) {
		t.Errorf("passive request after the idle stop: %v, want ErrStopped", err)
	}
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1", n)
	}
}

// A passive request can be the first request for a pod that this process did
// not examine: after a restart of warren, or after a start by the management
// API. No request goes to a pod before the key check.
func TestPassiveRequestChecksTheKeyOfANewPod(t *testing.T) {
	logs := &syncBuffer{}
	e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	req := userRef("alice")
	name := req.Ref.Name()
	first := e.acquire(req)
	key := first.Target().APIKey
	first.Release()
	checks := func() []backend.Target {
		e.mu.Lock()
		defer e.mu.Unlock()
		return append([]backend.Target(nil), e.verified...)
	}
	if n := len(checks()); n != 1 {
		t.Fatalf("the test is wrong: key checks after the first lease = %d", n)
	}

	// A new process, as after a restart of warren.
	second := e.newManager(e.m.cfg)
	lease, err := second.Acquire(soon(t), passive(req))
	if err != nil {
		t.Fatalf("passive request to a new process: %v", err)
	}
	if lease.Target().APIKey != key {
		t.Error("the lease does not have the key of the pod")
	}
	lease.Release()
	if got := checks(); len(got) != 2 || got[1].APIKey != key {
		t.Fatalf("key checks = %d, want 2 with the key of the pod", len(got))
	}
	if got := e.leaseRecord(second, name); got.leases != 0 || !got.last.IsZero() {
		t.Errorf("record of the new process = %+v, want no lease and no activity time", got)
	}
	// The second passive request does no check.
	lease, err = second.Acquire(soon(t), passive(req))
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if n := len(checks()); n != 2 {
		t.Errorf("key checks after the second passive request = %d, want 2", n)
	}

	// The pod refuses its key. Warren stops it, as for each request. The
	// passive request does not start the next pod.
	third := e.newManager(e.m.cfg)
	e.mu.Lock()
	e.badKeys = 1
	e.mu.Unlock()
	if _, err := third.Acquire(soon(t), passive(req)); !errors.Is(err, backend.ErrStopped) {
		t.Errorf("passive request to a pod that refuses its key: %v, want ErrStopped", err)
	}
	waitFor(t, "stop of the pod that refuses its key", func() bool { return len(e.pods()) == 0 })
	if !strings.Contains(logs.String(), "sandbox="+name+" reason=bad_key ") {
		t.Errorf("the reason of the stop is not bad_key: %s", logs.String())
	}
	time.Sleep(50 * time.Millisecond)
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1: the passive request started a pod", n)
	}
}

// The first request for a pod also writes the address of the node into the
// network policy of the pod. A passive request does the same.
func TestPassiveRequestDoesTheChecksOfAFirstLease(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.startNotReady(req)
	if e.refusesNode(req, publicNodeA) {
		t.Fatal("the test is wrong: the policy has the address of the node before the pod has a node")
	}
	e.setStatus(name, onPublicNode(publicNodeA))
	lease, err := e.m.Acquire(soon(t), passive(req))
	if err != nil {
		t.Fatalf("passive request as the first request for a pod: %v", err)
	}
	defer lease.Release()
	if !e.refusesNode(req, publicNodeA) {
		t.Error("the policy of the sandbox permits the address of its node after the passive request")
	}
	e.mu.Lock()
	n := len(e.verified)
	e.mu.Unlock()
	if n != 1 {
		t.Errorf("key checks = %d, want 1", n)
	}

	// A pod with a public address gets no request. Warren stops it, as for
	// each request.
	bob := userRef("bob")
	e.startNotReady(bob)
	var uid types.UID
	e.setStatus(bob.Ref.Name(), func(p *corev1.Pod) {
		readyStatus(p)
		p.Status.PodIP = "203.0.113.77"
		uid = p.UID
	})
	if _, err := e.m.Acquire(soon(t), passive(bob)); err == nil || errors.Is(err, backend.ErrStopped) || errors.Is(err, backend.ErrNotReady) ||
		!strings.Contains(err.Error(), "public address") {
		t.Errorf("passive request to a pod with a public address: %v, want the error about the address", err)
	}
	waitFor(t, "stop of the pod with a public address", func() bool { p := e.m.pod(bob.Ref.Name()); return p == nil || p.UID != uid })
	if n := e.creates(); n != 2 {
		t.Errorf("pod create requests = %d, want 2", n)
	}
}

// While warren is not ready, a passive request gets the reply of a sandbox
// that is not ready, with a pod and with no pod.
func TestPassiveRequestWhenWarrenIsNotReady(t *testing.T) {
	for state, set := range notReadyStates {
		t.Run(state, func(t *testing.T) {
			e := newTestEnv(t, nil)
			alice, bob := userRef("alice"), userRef("bob")
			e.acquire(alice).Release()
			set(e.m)
			for what, req := range map[string]backend.Request{"sandbox that operates": alice, "sandbox with no pod": bob} {
				if _, err := e.m.Acquire(soon(t), passive(req)); !errors.Is(err, backend.ErrNotReady) {
					t.Errorf("%s: passive request while warren is not ready: %v, want ErrNotReady", what, err)
				}
			}
			if got := e.leaseRecord(e.m, alice.Ref.Name()); got.leases != 0 {
				t.Errorf("leases = %d, want 0", got.leases)
			}
		})
	}
}
