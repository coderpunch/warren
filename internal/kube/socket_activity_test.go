package kube

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stesting "k8s.io/client-go/testing"

	"github.com/coderpunch/warren/internal/backend"
	"github.com/coderpunch/warren/internal/policy"
)

// These tests are for the use of a sandbox through a WebSocket
// (docs/policies.md, section 10.4). An open WebSocket is not a use of the
// sandbox: a page that a person forgot must not keep a sandbox with no end.
// The start of the WebSocket lease is a use, and each message of the client
// that the proxy reports with backend.Lease.Active. The reaper stops a
// sandbox that had no use for its idle time, also with open WebSockets, and
// tells their leases. The WebSocket of an agent session is different
// (backend.Request.Hold): its open connection keeps the sandbox.

// held returns the request of a WebSocket whose open connection keeps the
// sandbox of a request.
func held(req backend.Request) backend.Request {
	req.WebSocket = true
	req.Hold = true
	return req
}

// podPatches returns the number of patch requests for a pod. Warren patches a
// pod only to write the activity time.
func (e *testEnv) podPatches(name string) int {
	n := 0
	for _, a := range e.client.Actions() {
		if a.Matches("patch", "pods") && a.(k8stesting.PatchAction).GetName() == name {
			n++
		}
	}
	return n
}

