package server_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These tests are for the use of a sandbox through a WebSocket
// (docs/policies.md, section 10.4). An open WebSocket is not a use of its
// sandbox. Warren tells the backend about each message of the client that is
// a use (backend.Lease.Active). The messages that a client of Open WebUI
// sends with no action of a person are not a use: "ping", "frameAck", and
// "ack" while the terminal is not visible (docs/protocol.md, section 4.2.4).
// A message of the sandbox is never a use. Warren relays each message with no
// change. The WebSocket of an agent session is the exception: it keeps its
// sandbox while it is open (backend.Request.Hold).

// exchange sends one message on a WebSocket whose terminal echoes, and makes
// sure that the terminal got the message with no change.
func exchange(t *testing.T, conn *websocket.Conn, messageType int, data string) {
	t.Helper()
	if err := conn.WriteMessage(messageType, []byte(data)); err != nil {
		t.Fatal(err)
	}
	expectEcho(t, conn, messageType, data)
}

// expectEcho reads the echo of one message.
func expectEcho(t *testing.T, conn *websocket.Conn, messageType int, data string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("no echo of the message %.80q: %v", data, err)
	}
	if mt != messageType || string(got) != "echo:"+data {
		t.Fatalf("the terminal got the message type %d and %d bytes, %.80q; want the type %d and %d bytes, %.80q",
			mt, len(got)-len("echo:"), strings.TrimPrefix(string(got), "echo:"), messageType, len(data), data)
	}
}

