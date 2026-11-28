package server_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/coderpunch/warren/internal/backend"
)

// These tests are for the two classes of WebSocket routes (docs/policies.md,
// section 10.3). On a watcher route the browser only watches a sandbox: the
// events of files and ports, the process list, and the output of a process.
// Open WebUI opens these WebSockets with no action of a person, and it
// connects again with no limit (docs/protocol.md, section 4.2.3). Thus a
// watcher must not begin the start of a sandbox. Each other WebSocket route
// is a viewer route.

// lastRequest waits until the backend got one more request than before, and
// returns that request.
func (e *env) lastRequest(t *testing.T, before int) backend.Request {
	t.Helper()
	waitFor(t, "backend call", func() bool { return e.resolver.callCount() == before+1 })
	return e.resolver.requests()[before]
}

func TestOnlyWatcherRoutesDoNotStartASandbox(t *testing.T) {
	e := newEnv(t)
	sockets := []struct {
		path    string
		watcher bool
	}{
		{"/events", true},
		{"/execute", true},
		{"/execute/p1/status", true},
		// The query string has no effect on the rule.
		{"/execute/p1/status?offset=0&tail=1000", true},
		{"/events?user_id=bob", true},
		// The rule uses the decoded path, as the route table.
		{"/%65vents", true},
		{"/execute/p%31/status", true},
		{"/api/terminals/abc12345", false},
		{"/api/terminals/execute", false},
		{"/api/terminals/events", false},
		{"/desktop/stream", false},
		{"/desktop/audio", false},
		{"/agents/sessions/s1/connect", false},
		{"/agents/sessions/execute/connect", false},
		{"/browser/view/sid/tok/ws", false},
		{"/browser/view/sid/tok/stream", false},
		{"/browser/view/sid/tok/vnc", false},
		{"/browser/view/events/execute/stream", false},
	}
	for _, c := range sockets {
		before := e.resolver.callCount()
		var conn *websocket.Conn
		if strings.HasPrefix(c.path, "/browser/view/") {
			// This route has its key in the header, and no first message.
			var err error
			conn, _, err = e.dial(c.path, "alice", http.Header{"Authorization": {"Bearer " + connectionKey}})
			if err != nil {
				t.Fatalf("%s: %v", c.path, err)
			}
		} else {
			conn = e.authenticatedSocket(t, c.path, "alice")
		}
		req := e.lastRequest(t, before)
		if req.NoStart != c.watcher || !req.WebSocket || req.Passive || req.UserID != "alice" {
			t.Errorf("WebSocket %s: backend request = %+v, want NoStart = %v for a WebSocket of alice", c.path, req, c.watcher)
		}
		// On a sandbox that operates, each class gets its relay.
		conn.WriteMessage(websocket.TextMessage, []byte("x"))
		if _, data, err := conn.ReadMessage(); err != nil || string(data) != "echo:x" {
			t.Errorf("WebSocket %s: echo = %q, %v", c.path, data, err)
		}
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
		conn.Close()
	}
	waitFor(t, "end of the leases", func() bool { return e.resolver.openLeases() == 0 })

	// The rule is for a WebSocket upgrade only. An HTTP request on the same
	// path is a usual request: it starts a stopped sandbox. The process list
	// of Open WebUI sends such requests when its WebSocket is not available.
	requests := []struct{ method, path string }{
		{"GET", "/execute"},
		{"GET", "/execute/p1/status?wait=0&offset=0&tail=1000"},
		{"POST", "/execute"},
		{"GET", "/events"},
		{"GET", "/files/cwd"},
		{"GET", "/system/info"},
	}
	for _, c := range requests {
		before := e.resolver.callCount()
		e.request(c.method, c.path, "alice", nil)
		if req := e.lastRequest(t, before); req.NoStart || req.WebSocket {
			t.Errorf("%s %s: backend request = %+v, want a request that can start the sandbox", c.method, c.path, req)
		}
	}
}

