package kube

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/coderpunch/warren/internal/backend"
)

// These tests are for the stop signal of a WebSocket lease
// (backend.Lease.Stopped). When warren stops a pod, or sees that a different
// actor deletes it, each WebSocket lease on that pod gets the signal and the
// reason. The proxy then closes the WebSocket with a close code that agrees
// with the reason (docs/policies.md, section 10.2).

var podResource = corev1.SchemeGroupVersion.WithResource("pods")

// socket returns the request of a WebSocket for the sandbox of a request.
func socket(req backend.Request) backend.Request {
	req.WebSocket = true
	return req
}

// hasSignal reports whether a lease has its stop signal.
func hasSignal(l backend.Lease) bool {
	select {
	case <-l.Stopped():
		return true
	default:
		return false
	}
}

// signal waits for the stop signal of a lease and returns the reason.
func signal(t *testing.T, l backend.Lease, what string) string {
	t.Helper()
	select {
	case <-l.Stopped():
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: the lease got no stop signal", what)
	}
	return l.StopReason()
}

// socketCount returns the number of WebSocket leases that warren has for a
// sandbox.
func (e *testEnv) socketCount(name string) int {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	if st := e.m.sandboxes[name]; st != nil {
		return len(st.sockets)
	}
	return 0
}

// terminationTime makes the simulated API server do for a pod what the API
// server of Kubernetes does: a delete request sets the deletion time, and the
// pod stays for its termination time. A test ends that time with
// endTermination. A delete request with the grace period 0 removes the pod
// immediately. A delete request with the UID of a different pod gets the
// status 409.
func (e *testEnv) terminationTime() {
	e.client.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		del := a.(k8stesting.DeleteAction)
		options := del.GetDeleteOptions()
		obj, err := e.client.Tracker().Get(podResource, testNamespace, del.GetName())
		if err != nil {
			return false, nil, nil
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		if pre := options.Preconditions; pre != nil && pre.UID != nil && *pre.UID != pod.UID {
			return true, nil, apierrors.NewConflict(corev1.Resource("pods"), pod.Name, errors.New("the UID in the precondition is not the UID of the object"))
		}
		if options.GracePeriodSeconds != nil && *options.GracePeriodSeconds == 0 {
			return false, nil, nil
		}
		if pod.DeletionTimestamp == nil {
			now := metav1.NewTime(e.clock.Now())
			pod.DeletionTimestamp = &now
			if err := e.client.Tracker().Update(podResource, pod, testNamespace); err != nil {
				return true, nil, err
			}
		}
		return true, nil, nil
	})
}

// inTermination waits until the cache shows the pod of a sandbox in its
// termination time.
func (e *testEnv) inTermination(name string) {
	e.t.Helper()
	waitFor(e.t, "pod in its termination time", func() bool {
		p := e.m.pod(name)
		return p != nil && p.DeletionTimestamp != nil
	})
}

// newPod waits until the cache shows a pod of the sandbox that is not the pod
// with the UID previous.
func (e *testEnv) newPod(name string, previous types.UID) {
	e.t.Helper()
	waitFor(e.t, "new pod", func() bool { p := e.m.pod(name); return p != nil && p.UID != previous })
}