func TestWebSocketActivity(t *testing.T) {
	const (
		text   = websocket.TextMessage
		binary = websocket.BinaryMessage
		// A Go text with a backslash and "u" would be the character itself.
		escape = "\\" + "u00"
	)
	type message struct {
		kind int
		data string
		// calls is the number of calls of Active that the message adds.
		calls int
	}
	pad := func(n int, msg string) string { return strings.Repeat(" ", n-len(msg)) + msg }
	ack := message{text, `{"type":"ack","cursor":5}`, 0}
	visibleAck := message{text, `{"type":"ack","cursor":5}`, 1}
	visible := message{text, `{"type":"activity","active":true}`, 1}
	hidden := message{text, `{"type":"activity","active":false}`, 1}
	cases := []struct {
		name     string
		messages []message
	}{
		// The messages that a client of Open WebUI sends with no person.
		{"ping", []message{{text, `{"type":"ping"}`, 0}}},
		{"ack", []message{ack}},
		{"frameAck", []message{{text, `{"type":"frameAck","window":3}`, 0}}},
		{"ping with white space", []message{{text, " \t\r\n{ \"type\" : \"ping\" } \n", 0}}},
		{"ack of the Open WebUI backend", []message{{text, `{"type": "ack", "sequence": 5}`, 0}}},
		// Only the first bytes decide. The size has no effect: a sandbox
		// controls the value, and thus the size, of the cursor.
		{"ping of 257 bytes", []message{{text, `{"type":"ping","pad":"` + strings.Repeat("x", 233) + `"}`, 0}}},
		{"ack with a cursor of 300 bytes", []message{{text, `{"type":"ack","cursor":"` + strings.Repeat("9", 300) + `"}`, 0}}},
		{"ping with a second type", []message{{text, `{"type":"ping","type":"resize"}`, 0}}},
		{"type that ends at byte 64", []message{{text, pad(64, `{"type":"ping"`) + "}", 0}}},

		// Use messages.
		{"binary", []message{{binary, "x", 1}}},
		{"empty binary", []message{{binary, "", 1}}},
		{"binary of 100000 bytes", []message{{binary, strings.Repeat("\x00\xff", 50000), 1}}},
		{"binary with the bytes of a ping", []message{{binary, `{"type":"ping"}`, 1}}},
		{"resize", []message{{text, `{"type":"resize","cols":80,"rows":24}`, 1}}},
		{"hidden", []message{hidden}},
		{"chat", []message{{text, `{"type":"chat","chat_id":"chat-1"}`, 1}}},
		{"subscribe", []message{{text, `{"type":"subscribe","path":"/home/user"}`, 1}}},
		{"agent input", []message{{text, `{"type":"input","sequence":1,"data":"x"}`, 1}}},
		{"text that is not JSON", []message{{text, "hello", 1}}},
		{"JSON array", []message{{text, `[{"type":"ping"}]`, 1}}},
		{"key with a capital letter", []message{{text, `{"Type":"ping"}`, 1}}},
		{"type with a capital letter", []message{{text, `{"type":"Ping"}`, 1}}},
		{"type that is longer", []message{{text, `{"type":"pingx"}`, 1}}},
		{"type that is not the first member", []message{{text, `{"cursor":1,"type":"ack"}`, 1}}},
		{"key with an escape sequence", []message{{text, `{"` + escape + `74ype":"ping"}`, 1}}},
		{"type with an escape sequence", []message{{text, `{"type":"p` + escape + `69ng"}`, 1}}},
		{"type that ends at byte 65", []message{{text, pad(65, `{"type":"ping"`) + "}", 1}}},
		{"text of 100000 bytes", []message{{text, strings.Repeat("x", 100000), 1}}},
		{"two use messages", []message{{binary, "a", 1}, {binary, "b", 1}}},

		// An ack is a use only while the client reports that it shows the
		// terminal. A ping and a frameAck are never a use.
		{"visible, then ack", []message{visible, visibleAck, visibleAck}},
		{"hidden, then ack", []message{hidden, ack}},
		{"visible, hidden, then ack", []message{visible, hidden, ack}},
		{"hidden, visible, then ack", []message{hidden, visible, visibleAck}},
		{"visible, then ping", []message{visible, {text, `{"type":"ping"}`, 0}}},
		{"visible, then frameAck", []message{visible, {text, `{"type":"frameAck","window":3}`, 0}}},
		{"visible, then an ack with a cursor of 300 bytes", []message{visible, {text, `{"type":"ack","cursor":"` + strings.Repeat("9", 300) + `"}`, 1}}},
		{"activity that is not a boolean", []message{{text, `{"type":"activity","active":"yes"}`, 1}, ack}},
		{"visible, then an activity that is not a boolean", []message{visible, {text, `{"type":"activity","active":"yes"}`, 1}, visibleAck}},
		{"visible in 64 bytes", []message{{text, pad(64, `{"type":"activity","active":true}`), 1}, visibleAck}},
		{"visible in 65 bytes", []message{{text, pad(65, `{"type":"activity","active":true}`), 1}, ack}},
		// The first 64 bytes are a complete visibility message.
		{"visible, then white space, 65 bytes", []message{{text, `{"type":"activity","active":true}` + strings.Repeat(" ", 32), 1}, ack}},
		{"visible in binary data", []message{{binary, `{"type":"activity","active":true}`, 1}, ack}},
	}
	e := newEnv(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn := e.authenticatedSocket(t, "/api/terminals/abc12345", "alice")
			want := 0
			for i, m := range c.messages {
				exchange(t, conn, m.kind, m.data)
				// The echo came, thus the relay has its lease, and the call
				// for this message is complete.
				lease := e.resolver.lastLease(t)
				want += m.calls
				if got := lease.activeCalls(); got != want {
					t.Fatalf("calls of Active after message %d (%.60q) = %d, want %d", i+1, m.data, got, want)
				}
			}
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
			conn.Close()
			waitFor(t, "end of the lease", func() bool { return e.resolver.openLeases() == 0 })
		})
	}
}