// activityAnnotation returns the activity time that the pod has in the API
// server.
func (e *testEnv) activityAnnotation(name string) string {
	e.t.Helper()
	p, err := e.client.CoreV1().Pods(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Annotations[AnnotationActivity]
}

// lastUse returns the time of the last use that warren has for a sandbox.
func (e *testEnv) lastUse(name string) time.Time {
	return e.leaseRecord(e.m, name).last
}

// hookedPolicies runs a function one time, before the next read of a policy.
// The reaper reads the policy of a pod after it read the time of the last use
// of that pod, and before it stops the pod. Thus the function runs between
// the decision of the reaper and its delete step.
type hookedPolicies struct {
	Policies
	hook atomic.Pointer[func()]
}

func (h *hookedPolicies) Get(id string) (policy.Policy, bool) {
	if f := h.hook.Swap(nil); f != nil {
		(*f)()
	}
	return h.Policies.Get(id)
}

// An open WebSocket with no message of its client does not keep the sandbox.
// The idle time counts from the start of the WebSocket. The reaper then stops
// the sandbox and tells the lease the reason "idle". This applies to a viewer
// route and to a watcher route.
func TestOpenWebSocketIsNotActivity(t *testing.T) {
	for class, mark := range map[string]func(backend.Request) backend.Request{"viewer": socket, "watcher": watcher} {
		t.Run(class, func(t *testing.T) {
			logs := &syncBuffer{}
			e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
			req := userRef("alice")
			name := req.Ref.Name()
			e.acquire(req).Release()
			e.clock.Advance(10 * time.Minute)
			opened := e.clock.Now()
			lease, err := e.m.Acquire(soon(t), mark(req))
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			// The start of the WebSocket is a use. The lease is in the record of
			// the WebSockets, and not in the number of leases that keep the
			// sandbox.
			if got := e.leaseRecord(e.m, name); got.leases != 0 || !got.last.Equal(opened) {
				t.Fatalf("record with one open WebSocket = %+v, want no lease that keeps the sandbox and the time %s", got, opened)
			}
			if n := e.socketCount(name); n != 1 {
				t.Fatalf("WebSocket leases in the record = %d, want 1", n)
			}

			e.clock.Advance(29 * time.Minute)
			e.m.Reap(context.Background())
			if hasSignal(lease) || e.podDeletes(name) != 0 {
				t.Fatalf("before the end of the idle time: signal = %v, delete requests = %d", hasSignal(lease), e.podDeletes(name))
			}
			// The reaper does not make an open WebSocket a use.
			if got := e.lastUse(name); !got.Equal(opened) {
				t.Errorf("time of the last use after a run of the reaper = %s, want %s", got, opened)
			}

			e.clock.Advance(2 * time.Minute)
			e.m.Reap(context.Background())
			if reason := signal(t, lease, "idle stop"); reason != backend.StopIdle {
				t.Errorf("stop reason = %q, want %q", reason, backend.StopIdle)
			}
			waitFor(t, "stop of the idle sandbox", func() bool { return e.m.pod(name) == nil })
			if !strings.Contains(logs.String(), "sandbox="+name+" reason=idle ") {
				t.Errorf("the audit record does not have the reason idle: %s", logs.String())
			}
		})
	}
}

// A message of the client on a WebSocket is a use: the idle time starts again
// at that message.
func TestWebSocketMessageIsActivity(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	lease := e.acquire(socket(req))
	defer lease.Release()

	e.clock.Advance(29 * time.Minute)
	lease.Active()
	used := e.clock.Now()
	if got := e.lastUse(name); !got.Equal(used) {
		t.Fatalf("time of the last use after a message = %s, want %s", got, used)
	}
	e.clock.Advance(29 * time.Minute)
	e.m.Reap(context.Background())
	if hasSignal(lease) || e.podDeletes(name) != 0 {
		t.Fatalf("29 minutes after a message: signal = %v, delete requests = %d", hasSignal(lease), e.podDeletes(name))
	}
	e.clock.Advance(2 * time.Minute)
	e.m.Reap(context.Background())
	if reason := signal(t, lease, "idle stop after the last message"); reason != backend.StopIdle {
		t.Errorf("stop reason = %q, want %q", reason, backend.StopIdle)
	}
	waitFor(t, "stop of the idle sandbox", func() bool { return e.m.pod(name) == nil })
}

// The proxy calls Active for each message. One lease writes the time of the
// last use one time in 10 seconds. The start of the lease is the first write.
func TestActiveWritesOneTimeInTenSeconds(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	// The lease keeps the time of its last write as the time after the start
	// of warren. Thus the lease must not start at the start of warren: a
	// lease with no first write would then have the correct value.
	e.clock.Advance(time.Hour)
	lease := e.acquire(socket(req))
	defer lease.Release()
	start := e.clock.Now()

	steps := []struct {
		advance time.Duration
		// want is the time of the last use after one call, as the time after
		// the start of the lease.
		want time.Duration
	}{
		// The first call comes after the start of the lease: the start is the
		// first write.
		{9 * time.Second, 0},
		{0, 0},
		{999 * time.Millisecond, 0},
		// 10 seconds after the start of the lease.
		{time.Millisecond, 10 * time.Second},
		{0, 10 * time.Second},
		{5 * time.Second, 10 * time.Second},
		{4 * time.Second, 10 * time.Second},
		// 10 seconds after the last write.
		{time.Second, 20 * time.Second},
		{time.Hour, time.Hour + 20*time.Second},
	}
	for i, step := range steps {
		e.clock.Advance(step.advance)
		lease.Active()
		if got := e.lastUse(name).Sub(start); got != step.want {
			t.Errorf("step %d: time of the last use = start + %s, want start + %s", i, got, step.want)
		}
	}

	// Each lease has its own interval. The start of a second WebSocket is a
	// use, and the first lease then does not write an earlier time.
	e.clock.Advance(3 * time.Second)
	second, err := e.m.Acquire(soon(t), socket(req))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	opened := e.clock.Now()
	lease.Active()
	if got := e.lastUse(name); !got.Equal(opened) {
		t.Errorf("time of the last use after the start of a second WebSocket = %s, want %s", got, opened)
	}
	e.clock.Advance(7 * time.Second)
	// The first call of the second lease, 7 seconds after its start: no
	// write.
	second.Active()
	if got := e.lastUse(name); !got.Equal(opened) {
		t.Errorf("time of the last use 7 seconds after the start of the second WebSocket = %s, want %s", got, opened)
	}
	lease.Active() // 10 seconds after its last write
	if got := e.lastUse(name); !got.Equal(e.clock.Now()) {
		t.Errorf("time of the last use = %s, want the time of the call of the first lease, %s", got, e.clock.Now())
	}
}

// steppedClock gives times as time.Now gives them: each time has a reading of
// the wall clock and a reading of the monotonic clock. Advance moves the two
// readings. back moves only the reading of the wall clock, as a step of the
// system clock does.
type steppedClock struct {
	mu      sync.Mutex
	base    time.Time
	elapsed time.Duration
	back    time.Duration
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return wallBack(c.base.Add(c.elapsed), c.back)
}

func (c *steppedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.elapsed += d
}

// stepBack moves the wall clock back, and not the monotonic clock.
func (c *steppedClock) stepBack(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.back += d
}

// wallBack returns a time from time.Now with its wall clock reading moved
// back by a whole number of seconds. The monotonic clock reading does not
// change. Only a step of the system clock gives such a time: the package time
// has no function that makes one, and a test cannot move the system clock.
// Thus this function writes the first word of the time.Time value through the
// package unsafe. In a time with a monotonic reading, that word has the wall
// seconds above its 30 bits of nanoseconds. This is the only use of unsafe in
// the tests, and it depends on the layout of time.Time: each test that uses
// the function must call wallBackOperates first.
func wallBack(t time.Time, back time.Duration) time.Time {
	*(*uint64)(unsafe.Pointer(&t)) -= uint64(back/time.Second) << 30
	return t
}

// wallBackOperates makes sure, with the functions of the package time only,
// that wallBack does its work with the Go release of this build. If a release
// changes time.Time, the test stops here with a clear cause.
func wallBackOperates(t *testing.T) {
	t.Helper()
	const back = 3 * time.Minute
	now := time.Now()
	stepped := wallBack(now, back)
	if got := now.UnixNano() - stepped.UnixNano(); got != int64(back) {
		t.Fatalf("wallBack does not operate with this Go release: it moved the wall clock by %s, want %s", time.Duration(got), back)
	}
	if d := stepped.Sub(now); d != 0 || !strings.Contains(stepped.String(), " m=") {
		t.Fatalf("wallBack does not operate with this Go release: the monotonic clock moved by %s (time %s)", d, stepped)
	}
}

// Active decides with the monotonic clock if 10 seconds went by, as the
// reaper measures the idle time with the monotonic clock. A step of the wall
// clock to an earlier time must not make Active ignore the messages for the
// time of the step: the reaper would then stop a sandbox that a person uses.
func TestStepOfTheWallClockDoesNotStopActivity(t *testing.T) {
	wallBackOperates(t)
	for name, back := range map[string]time.Duration{"no step": 0, "3 minutes back": 3 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			clock := &steppedClock{base: time.Now()}
			e := newTestEnv(t, func(c *Config) { c.Now = clock.Now })
			req := templateRef("quick", "alice") // idle time of 2 minutes
			sandbox := req.Ref.Name()
			lease := e.acquire(socket(req))
			defer lease.Release()

			// The client sends one message each 5 seconds, for one minute.
			for i := 0; i < 12; i++ {
				clock.Advance(5 * time.Second)
				lease.Active()
			}
			clock.stepBack(back)
			// The messages continue for 3 minutes. The reaper runs each 30
			// seconds.
			for i := 1; i <= 36; i++ {
				clock.Advance(5 * time.Second)
				lease.Active()
				if age := clock.Now().Sub(e.lastUse(sandbox)); age > activityInterval {
					t.Fatalf("%s after the step, with one message each 5 seconds: the last use is %s old, want a maximum of %s",
						time.Duration(i)*5*time.Second, age, activityInterval)
				}
				if i%6 == 0 {
					e.m.Reap(context.Background())
				}
				if hasSignal(lease) || e.podDeletes(sandbox) != 0 {
					t.Fatalf("%s after the step, with one message each 5 seconds: stop signal = %v (reason %q), delete requests = %d",
						time.Duration(i)*5*time.Second, hasSignal(lease), lease.StopReason(), e.podDeletes(sandbox))
				}
			}
		})
	}
}