// Each operation of the management API that stops a pod tells the WebSocket
// leases, with the reason "api", before the delete request goes to
// Kubernetes. The pod gets its stop signal from Kubernetes only after that
// request, thus the signal of warren always comes first.
func TestStopTellsTheWebSocketLeases(t *testing.T) {
	operations := map[string]func(e *testEnv, req backend.Request) error{
		"stop":    func(e *testEnv, req backend.Request) error { return e.m.Stop(context.Background(), req.Ref) },
		"restart": func(e *testEnv, req backend.Request) error { return e.m.Restart(context.Background(), req) },
		"reset":   func(e *testEnv, req backend.Request) error { return e.m.Reset(context.Background(), req.Ref) },
		"delete":  func(e *testEnv, req backend.Request) error { return e.m.Delete(context.Background(), req.Ref) },
	}
	for operation, do := range operations {
		t.Run(operation, func(t *testing.T) {
			e := newTestEnv(t, nil)
			// The policy "kept" has a volume, thus a reset and a delete also
			// remove a volume.
			req := templateRef("kept", "alice")
			name := req.Ref.Name()
			first := e.acquire(socket(req))
			second := e.acquire(socket(req))
			uid := e.m.pod(name).UID
			if hasSignal(first) || hasSignal(second) || first.StopReason() != "" {
				t.Fatal("a new lease has a stop signal")
			}
			if n := e.socketCount(name); n != 2 {
				t.Fatalf("WebSocket leases in the record = %d, want 2", n)
			}

			// atRequest tells if the leases had the signal when the delete
			// request for the pod came to the API server.
			var requests atomic.Int32
			var atRequest atomic.Bool
			e.client.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				if requests.Add(1) == 1 {
					atRequest.Store(hasSignal(first) && hasSignal(second))
				}
				return false, nil, nil
			})
			if err := do(e, req); err != nil {
				t.Fatalf("%s: %v", operation, err)
			}
			for _, l := range []backend.Lease{first, second} {
				if reason := signal(t, l, operation); reason != backend.StopAPI {
					t.Errorf("stop reason = %q, want %q", reason, backend.StopAPI)
				}
			}
			if requests.Load() == 0 || !atRequest.Load() {
				t.Errorf("delete requests = %d, leases had the signal at the first one: %v; want the signal before the request",
					requests.Load(), atRequest.Load())
			}

			// The leases end as usual. After that, warren has no record of
			// them.
			first.Release()
			second.Release()
			second.Release() // a second call has no effect
			if n := e.socketCount(name); n != 0 {
				t.Errorf("WebSocket leases in the record after the release = %d, want 0", n)
			}
			if got := e.leaseRecord(e.m, name); got.leases != 0 {
				t.Errorf("leases after the release = %d, want 0", got.leases)
			}
			if operation == "restart" {
				e.newPod(name, uid)
				// A lease on the new pod has no signal.
				next := e.acquire(socket(req))
				if hasSignal(next) {
					t.Error("a lease on the pod after the restart has a stop signal")
				}
				next.Release()
			}
		})
	}
}

