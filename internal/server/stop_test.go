package server_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/coderpunch/warren/internal/backend"
)

// These tests are for the stop of a sandbox that has an open WebSocket. The
// backend gives the lease a stop signal and a reason (backend.Lease.Stopped).
// Warren then closes the two connections of the relay. The client gets a
// close code that agrees with the reason (docs/policies.md, section 10.2).
// The sandbox gets the code 1001.

// openSocket opens an authenticated WebSocket of alice, and waits until the
// relay to the terminal operates. It returns the connection and its lease.
func (e *env) openSocket(t *testing.T, path string) (*websocket.Conn, *mapLease) {
	t.Helper()
	conn := e.authenticatedSocket(t, path, "alice")
	if err := conn.WriteMessage(websocket.TextMessage, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := conn.ReadMessage(); err != nil || string(data) != "echo:x" {
		t.Fatalf("echo = %q, %v", data, err)
	}
	return conn, e.resolver.lastLease(t)
}

// sandboxCloses returns the close codes that the terminal of alice got.
func (e *env) sandboxCloses() []int {
	e.alice.mu.Lock()
	defer e.alice.mu.Unlock()
	return append([]int(nil), e.alice.wsClosed...)
}

func TestSandboxStopClosesWebSockets(t *testing.T) {
	const (
		terminal = "/api/terminals/abc12345"
		events   = "/events"
	)
	cases := []struct {
		reason, path string
		code         int
		text         string
	}{
		{backend.StopAPI, terminal, 1012, "An operation stopped or restarted the terminal"},
		{backend.StopIdle, terminal, 4004, "The terminal stopped after its idle time"},
		{backend.StopNoPolicy, terminal, 4004, "Template not found"},
		{backend.StopNetworkChanged, terminal, 1012, "The terminal restarts"},
		{backend.StopVolumeGone, terminal, 1012, "The terminal restarts"},
		{backend.StopExternal, terminal, 1012, "The terminal restarts"},
		{"failed", terminal, 1011, "The terminal failed"},
		{"not_ready", terminal, 1011, "The terminal failed"},
		{"a reason that the proxy does not know", terminal, 1011, "The terminal failed"},
		// In this version each route has the same codes.
		{backend.StopAPI, events, 1012, "An operation stopped or restarted the terminal"},
		{backend.StopIdle, events, 4004, "The terminal stopped after its idle time"},
		{backend.StopExternal, "/execute/p1/status", 1012, "The terminal restarts"},
		{backend.StopAPI, "/browser/view/sid/tok/stream", 1012, "An operation stopped or restarted the terminal"},
	}
	for _, c := range cases {
		t.Run(c.reason+" "+c.path, func(t *testing.T) {
			e := newEnv(t)
			var conn *websocket.Conn
			var lease *mapLease
			if strings.HasPrefix(c.path, "/browser/view/") {
				// This route has its key in the header, and no first message.
				var err error
				conn, _, err = e.dial(c.path, "alice", map[string][]string{"Authorization": {"Bearer " + connectionKey}})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				conn.WriteMessage(websocket.TextMessage, []byte("x"))
				if _, data, err := conn.ReadMessage(); err != nil || string(data) != "echo:x" {
					t.Fatalf("echo = %q, %v", data, err)
				}
				lease = e.resolver.lastLease(t)
			} else {
				conn, lease = e.openSocket(t, c.path)
			}
			if n := e.resolver.releases(); n != 0 {
				t.Fatalf("leases that ended while the WebSocket is open = %d", n)
			}

			lease.stop(c.reason)
			code, text := closeMessage(t, conn)
			if code != c.code || text != c.text {
				t.Errorf("close message = %d %q, want %d %q", code, text, c.code, c.text)
			}
			conn.Close()
			waitFor(t, "close of the sandbox connection", func() bool { return len(e.sandboxCloses()) == 1 })
			if got := e.sandboxCloses()[0]; got != websocket.CloseGoingAway {
				t.Errorf("the sandbox got close code %d, want 1001", got)
			}
			want := fmt.Sprintf(`"detail":"closed by warren, code %d, reason %s"`, c.code, c.reason)
			waitFor(t, "audit record", func() bool { return strings.Contains(e.logs.String(), want) })
			// The lease ends one time.
			waitFor(t, "end of the lease", func() bool { return e.resolver.openLeases() == 0 })
			if n := e.resolver.releases(); n != 1 {
				t.Errorf("leases that ended = %d, want 1", n)
			}
		})
	}
}

// A sandbox that stops sends its own close code, or it breaks the
// connection. Warren gives the stop signal before its delete request, thus
// the lease has the signal at that time. The client gets the code of warren,
// and not the code of the sandbox.
func TestStopReasonWinsOverTheCloseOfTheSandbox(t *testing.T) {
	for _, end := range []string{"close-1012", "cut"} {
		t.Run(end, func(t *testing.T) {
			e := newEnv(t)
			conn, lease := e.openSocket(t, "/api/terminals/abc12345")
			// The backend stops the sandbox, and the sandbox ends its side
			// immediately after that.
			e.alice.mu.Lock()
			e.alice.beforeEnd = func() { lease.stop(backend.StopIdle) }
			e.alice.mu.Unlock()
			if err := conn.WriteMessage(websocket.TextMessage, []byte(end)); err != nil {
				t.Fatal(err)
			}
			code, text := closeMessage(t, conn)
			if code != 4004 || text != "The terminal stopped after its idle time" {
				t.Errorf("close message = %d %q, want the code and the text of warren for the idle stop", code, text)
			}
			conn.Close()
			want := `"detail":"closed by warren, code 4004, reason idle"`
			waitFor(t, "audit record", func() bool { return strings.Contains(e.logs.String(), want) })
			waitFor(t, "end of the lease", func() bool { return e.resolver.openLeases() == 0 })
		})
	}
}

// With no stop signal, the close of the sandbox goes to the client as
// before.
func TestSandboxCloseWithNoStopPassesUnchanged(t *testing.T) {
	cases := []struct {
		end  string
		code int
	}{
		// The code of a sandbox that stops with no action of warren.
		{"close-1012", 1012},
		// A connection that breaks has no code. The client gets 1011.
		{"cut", 1011},
	}
	for _, c := range cases {
		t.Run(c.end, func(t *testing.T) {
			e := newEnv(t)
			conn, lease := e.openSocket(t, "/api/terminals/abc12345")
			if err := conn.WriteMessage(websocket.TextMessage, []byte(c.end)); err != nil {
				t.Fatal(err)
			}
			code, text := closeMessage(t, conn)
			// The text is not a text of warren for a stop.
			if code != c.code || strings.Contains(text, "terminal") {
				t.Errorf("close message = %d %q, want %d and no text of warren", code, text, c.code)
			}
			conn.Close()
			waitFor(t, "audit record", func() bool { return strings.Contains(e.logs.String(), `"detail":"closed by sandbox, code `) })
			if strings.Contains(e.logs.String(), "reason") {
				t.Errorf("the audit record has a stop reason:\n%s", e.logs.String())
			}
			waitFor(t, "end of the lease", func() bool { return e.resolver.openLeases() == 0 })
			// A stop signal after the end of the relay has no effect.
			lease.stop(backend.StopAPI)
		})
	}
}

// A client that does not answer the close message must not hold the handler
// and the lease.
func TestStopWhileTheClientDoesNotAnswer(t *testing.T) {
	e := newEnv(t)
	conn, lease := e.openSocket(t, "/api/terminals/abc12345")
	defer conn.Close()
	// The client does not read from this point, thus it sends no reply to the
	// close message.
	start := time.Now()
	lease.stop(backend.StopAPI)
	waitFor(t, "end of the lease", func() bool { return e.resolver.openLeases() == 0 })
	// The wait for the reply is 3 seconds. One more second is the limit for a
	// relay that waits in a write.
	if d := time.Since(start); d > 4500*time.Millisecond {
		t.Errorf("the handler ended %s after the stop, want a maximum of 4 seconds", d)
	}
	waitFor(t, "audit record", func() bool {
		return strings.Contains(e.logs.String(), `"detail":"closed by warren, code 1012, reason api"`)
	})
	if n := e.resolver.releases(); n != 1 {
		t.Errorf("leases that ended = %d, want 1", n)
	}
}

// The stop of a sandbox closes each WebSocket of that sandbox, and no
// WebSocket of a different sandbox.
func TestStopClosesOnlyTheWebSocketsOfItsSandbox(t *testing.T) {
	e := newEnv(t)
	first, firstLease := e.openSocket(t, "/api/terminals/abc12345")
	second, secondLease := e.openSocket(t, "/events")
	other := e.authenticatedSocket(t, "/events", "bob")
	other.WriteMessage(websocket.TextMessage, []byte("x"))
	if _, data, err := other.ReadMessage(); err != nil || string(data) != "echo:x" {
		t.Fatalf("echo for bob = %q, %v", data, err)
	}
	firstLease.stop(backend.StopAPI)
	secondLease.stop(backend.StopAPI)
	for _, conn := range []*websocket.Conn{first, second} {
		if code := closeCode(t, conn); code != websocket.CloseServiceRestart {
			t.Errorf("close code = %d, want 1012", code)
		}
		conn.Close()
	}
	// The WebSocket of bob operates.
	other.WriteMessage(websocket.TextMessage, []byte("y"))
	if _, data, err := other.ReadMessage(); err != nil || string(data) != "echo:y" {
		t.Errorf("echo for bob after the stop of the sandbox of alice = %q, %v", data, err)
	}
}
