package kube

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/coderpunch/warren/internal/backend"
	"github.com/coderpunch/warren/internal/scope"
)

// These tests are for the mark of a restart operation. The stop of a restart
// closes the WebSockets of the sandbox, and their clients connect again in a
// short time. They wait in Acquire when the pod goes away. While the
// operation is in progress, only the operation starts the sandbox: a request
// that waits must not send the start request first, with its own user and
// with the limit of that user.

// restartCount returns the number of restart operations that warren records
// for a sandbox, and tells if the record exists.
func (e *testEnv) restartCount(name string) (int, bool) {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	st := e.m.sandboxes[name]
	if st == nil {
		return 0, false
	}
	return st.restarting, true
}

// setRestarting sets the mark of a restart operation and wakes the callers
// that wait.
func (e *testEnv) setRestarting(name string, n int) {
	e.m.mu.Lock()
	e.m.stateLocked(name).restarting = n
	e.m.dropLocked(name)
	e.m.wakeLocked()
	e.m.mu.Unlock()
}

// holdPodDelete stops the first delete request for the pod of a sandbox in
// the simulated API server. The channel entered closes when the request is
// there. The request continues when the test calls release. The simulated API
// server answers no other request in that time, thus the test must not use
// the client then.
func (e *testEnv) holdPodDelete(name string) (entered <-chan struct{}, release func()) {
	in, out := make(chan struct{}), make(chan struct{})
	var once, free sync.Once
	e.client.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.DeleteAction).GetName() == name {
			once.Do(func() {
				close(in)
				<-out
			})
		}
		return false, nil, nil
	})
	release = func() { free.Do(func() { close(out) }) }
	e.t.Cleanup(release)
	return in, release
}

// While a restart operation is in progress and the sandbox has no pod, a
// request does not start the sandbox. It waits for the start of the
// operation.
func TestRequestDoesNotStartASandboxDuringARestart(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxPerUser = 1 })
	team := scope.Ref{TemplateID: "team", Kind: scope.KindShared}
	bob := backend.Request{Ref: team, UserID: "bob"}
	name := team.Name()
	e.acquire(userRef("bob")).Release() // bob is at his limit
	creates := e.creates()

	// The restart operation deleted the pod, and it did not send its start
	// request.
	e.setRestarting(name, 1)
	for what, req := range map[string]backend.Request{"usual request": bob, "WebSocket": socket(bob)} {
		l, err := e.m.tryAcquire(context.Background(), name, req)
		if l != nil || err != nil {
			t.Errorf("%s during a restart: lease = %v, error = %v; want a wait", what, l != nil, err)
		}
	}
	// A run of the reaper keeps the mark, also while the sandbox has no pod
	// and no lease.
	e.m.Reap(context.Background())
	if n, ok := e.restartCount(name); !ok || n != 1 {
		t.Fatalf("mark after a run of the reaper = %d (record exists: %v), want 1", n, ok)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	usual := e.acquireAsync(ctx, e.m, bob)
	info := e.acquireAsync(ctx, e.m, passive(bob))
	stillWaits(t, usual, "usual request during a restart")
	stillWaits(t, info, "passive request during a restart")
	if n := e.creates() - creates; n != 0 {
		t.Fatalf("pod create requests during the restart = %d, want 0", n)
	}

	// The operation ends with no start, for example because its context
	// ended. The requests that wait then get the usual replies: a passive
	// request gets the reply of a stopped sandbox, and a usual request starts
	// the sandbox with its own user and the limit of that user.
	e.setRestarting(name, 0)
	if r := result(t, info, "passive request after a restart with no start"); !errors.Is(r.err, backend.ErrStopped) {
		t.Errorf("passive request after a restart with no start: %v, want ErrStopped", r.err)
	}
	if r := result(t, usual, "usual request after a restart with no start"); !errors.Is(r.err, backend.ErrLimit) {
		t.Errorf("request of a user at the limit after a restart with no start: %v, want ErrLimit", r.err)
	}
	if n := e.creates() - creates; n != 0 {
		t.Errorf("pod create requests = %d, want 0", n)
	}
	if _, ok := e.restartCount(name); ok {
		t.Error("the record of the sandbox stays after the restart, with no pod and no lease")
	}
	// A user below the limit starts the sandbox.
	e.acquire(backend.Request{Ref: team, UserID: "alice"}).Release()
}