// Each stop by the reaper tells the WebSocket leases the reason that is in
// the audit record and in the metrics.
func TestReaperStopsTellTheReason(t *testing.T) {
	cases := []struct {
		reason string
		req    backend.Request
		// change makes the condition for which the reaper stops the pod.
		change func(t *testing.T, e *testEnv, req backend.Request)
	}{
		{backend.StopNoPolicy, templateRef("quick", "alice"), func(t *testing.T, e *testEnv, _ backend.Request) {
			// The new file does not have the policy "quick".
			e.usePolicies("policies:\n  - {id: personal, name: Personal, scope: user, storage: {type: ephemeral}}\n")
		}},
		{backend.StopNoPolicy, chatKeptRef("alice", "c1"), func(t *testing.T, e *testEnv, _ backend.Request) {
			// The policy has a different scope.
			e.usePolicies("policies:\n  - {id: chat-kept, name: Was for each chat, scope: user, storage: {retention: 24h}}\n")
		}},
		{backend.StopNetworkChanged, templateRef("quick", "alice"), func(t *testing.T, e *testEnv, _ backend.Request) {
			e.usePolicies("policies:\n  - {id: quick, name: Quick, scope: user, storage: {type: ephemeral}, network: {egress: none}}\n")
		}},
		{"failed", userRef("alice"), func(t *testing.T, e *testEnv, req backend.Request) {
			name := req.Ref.Name()
			e.setStatus(name, func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
			waitFor(t, "final phase in the cache", func() bool { return podTerminal(e.m.pod(name)) })
		}},
		{backend.StopVolumeGone, templateRef("kept", "alice"), func(t *testing.T, e *testEnv, req backend.Request) {
			// A person deletes the volume. Kubernetes keeps it while the pod
			// uses it.
			name := req.Ref.Name()
			e.waitVolumeVersion(name)
			v := e.storedVolume(name)
			deleted := metav1.NewTime(e.clock.Now())
			v.DeletionTimestamp, v.Finalizers = &deleted, []string{protection}
			e.storeVolume(v)
			waitFor(t, "deletion of the volume in the cache", func() bool { c := e.m.volume(name); return c != nil && c.DeletionTimestamp != nil })
		}},
		{"not_ready", userRef("alice"), func(t *testing.T, e *testEnv, req backend.Request) {
			// The pod is not ready for more than the start time of 5 minutes.
			name := req.Ref.Name()
			e.setStatus(name, func(p *corev1.Pod) {
				p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
			})
			waitFor(t, "condition in the cache", func() bool { return !podReady(e.m.pod(name)) })
			e.clock.Advance(6 * time.Minute)
		}},
	}
	for _, c := range cases {
		t.Run(c.reason+" "+c.req.Ref.TemplateID, func(t *testing.T) {
			logs := &syncBuffer{}
			e := newTestEnv(t, func(cfg *Config) { cfg.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
			name := c.req.Ref.Name()
			lease := e.acquire(socket(c.req))
			defer lease.Release()
			// A run with no condition tells nothing.
			e.m.Reap(context.Background())
			if hasSignal(lease) {
				t.Fatal("the lease has a stop signal before the condition")
			}
			c.change(t, e, c.req)
			e.m.Reap(context.Background())
			if reason := signal(t, lease, c.reason); reason != c.reason {
				t.Errorf("stop reason = %q, want %q", reason, c.reason)
			}
			if !strings.Contains(logs.String(), "sandbox="+name+" reason="+c.reason+" ") {
				t.Errorf("the audit record does not have the reason %s: %s", c.reason, logs.String())
			}
		})
	}
}

// In this version an open WebSocket is a use of the sandbox: its lease is in
// the lease count. Thus the reaper does not stop the sandbox for its idle
// time, and the lease gets no signal with the reason "idle".
func TestOpenWebSocketKeepsTheSandbox(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	lease := e.acquire(socket(req))
	if got := e.leaseRecord(e.m, name); got.leases != 1 {
		t.Fatalf("leases with one open WebSocket = %d, want 1", got.leases)
	}
	e.clock.Advance(2 * time.Hour)
	e.m.Reap(context.Background())
	if hasSignal(lease) || e.podDeletes(name) != 0 {
		t.Fatalf("the reaper stopped a sandbox with an open WebSocket: signal = %v, delete requests = %d", hasSignal(lease), e.podDeletes(name))
	}
	// The end of the WebSocket is the last use.
	lease.Release()
	e.clock.Advance(31 * time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the idle sandbox", func() bool { return e.m.pod(name) == nil })
	if hasSignal(lease) {
		t.Error("a lease that ended got a stop signal")
	}
}

// A different actor can delete a pod: a person with kubectl, or Kubernetes
// when it drains a node. Warren sees that in the pod cache and tells the
// leases, with the reason "external".
func TestExternalDeleteTellsTheLeases(t *testing.T) {
	t.Run("the pod goes away", func(t *testing.T) {
		e := newTestEnv(t, nil)
		req := userRef("alice")
		lease := e.acquire(socket(req))
		defer lease.Release()
		if err := e.client.CoreV1().Pods(testNamespace).Delete(context.Background(), req.Ref.Name(), metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if reason := signal(t, lease, "delete by a different actor"); reason != backend.StopExternal {
			t.Errorf("stop reason = %q, want %q", reason, backend.StopExternal)
		}
	})
	t.Run("the pod gets a deletion time", func(t *testing.T) {
		logs := &syncBuffer{}
		e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
		e.terminationTime()
		req := userRef("alice")
		name := req.Ref.Name()
		lease := e.acquire(socket(req))
		defer lease.Release()
		if err := e.client.CoreV1().Pods(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		// The signal comes when the termination time starts, not when the
		// pod is gone.
		if reason := signal(t, lease, "deletion time from a different actor"); reason != backend.StopExternal {
			t.Errorf("stop reason = %q, want %q", reason, backend.StopExternal)
		}
		if len(e.pods()) != 1 {
			t.Fatal("the test is wrong: the pod is gone before the end of its termination time")
		}
		// The node does not confirm the deletion. The reaper removes the pod.
		// That is no new signal.
		e.clock.Advance(stuckMargin + time.Minute)
		e.m.Reap(context.Background())
		waitFor(t, "removal of the pod that stays in deletion", func() bool { return e.m.pod(name) == nil })
		if !strings.Contains(logs.String(), "sandbox="+name+" reason=stuck") {
			t.Errorf("the reaper did not remove the pod with the reason stuck: %s", logs.String())
		}
		if reason := lease.StopReason(); reason != backend.StopExternal {
			t.Errorf("stop reason after the removal = %q, want %q", reason, backend.StopExternal)
		}
	})
}

// A lease keeps its first stop reason. Warren deletes the pod, and the pod
// events of that deletion come later.
func TestFirstStopReasonStays(t *testing.T) {
	e := newTestEnv(t, nil)
	e.terminationTime()
	req := userRef("alice")
	name := req.Ref.Name()
	lease := e.acquire(socket(req))
	defer lease.Release()
	if err := e.m.Stop(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	if reason := signal(t, lease, "stop operation"); reason != backend.StopAPI {
		t.Fatalf("stop reason = %q, want %q", reason, backend.StopAPI)
	}
	// The event with the deletion time.
	e.inTermination(name)
	// The event of the end of the pod. The handler of that event removes the
	// delete mark of warren.
	e.endTermination(name)
	waitFor(t, "delete event", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		st := e.m.sandboxes[name]
		return st != nil && st.deleting == "" && e.m.pod(name) == nil
	})
	if reason := lease.StopReason(); reason != backend.StopAPI {
		t.Errorf("stop reason after the pod events = %q, want %q", reason, backend.StopAPI)
	}
}

// A lease is for one pod. The stop of a previous pod with the same name must
// not close a WebSocket to the pod that operates.
func TestLeaseOfADifferentPodGetsNoSignal(t *testing.T) {
	e := newTestEnv(t, nil)
	e.strictDeletes(podResource)
	req := userRef("alice")
	name := req.Ref.Name()
	// The relay to the first pod did not end when the second pod starts.
	old := e.acquire(socket(req))
	previous := e.m.pod(name).DeepCopy()
	e.stop(req)
	if reason := signal(t, old, "stop of the first pod"); reason != backend.StopAPI {
		t.Fatalf("stop reason of the first lease = %q, want %q", reason, backend.StopAPI)
	}
	current := e.acquire(socket(req))
	defer current.Release()
	if e.m.pod(name).UID == previous.UID {
		t.Fatal("the test is wrong: the second pod has the UID of the first pod")
	}
	if n := e.socketCount(name); n != 2 {
		t.Fatalf("WebSocket leases in the record = %d, want 2", n)
	}

	// A late event for the first pod.
	deleted := metav1.NewTime(e.clock.Now())
	late := previous.DeepCopy()
	late.DeletionTimestamp = &deleted
	e.m.podChanged(late, false)
	e.m.podChanged(late, true)
	if hasSignal(current) {
		t.Fatal("a pod event for the previous pod stopped the lease on the pod that operates")
	}
	// The reaper has an old copy of the first pod and sends its delete
	// request. Kubernetes refuses the request, because the UID is different.
	if err := e.m.deletePod(context.Background(), previous, "failed", false); err == nil {
		t.Error("the test is wrong: the delete request with the UID of the previous pod got no error")
	}
	if hasSignal(current) {
		t.Error("a delete request for the previous pod stopped the lease on the pod that operates")
	}
	if p := e.m.pod(name); p == nil || p.DeletionTimestamp != nil {
		t.Error("the pod that operates is gone")
	}
	// The first lease keeps its reason, and its end does not change the
	// second lease.
	old.Release()
	if old.StopReason() != backend.StopAPI || hasSignal(current) || e.socketCount(name) != 1 {
		t.Errorf("after the end of the first lease: reason = %q, signal of the second lease = %v, leases in the record = %d",
			old.StopReason(), hasSignal(current), e.socketCount(name))
	}
	// The stop of the second pod tells the second lease.
	e.stop(req)
	if reason := signal(t, current, "stop of the second pod"); reason != backend.StopAPI {
		t.Errorf("stop reason of the second lease = %q, want %q", reason, backend.StopAPI)
	}
}

// Only the lease of a WebSocket request gets the stop signal. An HTTP request
// in progress ends with the connection to its pod.
func TestRequestLeaseGetsNoStopSignal(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	usual := e.acquire(req)
	defer usual.Release()
	passiveLease, err := e.m.Acquire(soon(t), passive(req))
	if err != nil {
		t.Fatal(err)
	}
	defer passiveLease.Release()
	// A passive request is never a WebSocket request. If a caller sets the
	// two marks, the passive rule applies.
	both, err := e.m.Acquire(soon(t), passive(socket(req)))
	if err != nil {
		t.Fatal(err)
	}
	defer both.Release()
	if n := e.socketCount(name); n != 0 {
		t.Fatalf("WebSocket leases in the record = %d, want 0", n)
	}
	if got := e.leaseRecord(e.m, name); got.leases != 1 {
		t.Fatalf("leases = %d, want 1: only the usual request", got.leases)
	}
	e.stop(req)
	for what, l := range map[string]backend.Lease{"usual request": usual, "passive request": passiveLease, "passive WebSocket request": both} {
		if hasSignal(l) || l.StopReason() != "" {
			t.Errorf("%s: signal = %v, reason = %q; want no signal", what, hasSignal(l), l.StopReason())
		}
	}
}

// The signal goes before the delete request, and warren does not take it
// back. If Kubernetes refuses the request, the WebSockets close and the pod
// continues. A new connection gets a new lease on it.
func TestFailedDeleteRequestStillTellsTheLeases(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	lease := e.acquire(socket(req))
	defer lease.Release()
	uid := e.m.pod(name).UID
	var refuse atomic.Bool
	refuse.Store(true)
	e.client.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if refuse.Load() {
			return forbidden(a)
		}
		return false, nil, nil
	})
	if err := e.m.Stop(context.Background(), req.Ref); err == nil {
		t.Fatal("Stop = nil, want the error of the API")
	}
	if reason := signal(t, lease, "delete request that Kubernetes refuses"); reason != backend.StopAPI {
		t.Errorf("stop reason = %q, want %q", reason, backend.StopAPI)
	}
	if p := e.m.pod(name); p == nil || p.UID != uid || p.DeletionTimestamp != nil {
		t.Fatal("the pod is gone after a delete request that Kubernetes refused")
	}
	next, err := e.m.Acquire(soon(t), socket(req))
	if err != nil {
		t.Fatalf("new WebSocket request after the refused delete request: %v", err)
	}
	defer next.Release()
	if hasSignal(next) {
		t.Error("the new lease has the stop signal of the old lease")
	}
	// The next stop tells the new lease.
	refuse.Store(false)
	e.stop(req)
	if reason := signal(t, next, "second stop operation"); reason != backend.StopAPI {
		t.Errorf("stop reason of the new lease = %q, want %q", reason, backend.StopAPI)
	}
}

// This test records a fact, and not a requirement: a stop operation does not
// hold. A request that comes in the termination time of the pod waits for the
// end of the pod, and then starts the sandbox again. Open WebUI sends such
// requests by itself after each instance operation (docs/protocol.md, section
// 4.1.3). No close code can prevent that. A change that makes a stop hold
// must change this test.
func TestRequestInTheTerminationTimeOfAStopStartsANewPod(t *testing.T) {
	for kind, mark := range map[string]func(backend.Request) backend.Request{
		"usual request": func(r backend.Request) backend.Request { return r },
		"WebSocket":     socket,
	} {
		t.Run(kind, func(t *testing.T) {
			e := newTestEnv(t, nil)
			e.terminationTime()
			req := userRef("alice")
			name := req.Ref.Name()
			open := e.acquire(socket(req))
			defer open.Release()
			uid := e.m.pod(name).UID
			if err := e.m.Stop(context.Background(), req.Ref); err != nil {
				t.Fatal(err)
			}
			if reason := signal(t, open, "stop operation"); reason != backend.StopAPI {
				t.Fatalf("stop reason = %q, want %q", reason, backend.StopAPI)
			}
			e.inTermination(name)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res := e.acquireAsync(ctx, e.m, mark(req))
			stillWaits(t, res, "pod in the termination time of a stop")
			if n := e.creates(); n != 1 {
				t.Fatalf("pod create requests in the termination time = %d, want 1", n)
			}
			e.endTermination(name)
			e.newPod(name, uid)
			e.makeReady(name)
			r := result(t, res, "new pod after the stop")
			if r.err != nil {
				t.Fatalf("request that waited for the end of the stopped pod: %v", r.err)
			}
			defer r.lease.Release()
			if n := e.creates(); n != 2 {
				t.Errorf("pod create requests = %d, want 2: the request that waited starts the sandbox again", n)
			}
			if hasSignal(r.lease) {
				t.Error("the lease on the new pod has the stop signal of the previous pod")
			}
		})
	}
}
