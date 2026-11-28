package backend

import (
	"context"
	"net/url"
	"testing"
)

// The development backend has one server that always operates. A passive
// request gets that server, as each request.
func TestStaticBackendIgnoresPassive(t *testing.T) {
	target := Target{URL: &url.URL{Scheme: "http", Host: "127.0.0.1:8000"}, APIKey: "key"}
	for _, passive := range []bool{false, true} {
		lease, err := Static{Server: target}.Acquire(context.Background(), Request{Passive: passive})
		if err != nil || lease.Target() != target {
			t.Fatalf("passive = %v: lease = %v, error = %v; want the one server", passive, lease, err)
		}
		lease.Release()
	}
}

// A request that does not begin a start gets the one server immediately:
// that server always operates.
func TestStaticBackendIgnoresNoStart(t *testing.T) {
	target := Target{URL: &url.URL{Scheme: "http", Host: "127.0.0.1:8000"}, APIKey: "key"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, req := range []Request{{NoStart: true}, {NoStart: true, WebSocket: true}} {
		// The backend does not wait, also when the context is at its end.
		lease, err := Static{Server: target}.Acquire(ctx, req)
		if err != nil || lease.Target() != target {
			t.Fatalf("request %+v: lease = %v, error = %v; want the one server", req, lease, err)
		}
		lease.Release()
	}
}

// The development backend stops nothing, thus its lease never gets a stop
// signal.
func TestStaticLeaseHasNoStopSignal(t *testing.T) {
	lease, err := Static{}.Acquire(context.Background(), Request{WebSocket: true})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	select {
	case <-lease.Stopped():
		t.Error("the lease of the development backend has a stop signal")
	default:
	}
	if got := lease.StopReason(); got != "" {
		t.Errorf("stop reason = %q, want no reason", got)
	}
}