// The clients of a sandbox connect again while a restart operation waits for
// the end of the pod. The operation must start the sandbox, and not a client:
// the sandbox stays on the account of the user that started it first, and the
// limit of the client user does not apply. Before the mark of the restart,
// the request of the client got the error of the limit in most runs of this
// test.
func TestRestartStartsTheSandboxAndNotARequestThatWaits(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxPerUser = 1 })
	team := scope.Ref{TemplateID: "team", Kind: scope.KindShared}
	alice := backend.Request{Ref: team, UserID: "alice"}
	bob := backend.Request{Ref: team, UserID: "bob"}
	name := team.Name()
	e.acquire(alice).Release()
	first := e.m.pod(name).UID
	e.acquire(userRef("bob")).Release() // bob is at his limit

	entered, release := e.holdPodDelete(name)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	// Open WebUI sends the ID of the manager for a shared sandbox.
	go func() { done <- e.m.Restart(ctx, backend.Request{Ref: team, UserID: "manager"}) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the restart sent no delete request for the pod")
	}
	// The WebSocket of bob closed, and his client connects again. The
	// requests wait for the end of the pod.
	var waiting []<-chan acquireResult
	for i := 0; i < 4; i++ {
		waiting = append(waiting, e.acquireAsync(ctx, e.m, socket(bob)))
	}
	stillWaits(t, waiting[0], "pod in deletion")
	// The pod goes away. The event wakes the operation and the requests at
	// the same time.
	release()
	if err := <-done; err != nil {
		t.Fatalf("Restart = %v", err)
	}
	e.newPod(name, first)
	e.makeReady(name)
	for _, res := range waiting {
		r := result(t, res, "request that waited for the restart")
		if r.err != nil {
			t.Fatalf("request that waited for the restart: %v, want a lease", r.err)
		}
		defer r.lease.Release()
	}
	pod := e.m.pod(name)
	if pod.Annotations[AnnotationCreatedBy] != "alice" || pod.Labels[LabelOwner] != ownerDigest("alice") {
		t.Errorf("after the restart, the sandbox is on the account of %q", pod.Annotations[AnnotationCreatedBy])
	}
	if n := e.creates(); n != 3 {
		t.Errorf("pod create requests = %d, want 3: two sandboxes and one restart", n)
	}
	if n, _ := e.restartCount(name); n != 0 {
		t.Errorf("mark after the restart = %d, want 0", n)
	}
}

// Two restart operations for one sandbox can run at the same time. The mark
// is a number: the end of the first operation must not remove the mark of the
// second one.
func TestTwoRestartOperationsAtTheSameTime(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	first := e.m.pod(name).UID

	entered, release := e.holdPodDelete(name)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- e.m.Restart(ctx, req) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the restart sent no delete request for the pod")
	}
	go func() { done <- e.m.Restart(ctx, req) }()
	waitFor(t, "mark of the second operation", func() bool { n, _ := e.restartCount(name); return n == 2 })
	// The second operation sets its mark and then reads the pod. Let it get
	// to its wait for the end of the pod.
	time.Sleep(50 * time.Millisecond)
	release()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("Restart = %v", err)
		}
	}
	e.newPod(name, first)
	if n := e.creates(); n != 2 {
		t.Errorf("pod create requests = %d, want 2: one start and one restart", n)
	}
	if n := e.podDeletes(name); n != 1 {
		t.Errorf("pod delete requests = %d, want 1", n)
	}
	if n, ok := e.restartCount(name); !ok || n != 0 {
		t.Errorf("mark after the two operations = %d (record exists: %v), want 0", n, ok)
	}
	e.acquire(req).Release()
}