// Active is only for the lease of a WebSocket. An HTTP request is a use from
// its start to its end, and a passive request is never a use.
func TestActiveIsOnlyForAWebSocketLease(t *testing.T) {
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
	start := e.clock.Now()
	e.clock.Advance(5 * time.Minute)
	usual.Active()
	passiveLease.Active()
	if got := e.lastUse(name); !got.Equal(start) {
		t.Errorf("time of the last use = %s, want %s: Active of a lease that is not a WebSocket lease wrote a time", got, start)
	}
}

// The reaper writes the time of the last use to the pod. For a WebSocket it
// is the time of the last message, and not the time of the run of the reaper.
// A WebSocket with no messages causes no write.
func TestSocketActivityIsWrittenToThePod(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	lease := e.acquire(socket(req))
	defer lease.Release()

	e.clock.Advance(5 * time.Minute)
	lease.Active()
	want := e.clock.Now().Format(timeFormat)
	// Active itself sends no request to Kubernetes.
	if n := e.podPatches(name); n != 0 {
		t.Fatalf("patch requests after Active = %d, want 0", n)
	}
	e.clock.Advance(30 * time.Second)
	e.m.Reap(context.Background())
	if got := e.activityAnnotation(name); got != want {
		t.Errorf("activity annotation = %q, want the time of the message, %q", got, want)
	}
	if n := e.podPatches(name); n != 1 {
		t.Fatalf("patch requests after the run of the reaper = %d, want 1", n)
	}

	// The WebSocket stays open with no message for 5 minutes.
	for i := 0; i < 5; i++ {
		e.clock.Advance(time.Minute)
		e.m.Reap(context.Background())
	}
	if n := e.podPatches(name); n != 1 {
		t.Errorf("patch requests with a silent WebSocket = %d, want 1: an open WebSocket is not a use", n)
	}
	if got := e.activityAnnotation(name); got != want {
		t.Errorf("activity annotation after 5 minutes with no message = %q, want %q", got, want)
	}
	if hasSignal(lease) {
		t.Error("the reaper stopped the sandbox before the end of its idle time")
	}
}

