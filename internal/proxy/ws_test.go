package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// Only the WebSocket of an agent session keeps its sandbox while it is open.
// The rule uses the decoded path, as the route table.
func TestHoldRoutes(t *testing.T) {
	cases := map[string]bool{
		"/agents/sessions/s1/connect":          true,
		"/agents/sessions/a b/connect":         true,
		"/agents/sessions/connect/connect":     true,
		"/agents/sessions/s1/connect/":         false,
		"/agents/sessions/s1/connect/x":        false,
		"/agents/sessions//connect":            false,
		"/agents/sessions/a/b/connect":         false,
		"/agents/sessions/s1":                  false,
		"/agents/sessions/s1/Connect":          false,
		"/agents/s1/connect":                   false,
		"/x/agents/sessions/s1/connect":        false,
		"//agents/sessions/s1/connect":         false,
		"/agents":                              false,
		"/api/terminals/abc12345":              false,
		"/api/terminals/connect":               false,
		"/events":                              false,
		"/execute":                             false,
		"/execute/p1/status":                   false,
		"/desktop/stream":                      false,
		"/desktop/audio":                       false,
		"/browser/view/sid/tok/ws":             false,
		"/browser/view/sid/tok/stream":         false,
		"/browser/view/sid/tok/vnc":            false,
		"/browser/view/agents/sessions/stream": false,
		"":                                     false,
		"/":                                    false,
		"/agents/sessions/s1/connect\n":        false,
		"/events\n/agents/sessions/s1/connect": false,
	}
	for path, want := range cases {
		if got := holdRoute(path); got != want {
			t.Errorf("holdRoute(%q) = %v, want %v", path, got, want)
		}
		// The route is a viewer route with a first message.
		if want && (classifyWS(path) != wsFirstMessage || watcherRoute(path)) {
			t.Errorf("the route %q: class = %v, watcher = %v; want a viewer route with a first message", path, classifyWS(path), watcherRoute(path))
		}
	}
}

// The rule for an automatic message examines only the start of the message.
func TestAutomaticKind(t *testing.T) {
	cases := []struct {
		head string
		want messageKind
	}{
		// The messages that the clients of Open WebUI send.
		{`{"type":"ping"}`, kindPing},
		{`{"type":"ack","cursor":5}`, kindAck},
		{`{"type":"frameAck","window":3}`, kindFrameAck},
		// The form that the Open WebUI backend sends on the agent WebSocket.
		{`{"type": "ack", "sequence": 5}`, kindAck},
		// White space of JSON in each permitted place.
		{" \t\r\n{ \t\r\n\"type\" \t\r\n: \t\r\n\"ping\" \t\r\n}", kindPing},
		// All bytes after the type have no effect.
		{`{"type":"ping"`, kindPing},
		{`{"type":"ping","type":"resize"}`, kindPing},
		{`{"type":"ack"junk`, kindAck},
		{`{"type":"ack","cursor":{"type":"activity","active":true}}`, kindAck},

		// Use messages.
		{``, kindUse},
		{`{`, kindUse},
		{`{}`, kindUse},
		{`{"type"`, kindUse},
		{`{"type":`, kindUse},
		{`{"type":"`, kindUse},
		{`{"type":"ping`, kindUse},
		{`{"type":"pin"}`, kindUse},
		{`{"type":"pingx"}`, kindUse},
		{`{"type":"Ping"}`, kindUse},
		{`{"type":"PING"}`, kindUse},
		{`{"type":"ackx"}`, kindUse},
		{`{"type":"frameack"}`, kindUse},
		{`{"type":"frameAck2"}`, kindUse},
		{`{"type":" ping"}`, kindUse},
		{`{"type":ping}`, kindUse},
		{`{"type":["ping"]}`, kindUse},
		{`{"type":null}`, kindUse},
		{`{"type":"resize","cols":80,"rows":24}`, kindUse},
		{`{"type":"activity","active":true}`, kindUse},
		{`{"type":"chat","chat_id":"c"}`, kindUse},
		{`{"type":"subscribe","path":"/home/user"}`, kindUse},
		{`{"type":"input","sequence":1,"data":"x"}`, kindUse},
		{`{"type":"eof"}`, kindUse},
		{`{"type":"auth","token":"x"}`, kindUse},
		{`{"Type":"ping"}`, kindUse},
		{`{"TYPE":"ping"}`, kindUse},
		{`{"type ":"ping"}`, kindUse},
		{`{'type':'ping'}`, kindUse},
		// The type is not the first member.
		{`{"cursor":1,"type":"ack"}`, kindUse},
		{`{"a":{"type":"ping"}}`, kindUse},
		// An escape sequence in the key or in the value.
		{`{"\u0074ype":"ping"}`, kindUse},
		{`{"type":"p\u0069ng"}`, kindUse},
		// White space that is not white space of JSON.
		{"\v{\"type\":\"ping\"}", kindUse},
		{"\f{\"type\":\"ping\"}", kindUse},
		{"\xc2\xa0{\"type\":\"ping\"}", kindUse},
		// A byte order mark.
		{"\xef\xbb\xbf{\"type\":\"ping\"}", kindUse},
		{`[{"type":"ping"}]`, kindUse},
		{`"ping"`, kindUse},
		{`ping`, kindUse},
		{`x`, kindUse},
		{"\x00", kindUse},
	}
	for _, c := range cases {
		if got := automaticKind([]byte(c.head)); got != c.want {
			t.Errorf("automaticKind(%q) = %d, want %d", c.head, got, c.want)
		}
	}
}