// The rule is the same on each WebSocket route. The first message of a route
// with a first message is the authentication, and not a use message. A route
// with its key in the header has no such message: its first message is data.
func TestWebSocketActivityOnEachRoute(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{
		"/api/terminals/abc12345", "/events", "/execute", "/execute/p1/status", "/desktop/stream", "/desktop/audio",
		"/agents/sessions/s1/connect", "/browser/view/sid/tok/ws", "/browser/view/sid/tok/stream", "/browser/view/sid/tok/vnc",
	} {
		t.Run(path, func(t *testing.T) {
			var conn *websocket.Conn
			if strings.HasPrefix(path, "/browser/view/") {
				var err error
				conn, _, err = e.dial(path, "alice", http.Header{"Authorization": {"Bearer " + connectionKey}})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
			} else {
				conn = e.authenticatedSocket(t, path, "alice")
			}
			for i, m := range []struct {
				kind  int
				data  string
				calls int
			}{
				{websocket.TextMessage, `{"type":"frameAck","window":3}`, 0},
				{websocket.TextMessage, `{"type":"ping"}`, 0},
				{websocket.TextMessage, `{"type":"ack","sequence":1}`, 0},
				{websocket.BinaryMessage, "\x03\x00\x00\x00", 1},
				{websocket.TextMessage, `{"type":"mouse","x":1,"y":2}`, 2},
			} {
				exchange(t, conn, m.kind, m.data)
				if got := e.resolver.lastLease(t).activeCalls(); got != m.calls {
					t.Fatalf("calls of Active after message %d = %d, want %d", i+1, got, m.calls)
				}
			}
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
			conn.Close()
			waitFor(t, "end of the lease", func() bool { return e.resolver.openLeases() == 0 })
		})
	}
}

// sendFragments sends one message as a sequence of frames, one for each part.
// The library of the tests has no function for this. A frame of a client has
// a mask: with the key 0, the data bytes do not change.
func sendFragments(t *testing.T, conn *websocket.Conn, messageType int, parts ...string) {
	t.Helper()
	var raw []byte
	for i, part := range parts {
		if len(part) > 125 {
			t.Fatalf("the test is wrong: a part of %d bytes needs a longer frame header", len(part))
		}
		opcode := byte(0) // continuation frame
		if i == 0 {
			opcode = byte(messageType)
		}
		if i == len(parts)-1 {
			opcode |= 0x80 // last frame
		}
		raw = append(raw, opcode, 0x80|byte(len(part)), 0, 0, 0, 0)
		raw = append(raw, part...)
	}
	if _, err := conn.NetConn().Write(raw); err != nil {
		t.Fatal(err)
	}
}

// A message can come in many frames. The decision is for the message, and
// not for a frame: a ping in 15 frames is one automatic message.
func TestPingInFragmentsIsNotActivity(t *testing.T) {
	e := newEnv(t)
	conn := e.authenticatedSocket(t, "/api/terminals/abc12345", "alice")
	ping := `{"type":"ping"}`
	sendFragments(t, conn, websocket.TextMessage, strings.Split(ping, "")...)
	// The terminal gets one message.
	expectEcho(t, conn, websocket.TextMessage, ping)
	lease := e.resolver.lastLease(t)
	if got := lease.activeCalls(); got != 0 {
		t.Fatalf("calls of Active after a ping in %d frames = %d, want 0", len(ping), got)
	}
	// Frames with no data, before and after the type.
	sendFragments(t, conn, websocket.TextMessage, "", `{"type"`, "", `:"pi`, `ng"`, "", "}", "")
	expectEcho(t, conn, websocket.TextMessage, ping)
	if got := lease.activeCalls(); got != 0 {
		t.Fatalf("calls of Active after a ping with empty frames = %d, want 0", got)
	}
	// A use message in many frames is one use.
	resize := `{"type":"resize","cols":80,"rows":24}`
	sendFragments(t, conn, websocket.TextMessage, strings.Split(resize, "")...)
	expectEcho(t, conn, websocket.TextMessage, resize)
	if got := lease.activeCalls(); got != 1 {
		t.Fatalf("calls of Active after a resize message in %d frames = %d, want 1", len(resize), got)
	}
	sendFragments(t, conn, websocket.BinaryMessage, "a", "b", "c")
	expectEcho(t, conn, websocket.BinaryMessage, "abc")
	if got := lease.activeCalls(); got != 2 {
		t.Fatalf("calls of Active after a binary message in 3 frames = %d, want 2", got)
	}
}