// A watcher for a sandbox that does not operate gets no refusal of its
// upgrade. Warren accepts the WebSocket, and closes it with the code 1013
// when the backend gives no sandbox. The backend gets the request with the
// mark that it must not begin a start.
func TestWatcherForAStoppedSandbox(t *testing.T) {
	for _, path := range []string{"/events", "/execute", "/execute/p1/status"} {
		t.Run(path, func(t *testing.T) {
			e := newEnv(t)
			// carol has no sandbox.
			conn, resp, err := e.dial(path, "carol", nil)
			if err != nil {
				t.Fatalf("warren refused the upgrade of a watcher: %v", err)
			}
			defer conn.Close()
			if resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("status of the upgrade = %d, want 101", resp.StatusCode)
			}
			conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth","token":"`+connectionKey+`"}`))
			code, text := closeMessage(t, conn)
			if code != websocket.CloseTryAgainLater || text != "The terminal is not ready" {
				t.Errorf("close message = %d %q, want 1013 and the text of a terminal that is not ready", code, text)
			}
			calls := e.resolver.requests()
			if len(calls) != 1 || !calls[0].NoStart || !calls[0].WebSocket || calls[0].Passive || calls[0].UserID != "carol" {
				t.Errorf("backend requests = %+v, want one WebSocket request of carol that does not begin a start", calls)
			}
			if n := e.terminalRequests(); n != 0 {
				t.Errorf("%d requests went to a terminal", n)
			}
			if n := e.resolver.openLeases(); n != 0 {
				t.Errorf("open leases = %d, want 0", n)
			}
			waitFor(t, "audit record", func() bool { return strings.Contains(e.logs.String(), `"detail":"sandbox not ready"`) })
			if logs := e.logs.String(); !strings.Contains(logs, `"status":101`) {
				t.Errorf("the audit record does not have the status 101:\n%s", logs)
			}
		})
	}
}

// A watcher waits while the backend has no sandbox for it. It has no lease
// in that time, and nothing goes to a terminal. When the sandbox operates,
// the watcher gets its relay, and the sandbox gets the messages that the
// client sent in the wait.
func TestWatcherWaitsForItsSandbox(t *testing.T) {
	e := newEnv(t)
	block := make(chan struct{})
	e.resolver.mu.Lock()
	e.resolver.block = block
	e.resolver.mu.Unlock()

	conn := e.authenticatedSocket(t, "/events", "alice")
	// The client of Open WebUI sends this message immediately after the
	// first message.
	subscribe := `{"type":"subscribe","path":"/home/user"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(subscribe)); err != nil {
		t.Fatal(err)
	}
	if req := e.lastRequest(t, 0); !req.NoStart || !req.WebSocket {
		t.Fatalf("backend request = %+v, want a WebSocket request that does not begin a start", req)
	}
	closed := make(chan error, 1)
	messages := make(chan string, 1)
	go func() {
		_, data, err := conn.ReadMessage()
		if err != nil {
			closed <- err
			return
		}
		messages <- string(data)
	}()
	select {
	case err := <-closed:
		t.Fatalf("warren closed the watcher in the wait: %v", err)
	case data := <-messages:
		t.Fatalf("the watcher got the message %q in the wait", data)
	case <-time.After(150 * time.Millisecond):
	}
	if n := e.resolver.openLeases(); n != 0 {
		t.Errorf("open leases in the wait = %d, want 0", n)
	}
	if n := e.terminalRequests(); n != 0 {
		t.Errorf("%d requests went to a terminal in the wait", n)
	}

	// A different request started the sandbox, and the sandbox is ready.
	close(block)
	select {
	case err := <-closed:
		t.Fatalf("warren closed the watcher after the start of its sandbox: %v", err)
	case data := <-messages:
		if data != "echo:"+subscribe {
			t.Errorf("first message from the sandbox = %q, want the echo of the message from the wait", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the watcher got no relay after the start of its sandbox")
	}
	if n := e.resolver.callCount(); n != 1 {
		t.Errorf("backend calls = %d, want 1", n)
	}
	if n := e.resolver.openLeases(); n != 1 {
		t.Errorf("open leases with the relay = %d, want 1", n)
	}
}