// The reaper reads the time of the last use, and then it stops the pod. A use
// between the two steps must prevent the stop: the delete step makes the
// decision again, with the time that the reaper read.
func TestIdleStopSeesActivityAfterTheDecision(t *testing.T) {
	cases := []struct {
		name string
		// open tells if a WebSocket is open before the run of the reaper.
		open bool
		// between runs after the decision of the reaper, and before its
		// delete step.
		between func(t *testing.T, e *testEnv, req backend.Request, open backend.Lease)
		stops   bool
	}{
		{"message on an open WebSocket", true, func(_ *testing.T, _ *testEnv, _ backend.Request, open backend.Lease) {
			open.Active()
		}, false},
		{"new WebSocket", false, func(t *testing.T, e *testEnv, req backend.Request, _ backend.Lease) {
			l, err := e.m.Acquire(soon(t), socket(req))
			if err != nil {
				t.Error(err)
				return
			}
			t.Cleanup(l.Release)
		}, false},
		{"new request that is in progress", false, func(t *testing.T, e *testEnv, req backend.Request, _ backend.Lease) {
			l, err := e.m.Acquire(soon(t), req)
			if err != nil {
				t.Error(err)
				return
			}
			t.Cleanup(l.Release)
		}, false},
		{"new request that is complete", false, func(t *testing.T, e *testEnv, req backend.Request, _ backend.Lease) {
			l, err := e.m.Acquire(soon(t), req)
			if err != nil {
				t.Error(err)
				return
			}
			l.Release()
		}, false},
		// These two are not a use, thus the stop continues.
		{"passive request", false, func(t *testing.T, e *testEnv, req backend.Request, _ backend.Lease) {
			l, err := e.m.Acquire(soon(t), passive(req))
			if err != nil {
				t.Error(err)
				return
			}
			l.Release()
		}, true},
		{"end of an open WebSocket", true, func(_ *testing.T, _ *testEnv, _ backend.Request, open backend.Lease) {
			open.Release()
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var policies *hookedPolicies
			e := newTestEnv(t, func(cfg *Config) {
				policies = &hookedPolicies{Policies: cfg.Policies}
				cfg.Policies = policies
			})
			req := userRef("alice")
			name := req.Ref.Name()
			e.acquire(req).Release()
			var open backend.Lease
			if c.open {
				var err error
				if open, err = e.m.Acquire(soon(t), socket(req)); err != nil {
					t.Fatal(err)
				}
				defer open.Release()
			}
			started := e.clock.Now()
			e.clock.Advance(31 * time.Minute)

			ran := false
			between := func() {
				ran = true
				// The reaper made its decision with the time of the start.
				if got := e.lastUse(name); !got.Equal(started) {
					t.Errorf("the test is wrong: time of the last use at the decision = %s, want %s", got, started)
				}
				c.between(t, e, req, open)
			}
			policies.hook.Store(&between)
			e.m.Reap(context.Background())
			if !ran {
				t.Fatal("the test is wrong: the reaper did not read the policy of the pod")
			}
			if c.stops {
				waitFor(t, "stop of the idle sandbox", func() bool { return e.m.pod(name) == nil })
				return
			}
			if n := e.podDeletes(name); n != 0 {
				t.Errorf("delete requests after a use between the decision and the delete step = %d, want 0", n)
			}
			if open != nil && hasSignal(open) {
				t.Error("the open WebSocket got the stop signal after a use")
			}
			if p := e.m.pod(name); p == nil || p.DeletionTimestamp != nil {
				t.Error("the pod is gone")
			}
		})
	}

	// The idle time counts from the message that came after the decision.
	t.Run("idle time after the message", func(t *testing.T) {
		var policies *hookedPolicies
		e := newTestEnv(t, func(cfg *Config) {
			policies = &hookedPolicies{Policies: cfg.Policies}
			cfg.Policies = policies
		})
		req := userRef("alice")
		lease := e.acquire(socket(req))
		defer lease.Release()
		e.clock.Advance(31 * time.Minute)
		between := func() { lease.Active() }
		policies.hook.Store(&between)
		e.m.Reap(context.Background())
		e.clock.Advance(29 * time.Minute)
		e.m.Reap(context.Background())
		if hasSignal(lease) {
			t.Fatal("the reaper stopped the sandbox 29 minutes after the message")
		}
		e.clock.Advance(2 * time.Minute)
		e.m.Reap(context.Background())
		if reason := signal(t, lease, "idle stop"); reason != backend.StopIdle {
			t.Errorf("stop reason = %q, want %q", reason, backend.StopIdle)
		}
	})

	// The delete step alone, with no run of the reaper.
	t.Run("delete step", func(t *testing.T) {
		e := newTestEnv(t, nil)
		req := userRef("alice")
		name := req.Ref.Name()
		lease := e.acquire(socket(req))
		defer lease.Release()
		decision := e.lastUse(name)
		e.clock.Advance(31 * time.Minute)
		lease.Active()
		pod := e.m.pod(name)
		if err := e.m.deletePod(context.Background(), pod, backend.StopIdle, decision); err != nil {
			t.Fatal(err)
		}
		if n := e.podDeletes(name); n != 0 || hasSignal(lease) {
			t.Fatalf("idle stop with a time before the last use: delete requests = %d, signal = %v; want 0 and no signal", n, hasSignal(lease))
		}
		// With the time of the last use, the same call stops the pod.
		if err := e.m.deletePod(context.Background(), pod, backend.StopIdle, e.lastUse(name)); err != nil {
			t.Fatal(err)
		}
		if reason := signal(t, lease, "idle stop with the time of the last use"); reason != backend.StopIdle {
			t.Errorf("stop reason = %q, want %q", reason, backend.StopIdle)
		}
		if n := e.podDeletes(name); n != 1 {
			t.Errorf("delete requests = %d, want 1", n)
		}
	})
}