// A passive request during a restart waits for the new pod. The sandbox
// starts again in a short time, thus the reply of a stopped sandbox is not
// correct here: Open WebUI stops its timer at that reply.
func TestPassiveRequestDuringARestart(t *testing.T) {
	e := newTestEnv(t, nil)
	e.terminationTime()
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	first := e.m.pod(name).UID
	last := e.leaseRecord(e.m, name).last

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.m.Restart(ctx, req) }()
	// The pod is in its termination time. With no restart, a passive request
	// gets the reply of a stopped sandbox here.
	e.inTermination(name)
	res := e.acquireAsync(ctx, e.m, passive(req))
	stillWaits(t, res, "pod in the termination time of a restart")
	if n := e.creates(); n != 1 {
		t.Fatalf("pod create requests in the termination time = %d, want 1", n)
	}
	e.endTermination(name)
	if err := <-done; err != nil {
		t.Fatalf("Restart = %v", err)
	}
	e.newPod(name, first)
	stillWaits(t, res, "new pod that is not ready")
	e.makeReady(name)
	r := result(t, res, "new pod after the restart")
	if r.err != nil {
		t.Fatalf("passive request after the restart: %v, want a lease", r.err)
	}
	r.lease.Release()
	if n := e.creates(); n != 2 {
		t.Errorf("pod create requests = %d, want 2: the passive request started nothing", n)
	}
	// The passive request is not a use of the sandbox.
	if got := e.leaseRecord(e.m, name); got.leases != 0 || !got.last.Equal(last) || !got.coldStart {
		t.Errorf("record after the passive request = %+v, want no lease, the activity time %s, and no first lease", got, last)
	}
}

// The mark goes away with each end of the operation, also with an error.
func TestRestartThatFailsRemovesItsMark(t *testing.T) {
	t.Run("Kubernetes refuses the delete request", func(t *testing.T) {
		e := newTestEnv(t, nil)
		req := userRef("alice")
		name := req.Ref.Name()
		e.acquire(req).Release()
		e.client.PrependReactor("delete", "pods", forbidden)
		if err := e.m.Restart(context.Background(), req); err == nil {
			t.Fatal("Restart = nil, want the error of the API")
		}
		if n, ok := e.restartCount(name); !ok || n != 0 {
			t.Errorf("mark after the error = %d (record exists: %v), want 0", n, ok)
		}
		// The pod operates, and a request gets a lease.
		lease, err := e.m.Acquire(soon(t), req)
		if err != nil {
			t.Fatalf("request after the restart that failed: %v", err)
		}
		lease.Release()
	})
	t.Run("the context of the operation ends", func(t *testing.T) {
		e := newTestEnv(t, nil)
		e.terminationTime()
		req := userRef("alice")
		name := req.Ref.Name()
		e.acquire(req).Release()
		first := e.m.pod(name).UID

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- e.m.Restart(ctx, req) }()
		e.inTermination(name)
		wait, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelWait()
		res := e.acquireAsync(wait, e.m, req)
		stillWaits(t, res, "pod in the termination time of a restart")
		// The client of the management API goes away.
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Restart = %v, want context.Canceled", err)
		}
		if n, ok := e.restartCount(name); !ok || n != 0 {
			t.Errorf("mark after the end of the context = %d (record exists: %v), want 0", n, ok)
		}
		// The request that waits starts the sandbox, as with no restart.
		e.endTermination(name)
		e.newPod(name, first)
		e.makeReady(name)
		r := result(t, res, "request after the restart that ended")
		if r.err != nil {
			t.Fatalf("request after the restart that ended: %v", r.err)
		}
		r.lease.Release()
	})
	t.Run("warren is not ready", func(t *testing.T) {
		e := newTestEnv(t, nil)
		req := userRef("alice")
		name := req.Ref.Name()
		e.acquire(req).Release()
		e.m.enforcement.Store(enforcementAbsent)
		if err := e.m.Restart(context.Background(), req); !errors.Is(err, backend.ErrNotReady) {
			t.Fatalf("Restart = %v, want ErrNotReady", err)
		}
		if n, _ := e.restartCount(name); n != 0 {
			t.Errorf("mark = %d, want 0", n)
		}
		if n := e.podDeletes(name); n != 0 {
			t.Errorf("pod delete requests = %d, want 0", n)
		}
	})
}