// A message of the sandbox is never a use: the output of a program is not
// proof that a person uses the sandbox. A message of the sandbox also cannot
// set the mark that makes an "ack" a use.
//
// This test is for the rule of the relay only. It does not show that a
// sandbox cannot cause a use. While a client of Open WebUI is connected, a
// sandbox can make that client send requests and messages (docs/policies.md,
// section 10.4).
func TestSandboxMessagesAreNotActivity(t *testing.T) {
	e := newEnv(t)
	conn := e.authenticatedSocket(t, "/api/terminals/burst123", "alice")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	texts, binaries := 0, 0
	for i := 0; i < burstMessages; i++ {
		mt, _, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("message %d of the sandbox: %v", i+1, err)
		}
		if mt == websocket.TextMessage {
			texts++
		} else {
			binaries++
		}
	}
	if texts != 75 || binaries != 25 {
		t.Fatalf("messages of the sandbox: %d text and %d binary, want 75 and 25", texts, binaries)
	}
	lease := e.resolver.lastLease(t)
	if got := lease.activeCalls(); got != 0 {
		t.Fatalf("calls of Active after %d messages of the sandbox = %d, want 0", burstMessages, got)
	}
	// The sandbox sent {"type":"activity","active":true}. The mark of the
	// client did not change: an ack is not a use.
	exchange(t, conn, websocket.TextMessage, `{"type":"ack","cursor":21}`)
	if got := lease.activeCalls(); got != 0 {
		t.Fatalf("calls of Active after an ack = %d, want 0: a message of the sandbox set the mark of the client", got)
	}
	// The echo of a message is also a message of the sandbox.
	exchange(t, conn, websocket.TextMessage, `{"type":"ping"}`)
	if got := lease.activeCalls(); got != 0 {
		t.Fatalf("calls of Active after a ping and its echo = %d, want 0", got)
	}
	// The same message from the client sets the mark.
	exchange(t, conn, websocket.TextMessage, `{"type":"activity","active":true}`)
	exchange(t, conn, websocket.TextMessage, `{"type":"ack","cursor":21}`)
	if got := lease.activeCalls(); got != 2 {
		t.Fatalf("calls of Active after the client reported that it is visible, and an ack = %d, want 2", got)
	}
}