// The visibility message of the interactive terminal, with the condition of
// Open Terminal: a JSON object with the type "activity" and a boolean value.
func TestVisibility(t *testing.T) {
	cases := []struct {
		msg        string
		active, ok bool
	}{
		{`{"type":"activity","active":true}`, true, true},
		{`{"type":"activity","active":false}`, false, true},
		{`{"active":true,"type":"activity"}`, true, true},
		{` { "type" : "activity" , "active" : true , "x" : 1 } `, true, true},
		{`{"type":"activity","active":"yes"}`, false, false},
		{`{"type":"activity","active":1}`, false, false},
		{`{"type":"activity","active":null}`, false, false},
		{`{"type":"activity","active":"true"}`, false, false},
		{`{"type":"activity"}`, false, false},
		{`{"type":"Activity","active":true}`, false, false},
		{`{"Type":"activity","active":true}`, false, false},
		{`{"type":"activity","Active":true}`, false, false},
		{`{"type":"resize","active":true}`, false, false},
		{`{"type":["activity"],"active":true}`, false, false},
		{`{"active":true}`, false, false},
		{`{"a":{"type":"activity","active":true}}`, false, false},
		{`[{"type":"activity","active":true}]`, false, false},
		{`{"type":"activity","active":true`, false, false},
		{`{"type":"activity","active":true} x`, false, false},
		{`null`, false, false},
		{`true`, false, false},
		{``, false, false},
		{`x`, false, false},
	}
	for _, c := range cases {
		if active, ok := visibility([]byte(c.msg)); active != c.active || ok != c.ok {
			t.Errorf("visibility(%q) = %v, %v; want %v, %v", c.msg, active, ok, c.active, c.ok)
		}
	}
}

// countLease counts the calls of Active.
type countLease struct {
	backend.Lease
	calls int
}

func (l *countLease) Active() { l.calls++ }