// The end of a WebSocket is not a use. The end of an HTTP request is.
func TestReleaseOfAWebSocketIsNotActivity(t *testing.T) {
	e := newTestEnv(t, nil)
	alice, bob := userRef("alice"), userRef("bob")
	socketLease := e.acquire(socket(alice))
	requestLease := e.acquire(bob)
	start := e.clock.Now()

	e.clock.Advance(20 * time.Minute)
	socketLease.Release()
	requestLease.Release()
	if got := e.lastUse(alice.Ref.Name()); !got.Equal(start) {
		t.Errorf("time of the last use after the end of a WebSocket = %s, want the time of its start, %s", got, start)
	}
	if got := e.lastUse(bob.Ref.Name()); !got.Equal(e.clock.Now()) {
		t.Errorf("time of the last use after the end of a request = %s, want %s", got, e.clock.Now())
	}

	// 31 minutes after the start of the WebSocket, and 11 minutes after the
	// end of the request.
	e.clock.Advance(11 * time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the sandbox of the WebSocket", func() bool { return e.m.pod(alice.Ref.Name()) == nil })
	if p := e.m.pod(bob.Ref.Name()); p == nil || e.podDeletes(bob.Ref.Name()) != 0 {
		t.Error("the reaper stopped the sandbox of the request 11 minutes after the end of the request")
	}
}

// A lease is for one pod. A relay to a previous pod that did not end must not
// keep the next pod of the same sandbox. A lease that ended writes nothing.
func TestActiveOfAnOldLeaseDoesNotCountForTheNextPod(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	old := e.acquire(socket(req))
	first := e.m.pod(name).UID
	e.stop(req)
	if reason := signal(t, old, "stop of the first pod"); reason != backend.StopAPI {
		t.Fatalf("stop reason = %q, want %q", reason, backend.StopAPI)
	}

	e.clock.Advance(time.Hour)
	e.acquire(req).Release()
	if e.m.pod(name).UID == first {
		t.Fatal("the test is wrong: the second pod has the UID of the first pod")
	}
	started := e.clock.Now()
	e.clock.Advance(time.Minute)
	old.Active()
	if got := e.lastUse(name); !got.Equal(started) {
		t.Errorf("time of the last use after Active of a lease on the previous pod = %s, want %s", got, started)
	}
	old.Release()
	if got := e.lastUse(name); !got.Equal(started) {
		t.Errorf("time of the last use after the end of a lease on the previous pod = %s, want %s", got, started)
	}

	// A lease on the pod that operates, after its end.
	current, err := e.m.Acquire(soon(t), socket(req))
	if err != nil {
		t.Fatal(err)
	}
	opened := e.clock.Now()
	current.Release()
	e.clock.Advance(time.Minute)
	current.Active()
	if got := e.lastUse(name); !got.Equal(opened) {
		t.Errorf("time of the last use after Active of a lease that ended = %s, want %s", got, opened)
	}
}

// Warren keeps the record of a sandbox while a WebSocket lease is open, also
// when the sandbox has no pod: the relay did not end. The record goes away
// with the last lease.
func TestStateStaysWhileAWebSocketLeaseIsOpen(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	lease := e.acquire(socket(req))
	e.stop(req)
	waitFor(t, "end of the delete mark", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		st := e.m.sandboxes[name]
		return st != nil && st.deleting == ""
	})
	// A run of the reaper removes each record that has nothing behind it.
	e.m.Reap(context.Background())
	if got := e.leaseRecord(e.m, name); !got.exists || got.leases != 0 {
		t.Fatalf("record with an open WebSocket lease and no pod = %+v, want a record with no lease that keeps the sandbox", got)
	}
	if n := e.socketCount(name); n != 1 {
		t.Fatalf("WebSocket leases in the record = %d, want 1", n)
	}
	lease.Release()
	if n := e.recordCount(); n != 0 {
		t.Errorf("records after the end of the lease = %d, want 0", n)
	}
}