// The control frames of the WebSocket protocol are not messages. Warren
// answers a ping itself. It does not send the frame to the sandbox, and the
// frame is not a use.
func TestControlFramesAreNotActivity(t *testing.T) {
	e := newEnv(t)
	conn := e.authenticatedSocket(t, "/api/terminals/abc12345", "alice")
	pongs := make(chan string, 4)
	conn.SetPongHandler(func(data string) error { pongs <- data; return nil })
	for _, data := range []string{"keepalive", ""} {
		if err := conn.WriteControl(websocket.PingMessage, []byte(data), time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	// A pong that answers no ping.
	if err := conn.WriteControl(websocket.PongMessage, []byte("x"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// The read of the next message handles the pong frames. The terminal
	// gets this message first: it got no data for the control frames.
	exchange(t, conn, websocket.TextMessage, `{"type":"ping"}`)
	for _, want := range []string{"keepalive", ""} {
		select {
		case got := <-pongs:
			if got != want {
				t.Errorf("pong data = %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("warren did not answer a ping of the client")
		}
	}
	if got := e.resolver.lastLease(t).activeCalls(); got != 0 {
		t.Errorf("calls of Active after control frames and a ping message = %d, want 0", got)
	}
	e.alice.mu.Lock()
	pings := e.alice.wsPings
	e.alice.mu.Unlock()
	if pings != 0 {
		t.Errorf("the terminal got %d ping frames, want 0", pings)
	}
}

// The start of a WebSocket is one request to the backend. The backend counts
// that start as a use (the Kubernetes backend does). The first message has
// the authentication, and it is not a use message.
func TestWebSocketOpenIsOneAcquire(t *testing.T) {
	e := newEnv(t)
	conn := e.authenticatedSocket(t, "/api/terminals/abc12345", "alice")
	waitFor(t, "first message at the terminal", func() bool {
		e.alice.mu.Lock()
		defer e.alice.mu.Unlock()
		return len(e.alice.wsFirst) == 1
	})
	// The relay operates: a message of the sandbox side comes back.
	exchange(t, conn, websocket.TextMessage, `{"type":"ping"}`)
	if n := e.resolver.callCount(); n != 1 {
		t.Errorf("backend calls = %d, want 1", n)
	}
	if got := e.resolver.lastLease(t).activeCalls(); got != 0 {
		t.Errorf("calls of Active after the first message = %d, want 0", got)
	}
	if n := e.resolver.openLeases(); n != 1 {
		t.Errorf("open leases = %d, want 1", n)
	}
}

// Only the WebSocket of an agent session has the mark that its open
// connection keeps the sandbox (docs/policies.md, section 10.4). The Open
// WebUI backend is its only client.
func TestOnlyTheAgentRouteHoldsTheSandbox(t *testing.T) {
	e := newEnv(t)
	sockets := []struct {
		path string
		hold bool
	}{
		{"/agents/sessions/s1/connect", true},
		// The query string has no effect, and the rule uses the decoded path.
		{"/agents/sessions/s1/connect?user_id=bob", true},
		{"/agents/sessions/s%31/connect", true},
		{"/%61gents/sessions/s1/connect", true},
		{"/api/terminals/abc12345", false},
		{"/api/terminals/connect", false},
		{"/events", false},
		{"/execute", false},
		{"/execute/p1/status", false},
		{"/desktop/stream", false},
		{"/desktop/audio", false},
		{"/browser/view/sid/tok/ws", false},
		{"/browser/view/sid/tok/stream", false},
		{"/browser/view/sid/tok/vnc", false},
		{"/browser/view/agents/sessions/stream", false},
	}
	for _, c := range sockets {
		before := e.resolver.callCount()
		var conn *websocket.Conn
		if strings.HasPrefix(c.path, "/browser/view/") {
			var err error
			conn, _, err = e.dial(c.path, "alice", http.Header{"Authorization": {"Bearer " + connectionKey}})
			if err != nil {
				t.Fatalf("%s: %v", c.path, err)
			}
		} else {
			conn = e.authenticatedSocket(t, c.path, "alice")
		}
		req := e.lastRequest(t, before)
		if req.Hold != c.hold || !req.WebSocket || req.Passive || req.UserID != "alice" {
			t.Errorf("WebSocket %s: backend request = %+v, want Hold = %v for a WebSocket of alice", c.path, req, c.hold)
		}
		// The agent route is a viewer route: it starts a stopped sandbox.
		if c.hold && req.NoStart {
			t.Errorf("WebSocket %s: the request does not begin a start", c.path)
		}
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
		conn.Close()
	}
	waitFor(t, "end of the leases", func() bool { return e.resolver.openLeases() == 0 })

	// A path that is not exactly the route is not a WebSocket route. Warren
	// refuses the upgrade, and the backend gets no request.
	before := e.resolver.callCount()
	for _, path := range []string{"/agents/sessions/s1/connect/", "/agents/sessions/s1/connect/x", "/agents/sessions/s1", "/agents/sessions//connect"} {
		_, resp, err := e.dial(path, "alice", nil)
		if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
			t.Errorf("WebSocket %s: err = %v, response = %v; want status 404", path, err, resp)
		}
	}
	if n := e.resolver.callCount(); n != before {
		t.Errorf("backend calls for paths that are not a WebSocket route = %d, want 0", n-before)
	}

	// The mark is for a WebSocket upgrade only. An HTTP request keeps the
	// sandbox while it is in progress, with no mark.
	for _, c := range []struct{ method, path string }{
		{"GET", "/agents/sessions/s1/connect"},
		{"GET", "/agents/sessions/s1"},
		{"DELETE", "/agents/sessions/s1"},
		{"POST", "/agents/claude/sessions"},
	} {
		before := e.resolver.callCount()
		e.request(c.method, c.path, "alice", nil)
		if req := e.lastRequest(t, before); req.Hold || req.WebSocket || req.NoStart || req.Passive {
			t.Errorf("%s %s: backend request = %+v, want a usual request", c.method, c.path, req)
		}
	}
}