// The decision for a message does not depend on the sizes of the parts in
// which the message comes. Warren examines a maximum of 64 bytes for the
// kind of a text message, and it reads the visibility only from a message of
// a maximum of 64 bytes.
func TestClientUse(t *testing.T) {
	text, binary := websocket.TextMessage, websocket.BinaryMessage
	pad := func(n int, msg string) string { return strings.Repeat(" ", n-len(msg)) + msg }
	type message struct {
		kind int
		data string
		// calls is the number of calls of Active that the message adds.
		calls int
	}
	visible := message{text, `{"type":"activity","active":true}`, 1}
	hidden := message{text, `{"type":"activity","active":false}`, 1}
	ack := `{"type":"ack","cursor":5}`
	cases := map[string][]message{
		"ping":             {{text, `{"type":"ping"}`, 0}},
		"frameAck":         {{text, `{"type":"frameAck","window":3}`, 0}},
		"ack, not visible": {{text, ack, 0}},
		"binary":           {{binary, "x", 1}},
		"empty binary":     {{binary, "", 1}},
		"empty text":       {{text, "", 1}},
		"large binary":     {{binary, strings.Repeat("\x00", 100000), 1}},
		// Bytes that are an automatic message as text are a use message as
		// binary data.
		"binary ping":  {{binary, `{"type":"ping"}`, 1}},
		"resize":       {{text, `{"type":"resize","cols":80,"rows":24}`, 1}},
		"not JSON":     {{text, "hello", 1}},
		"large text":   {{text, strings.Repeat("x", 100000), 1}},
		"large object": {{text, `{"type":"input","data":"` + strings.Repeat("x", 100000) + `"}`, 1}},
		// The size of an automatic message has no effect.
		"ping of 257 bytes":        {{text, `{"type":"ping","pad":"` + strings.Repeat("x", 233) + `"}`, 0}},
		"ack with a large cursor":  {{text, `{"type":"ack","cursor":"` + strings.Repeat("9", 300) + `"}`, 0}},
		"ping of exactly 64 bytes": {{text, `{"type":"ping","pad":"` + strings.Repeat("x", 39) + `"}`, 0}},
		"ping of exactly 65 bytes": {{text, `{"type":"ping","pad":"` + strings.Repeat("x", 40) + `"}`, 0}},
		// The type must end in the first 64 bytes.
		"type ends at byte 64": {{text, pad(64, `{"type":"ping"`) + "}", 0}},
		"type ends at byte 65": {{text, pad(65, `{"type":"ping"`) + "}", 1}},
		"64 spaces, then ping": {{text, pad(79, `{"type":"ping"}`), 1}},
		// An ack is a use only while the client reports that it is visible.
		"visible, ack":                                {visible, {text, ack, 1}, {text, ack, 1}},
		"hidden, ack":                                 {hidden, {text, ack, 0}},
		"visible, hidden, ack":                        {visible, hidden, {text, ack, 0}},
		"hidden, visible, ack":                        {hidden, visible, {text, ack, 1}},
		"visible, ping":                               {visible, {text, `{"type":"ping"}`, 0}},
		"visible, frameAck":                           {visible, {text, `{"type":"frameAck","window":3}`, 0}},
		"visible, large ack":                          {visible, {text, `{"type":"ack","cursor":"` + strings.Repeat("9", 300) + `"}`, 1}},
		"value that is not a boolean":                 {{text, `{"type":"activity","active":"yes"}`, 1}, {text, ack, 0}},
		"visible, then a value that is not a boolean": {visible, {text, `{"type":"activity","active":"yes"}`, 1}, {text, ack, 1}},
		// A visibility message of more than 64 bytes does not change the mark.
		"visibility of 64 bytes":           {{text, pad(64, `{"type":"activity","active":true}`), 1}, {text, ack, 1}},
		"visibility of 65 bytes":           {{text, pad(65, `{"type":"activity","active":true}`), 1}, {text, ack, 0}},
		"visible, then hidden in 65 bytes": {visible, {text, pad(65, `{"type":"activity","active":false}`), 1}, {text, ack, 1}},
		// Binary data does not change the mark.
		"binary visibility": {{binary, `{"type":"activity","active":true}`, 1}, {text, ack, 0}},
	}
	for name, messages := range cases {
		// The sizes of the parts: the full message, and parts of 1, 7, 63, 64,
		// and 65 bytes.
		for _, size := range []int{0, 1, 7, 63, 64, 65} {
			lease := &countLease{}
			use := &clientUse{lease: lease}
			want := 0
			for i, m := range messages {
				use.start(m.kind)
				data := []byte(m.data)
				for len(data) > 0 {
					n := len(data)
					if size > 0 && size < n {
						n = size
					}
					use.data(data[:n])
					data = data[n:]
				}
				use.end()
				want += m.calls
				if lease.calls != want {
					t.Errorf("%s, parts of %d bytes: calls of Active after message %d = %d, want %d", name, size, i+1, lease.calls, want)
				}
			}
		}
	}
}

