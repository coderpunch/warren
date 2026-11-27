package proxy

import (
	"context"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/coderpunch/warren/internal/backend"
)

// stopLease is a lease that has the stop signal when reason is not empty.
type stopLease struct {
	backend.Lease
	reason string
}

func (l stopLease) Stopped() <-chan struct{} {
	if l.reason == "" {
		return nil
	}
	c := make(chan struct{})
	close(c)
	return c
}

func (l stopLease) StopReason() string { return l.reason }

// A sandbox that stops sends its own close code, or it breaks the
// connection. The relay can see that event before it sees the stop signal of
// the lease. The client must get the code of warren also in that sequence,
// and the code of its class of route.
func TestStopOfTheBackendIsTheCauseWhenTheSandboxSideEnds(t *testing.T) {
	const text = "The terminal stopped after its idle time"
	for class, c := range map[string]struct {
		watcher bool
		want    closeInfo
	}{
		"viewer":  {false, closeInfo{side: sideWarren, code: closeNotFound, text: text, reason: backend.StopIdle}},
		"watcher": {true, closeInfo{side: sideWarren, code: websocket.CloseServiceRestart, text: text, reason: backend.StopIdle}},
	} {
		stopped := &session{lease: stopLease{reason: backend.StopIdle}, watcher: c.watcher}
		for name, first := range map[string]closeInfo{
			"close message of the sandbox": {side: sideSandbox, code: websocket.CloseServiceRestart},
			"connection that broke":        {side: sideSandbox, code: websocket.CloseAbnormalClosure},
			"write that failed":            {side: sideSandbox, code: websocket.CloseAbnormalClosure, writeFailed: true},
		} {
			if got := stopped.cause(first); got != c.want {
				t.Errorf("%s, %s, with a stop signal: cause = %+v, want %+v", class, name, got, c.want)
			}
			// With no stop signal, the close of the sandbox is the cause.
			running := &session{lease: stopLease{}, watcher: c.watcher}
			if got := running.cause(first); got != first {
				t.Errorf("%s, %s, with no stop signal: cause = %+v, want %+v", class, name, got, first)
			}
		}
		// A client that closes at the same time is the cause of the end. The
		// sandbox then gets the close code of the client.
		client := closeInfo{side: sideClient, code: websocket.CloseNormalClosure}
		if got := stopped.cause(client); got != client {
			t.Errorf("%s, close of the client, with a stop signal: cause = %+v, want %+v", class, got, client)
		}
	}
}

func TestBackendStopNeedsTheSignal(t *testing.T) {
	for _, watcher := range []bool{false, true} {
		if _, ok := backendStop(stopLease{}, watcher); ok {
			t.Errorf("watcher %v: a lease with no stop signal gave a stop", watcher)
		}
		// The development backend stops nothing.
		lease, err := backend.Static{}.Acquire(context.Background(), backend.Request{WebSocket: true, NoStart: watcher})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := backendStop(lease, watcher); ok {
			t.Errorf("watcher %v: the lease of the development backend gave a stop", watcher)
		}
		got, ok := backendStop(stopLease{reason: backend.StopAPI}, watcher)
		want := closeInfo{side: sideWarren, code: websocket.CloseServiceRestart, text: "An operation stopped or restarted the terminal", reason: backend.StopAPI}
		if !ok || got != want {
			t.Errorf("watcher %v: stop by an operation = %+v, %v; want %+v", watcher, got, ok, want)
		}
	}
}

// The close table of docs/policies.md, section 10.2: one row for each stop
// reason, and one column for each class of route. The two classes are different
// only for the idle stop.
func TestStopCloseTable(t *testing.T) {
	const (
		operation = "An operation stopped or restarted the terminal"
		idle      = "The terminal stopped after its idle time"
		restarts  = "The terminal restarts"
		failed    = "The terminal failed"
	)
	cases := []struct {
		reason          string
		viewer, watcher int
		text            string
	}{
		{backend.StopAPI, 1012, 1012, operation},
		{backend.StopIdle, 4004, 1012, idle},
		{backend.StopNoPolicy, 4004, 4004, "Template not found"},
		{backend.StopNetworkChanged, 1012, 1012, restarts},
		{backend.StopVolumeGone, 1012, 1012, restarts},
		{backend.StopExternal, 1012, 1012, restarts},
		{"failed", 1011, 1011, failed},
		{"not_ready", 1011, 1011, failed},
		{"start_error", 1011, 1011, failed},
		{"unreachable", 1011, 1011, failed},
		{"bad_key", 1011, 1011, failed},
		{"public_address", 1011, 1011, failed},
		{"no_network_policy", 1011, 1011, failed},
		{"a reason that the proxy does not know", 1011, 1011, failed},
		{"", 1011, 1011, failed},
	}
	for _, c := range cases {
		if code, text := stopClose(c.reason, false); code != c.viewer || text != c.text {
			t.Errorf("reason %q, viewer: %d %q, want %d %q", c.reason, code, text, c.viewer, c.text)
		}
		if code, text := stopClose(c.reason, true); code != c.watcher || text != c.text {
			t.Errorf("reason %q, watcher: %d %q, want %d %q", c.reason, code, text, c.watcher, c.text)
		}
	}
}

// The watcher routes are exactly three of the routes with a first message.
// The rule uses the decoded path, as the route table.
func TestWatcherRoutes(t *testing.T) {
	cases := map[string]bool{
		"/events":                             true,
		"/execute":                            true,
		"/execute/p1/status":                  true,
		"/execute/a b/status":                 true,
		"/api/terminals/abc12345":             false,
		"/desktop/stream":                     false,
		"/desktop/audio":                      false,
		"/agents/sessions/s1/connect":         false,
		"/browser/view/sid/tok/ws":            false,
		"/browser/view/sid/tok/stream":        false,
		"/browser/view/sid/tok/vnc":           false,
		"/browser/view/events/execute/ws":     false,
		"/events/":                            false,
		"/events/x":                           false,
		"/Events":                             false,
		"//events":                            false,
		"/x/events":                           false,
		"/execute/":                           false,
		"/execute/p1":                         false,
		"/execute/p1/status/":                 false,
		"/execute/p1/status/x":                false,
		"/execute//status":                    false,
		"/execute/a/b/status":                 false,
		"/execute/status":                     false,
		"/api/terminals/execute":              false,
		"/api/terminals/events":               false,
		"/agents/sessions/execute/connect":    false,
		"/proxy/3000/events":                  false,
		"/system/info":                        false,
		"":                                    false,
		"/":                                   false,
		"/events\n":                           false,
		"events":                              false,
		"/execute/p1/status\n/events":         false,
		"/api/terminals/x\n/execute/p/status": false,
	}
	for path, want := range cases {
		if got := watcherRoute(path); got != want {
			t.Errorf("watcherRoute(%q) = %v, want %v", path, got, want)
		}
		// Each watcher route is a route with a first message.
		if want && classifyWS(path) != wsFirstMessage {
			t.Errorf("the watcher route %q is not a route with a first message", path)
		}
	}
}