// The WebSocket of an agent session keeps its sandbox while it is open, as an
// HTTP request in progress does: the Open WebUI backend sends no other data
// to the sandbox while an agent works. A WebSocket with no such mark, which
// is open for the same time with no use message, does not keep its sandbox.
func TestHeldWebSocketKeepsTheSandbox(t *testing.T) {
	e := newTestEnv(t, nil)
	alice, bob := userRef("alice"), userRef("bob")
	agent := e.acquire(held(alice))
	terminal := e.acquire(socket(bob))
	defer terminal.Release()
	name := alice.Ref.Name()
	// The lease is in the two records: it keeps the sandbox, and it gets the
	// stop signal.
	if got := e.leaseRecord(e.m, name); got.leases != 1 {
		t.Fatalf("leases that keep the sandbox = %d, want 1", got.leases)
	}
	if n := e.socketCount(name); n != 1 {
		t.Fatalf("WebSocket leases in the record = %d, want 1", n)
	}

	// The open connection is the use. A message on it writes no time.
	opened := e.clock.Now()
	e.clock.Advance(time.Minute)
	agent.Active()
	if got := e.lastUse(name); !got.Equal(opened) {
		t.Errorf("time of the last use after Active of an agent WebSocket = %s, want the time of its start, %s", got, opened)
	}
	e.clock.Advance(2 * time.Hour)
	e.m.Reap(context.Background())
	if hasSignal(agent) || e.podDeletes(name) != 0 {
		t.Fatalf("the reaper stopped a sandbox with an open agent WebSocket: signal = %v, delete requests = %d", hasSignal(agent), e.podDeletes(name))
	}
	if reason := signal(t, terminal, "idle stop of the sandbox with a usual WebSocket"); reason != backend.StopIdle {
		t.Errorf("stop reason of the usual WebSocket = %q, want %q", reason, backend.StopIdle)
	}
	// While the WebSocket is open, each run of the reaper records a use.
	if got := e.lastUse(name); !got.Equal(e.clock.Now()) {
		t.Errorf("time of the last use with an open agent WebSocket = %s, want the time of the run, %s", got, e.clock.Now())
	}

	// The end of the WebSocket is the last use.
	e.clock.Advance(10 * time.Minute)
	agent.Release()
	agent.Release() // a second call has no effect
	if got := e.leaseRecord(e.m, name); got.leases != 0 || !got.last.Equal(e.clock.Now()) {
		t.Errorf("record after the end of the agent WebSocket = %+v, want no lease and the time %s", got, e.clock.Now())
	}
	if n := e.socketCount(name); n != 0 {
		t.Errorf("WebSocket leases after the end = %d, want 0", n)
	}
	e.clock.Advance(29 * time.Minute)
	e.m.Reap(context.Background())
	if e.podDeletes(name) != 0 {
		t.Fatal("the reaper stopped the sandbox 29 minutes after the end of the agent WebSocket")
	}
	e.clock.Advance(2 * time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the idle sandbox", func() bool { return e.m.pod(name) == nil })
}

// The WebSocket of an agent session can break in a turn. The Open WebUI
// backend then connects again after 1 second (docs/protocol.md, section 4.7).
// The new WebSocket keeps the sandbox again, with no time limit
// (docs/policies.md, section 10.4).
func TestAgentWebSocketThatConnectsAgainKeepsTheSandbox(t *testing.T) {
	e := newTestEnv(t, nil)
	alice := userRef("alice")
	name := alice.Ref.Name()
	first := e.acquire(held(alice))
	e.clock.Advance(time.Hour)
	e.m.Reap(context.Background())
	first.Release() // the connection breaks
	if got := e.leaseRecord(e.m, name); got.leases != 0 {
		t.Fatalf("leases after the end of the first WebSocket = %d, want 0", got.leases)
	}
	e.clock.Advance(time.Second)
	second := e.acquire(held(alice))
	defer second.Release()
	if got := e.leaseRecord(e.m, name); got.leases != 1 {
		t.Fatalf("leases after the new connection = %d, want 1", got.leases)
	}
	for i := 0; i < 48; i++ {
		e.clock.Advance(time.Hour)
		e.m.Reap(context.Background())
	}
	if hasSignal(second) || e.podDeletes(name) != 0 {
		t.Fatalf("the sandbox stopped with an open agent WebSocket: signal = %v, delete requests = %d", hasSignal(second), e.podDeletes(name))
	}
	if n := e.creates(); n != 1 {
		t.Errorf("pod create requests = %d, want 1", n)
	}
}

// A stop operation of the management API applies also to a sandbox with an
// open agent WebSocket, and its lease gets the stop signal.
// TestReaperStopsTellTheReason examines the stops of the reaper.
func TestHeldWebSocketGetsTheStopSignal(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	agent := e.acquire(held(req))
	if err := e.m.Stop(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	if reason := signal(t, agent, "stop operation"); reason != backend.StopAPI {
		t.Errorf("stop reason = %q, want %q", reason, backend.StopAPI)
	}
	agent.Release()
	if got := e.leaseRecord(e.m, name); got.leases != 0 {
		t.Errorf("leases after the end = %d, want 0", got.leases)
	}
	if n := e.socketCount(name); n != 0 {
		t.Errorf("WebSocket leases after the end = %d, want 0", n)
	}

	// The mark has an effect only for a WebSocket. A request that is not a
	// WebSocket keeps the sandbox in each condition, and it gets no signal.
	waitFor(t, "end of the stopped pod", func() bool { return e.m.pod(name) == nil })
	plain := req
	plain.Hold = true
	usual := e.acquire(plain)
	defer usual.Release()
	if got := e.leaseRecord(e.m, name); got.leases != 1 || e.socketCount(name) != 0 {
		t.Errorf("request with the mark and no WebSocket: leases = %d, WebSocket leases = %d; want 1 and 0", got.leases, e.socketCount(name))
	}
}

// This test records a limit, and not a requirement. The start of a WebSocket
// is a use of the sandbox, as each request is. A client that connects again
// thus starts the idle time again. A sandbox can make a connected client of
// Open WebUI do that: it closes its side with a code after which the client
// connects again (docs/policies.md, section 10.4). Thus the rule "a message
// of a sandbox is not a use" does not prevent that a sandbox with a connected
// client keeps itself. A change that removes this limit must change this
// test.
func TestNewWebSocketNearTheEndOfTheIdleTimeKeepsTheSandbox(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()

	// One second before the end of the idle time of 30 minutes.
	e.clock.Advance(30*time.Minute - time.Second)
	lease, err := e.m.Acquire(soon(t), socket(req))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	e.clock.Advance(2 * time.Minute)
	e.m.Reap(context.Background())
	if hasSignal(lease) || e.podDeletes(name) != 0 {
		t.Fatalf("the reaper stopped a sandbox 2 minutes after the start of a WebSocket: signal = %v, delete requests = %d",
			hasSignal(lease), e.podDeletes(name))
	}
	// With no more use, the sandbox stops one idle time after that start.
	e.clock.Advance(29 * time.Minute)
	e.m.Reap(context.Background())
	if reason := signal(t, lease, "idle stop"); reason != backend.StopIdle {
		t.Errorf("stop reason = %q, want %q", reason, backend.StopIdle)
	}
}