// socketPair returns the two ends of one WebSocket connection: the end of the
// server that accepted it, and the end of the peer that connected.
func socketPair(t *testing.T) (accepted, connected *websocket.Conn) {
	t.Helper()
	conns := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		conns <- conn
	}))
	t.Cleanup(srv.Close)
	connected, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	accepted = <-conns
	t.Cleanup(func() {
		_ = accepted.Close()
		_ = connected.Close()
	})
	return accepted, connected
}

// hookReader runs a function one time, before its first read.
type hookReader struct {
	io.Reader
	hook func()
}

func (h *hookReader) Read(p []byte) (int, error) {
	if h.hook != nil {
		h.hook()
		h.hook = nil
	}
	return h.Reader.Read(p)
}

// Warren can send its close messages while the relay copies one message of
// the client. After the close message to the sandbox, each write to the
// sandbox fails. That failure must not stop the relay: it must continue to
// read the client until the client answers the close message. If nothing
// reads the client, the connection closes with data that nothing read, and
// the client can lose the close message and its code.
//
// A write that fails before the close messages of warren shows a sandbox that
// went away. That result does not change.
func TestRelayReadsTheClientAfterTheCloseMessages(t *testing.T) {
	cases := []struct {
		name string
		// size is the size of the message in the copy. A small message stays
		// in the write buffer until its end, thus the end of the message
		// fails. A large message fails at the write of a part.
		size int
		// closing is false for a sandbox connection that fails before warren
		// sends its close messages.
		closing bool
		want    closeInfo
	}{
		{"small message", 5, true, closeInfo{side: sideClient, code: websocket.CloseNormalClosure}},
		{"large message", 200 << 10, true, closeInfo{side: sideClient, code: websocket.CloseNormalClosure}},
		{"small message, no close messages", 5, false, closeInfo{side: sideSandbox, code: websocket.CloseAbnormalClosure, writeFailed: true}},
		{"large message, no close messages", 200 << 10, false, closeInfo{side: sideSandbox, code: websocket.CloseAbnormalClosure, writeFailed: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, browser := socketPair(t)
			// Warren connects to the sandbox, thus its end is the end that
			// connected.
			_, sandbox := socketPair(t)
			p := &Proxy{}
			p.opts.WriteTimeout = 5 * time.Second
			lease := &countLease{}
			s := &session{proxy: p, client: client, sandbox: sandbox, lease: lease}

			// The client sends two messages, and then it answers the close
			// message of warren.
			sent := make(chan error, 1)
			go func() {
				err := browser.WriteMessage(websocket.BinaryMessage, make([]byte, c.size))
				if err == nil {
					err = browser.WriteMessage(websocket.BinaryMessage, []byte("more input"))
				}
				if err == nil {
					err = browser.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(5*time.Second))
				}
				sent <- err
			}()
			_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
			mt, reader, err := client.NextReader()
			if err != nil {
				t.Fatal(err)
			}
			// The relay examined the mark for this message, and it has the
			// writer of the sandbox connection. The close message comes at
			// this time.
			stop := func() {
				s.closing.Store(c.closing)
				closeConn(sandbox, websocket.CloseGoingAway, "")
			}
			got := s.copyMessages(sandbox, client, true, mt, &hookReader{Reader: reader, hook: stop})
			if got != c.want {
				t.Errorf("end of the relay = %+v, want %+v", got, c.want)
			}
			if c.closing {
				if err := <-sent; err != nil {
					t.Errorf("the client could not send: %v", err)
				}
			}
			// Only the message in the copy at the time of the stop is a use.
			if lease.calls != 1 {
				t.Errorf("calls of Active = %d, want 1", lease.calls)
			}
		})
	}
}
