package server_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/proxy"
	"github.com/h3xcat/warren/internal/scope"
)

// These tests are for defects that an independent review found. Each test
// fails on the code before the correction.

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

// A sandbox must not select the address of the tool calls of Open WebUI,
// and it must not put text into the tool list.
func TestToolListComesFromWarren(t *testing.T) {
	e := newEnv(t)
	resp := e.request("GET", "/openapi.json", "alice", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	text := body(t, resp)
	var doc struct {
		Servers any                       `json:"servers"`
		Paths   map[string]map[string]any `json:"paths"`
		Info    map[string]any            `json:"info"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Servers != nil || strings.Contains(text, "evil.example") {
		t.Errorf("the document has an address of the sandbox: %s", text)
	}
	// The sandbox has three tools that warren knows. All other path keys
	// of the sandbox are not in the result.
	want := map[string]bool{"/execute": true, "/files/list": true, "/execute/{process_id}/status": true}
	for key := range doc.Paths {
		if !want[key] {
			t.Errorf("the document has the path key %q", key)
		}
	}
	for key := range want {
		if doc.Paths[key] == nil {
			t.Errorf("the document lost the path key %q", key)
		}
	}
	// The sandbox has only POST on /execute. The text of the operation
	// comes from warren.
	if doc.Paths["/execute"]["get"] != nil {
		t.Error("the document has an operation that the sandbox does not have")
	}
	if op, _ := doc.Paths["/execute"]["post"].(map[string]any); op["operationId"] == "run_command" && op["description"] == nil {
		t.Errorf("the operation of /execute comes from the sandbox: %v", op)
	}
	if doc.Info["title"] != "Open Terminal" {
		t.Errorf("the info object comes from the sandbox: %v", doc.Info)
	}
	if got := e.alice.last().Header.Get("Authorization"); got != "Bearer pod-key-alice" {
		t.Errorf("the request for the document had the key %q", got)
	}

	// A sandbox that is not ready: warren sends its complete tool list
	// immediately. Open WebUI stops the chat turn after 10 seconds.
	start := time.Now()
	resp = e.request("GET", "/openapi.json", "carol", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("sandbox not ready: status = %d", resp.StatusCode)
	}
	if err := json.Unmarshal([]byte(body(t, resp)), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Paths) < 10 || doc.Paths["/files/read"] == nil {
		t.Errorf("sandbox not ready: the tool list has %d paths", len(doc.Paths))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("sandbox not ready: the tool list came after %v", d)
	}
}

// A sandbox must not remove all tools, and its document must not make
// warren use much memory.
func TestToolListWithAHostileSandboxDocument(t *testing.T) {
	e := newEnv(t)
	docs := map[string]string{
		"no paths":          `{"paths":{}}`,
		"unknown paths":     `{"paths":{"/x":{"get":{}}}}`,
		"upper-case method": `{"paths":{"/EXECUTE":{"POST":{}}}}`,
		"not JSON":          `x`,
		"too large":         `{"paths":{"/execute":{"post":{}}},"x":"` + strings.Repeat("A", 300<<10) + `"}`,
	}
	for name, doc := range docs {
		e.alice.setOpenAPI(doc)
		resp := e.request("GET", "/openapi.json", "alice", nil)
		var out struct {
			Paths map[string]any `json:"paths"`
		}
		if err := json.Unmarshal([]byte(body(t, resp)), &out); err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: status %d, %v", name, resp.StatusCode, err)
		}
		if len(out.Paths) < 10 {
			t.Errorf("%s: the tool list has %d paths, want the complete list", name, len(out.Paths))
		}
	}
}

func TestFrameworkPagesAreNotRelayed(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/docs", "/redoc", "/docs/oauth2-redirect"} {
		if resp := e.request("GET", path, "alice", nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, resp.StatusCode)
		}
	}
	if n := len(e.alice.seen()); n != 0 {
		t.Errorf("%d requests for framework pages went to the sandbox", n)
	}
}

// Open WebUI sends the response headers of a terminal server to the
// browser. There they apply to the origin of Open WebUI.
func TestResponseHeaderRules(t *testing.T) {
	e := newEnv(t)
	// A route of Open Terminal: only known headers pass.
	resp := e.request("GET", "/files/headers", "alice", nil)
	for _, name := range append(hostileResponseHeaders, "Content-Security-Policy") {
		if v := resp.Header.Values(name); len(v) != 0 {
			t.Errorf("terminal route: the header %s went to the client: %q", name, v)
		}
	}
	if resp.Header.Get("Content-Type") != "text/plain" || resp.Header.Get("Content-Disposition") == "" {
		t.Errorf("terminal route: a necessary header is absent: %v", resp.Header)
	}
	// An application route: the headers of the application pass, but not
	// the headers that change the origin.
	resp = e.request("GET", "/proxy/3000/headers", "alice", nil)
	for _, name := range hostileResponseHeaders {
		if v := resp.Header.Values(name); len(v) != 0 {
			t.Errorf("application route: the header %s went to the client: %q", name, v)
		}
	}
	if resp.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Error("application route: the content security policy of the sandbox is absent")
	}
}

func TestSetCookieCanBePermitted(t *testing.T) {
	e := newEnvWith(t, func(o *proxy.Options) { o.AllowSetCookie = true })
	if resp := e.request("GET", "/proxy/3000/headers", "alice", nil); resp.Header.Get("Set-Cookie") == "" {
		t.Error("the option did not permit a cookie on an application route")
	}
	if resp := e.request("GET", "/files/headers", "alice", nil); resp.Header.Get("Set-Cookie") != "" {
		t.Error("the option permitted a cookie on a terminal route")
	}
}

// The browser sends a redirected request again, with the session token of
// the user. A sandbox must not select the new address.
func TestRedirectRules(t *testing.T) {
	e := newEnv(t)
	sandbox := e.alice.target().URL.Host
	view := "/browser/view/sid/tok/frame/https/example.com"
	cases := []struct {
		path, to, want string
	}{
		// Terminal routes: only the redirect of the framework passes.
		{"/files/loc", "/api/v1/users/victim", ""},
		{"/files/loc", "//evil.example/x", ""},
		{"/files/loc", "https://evil.example/x", ""},
		{"/files/loc", `/\evil.example/x`, ""},
		{"/files/loc", "../../../api/v1/users/victim", ""},
		{"/files/loc", "http://" + sandbox + "/other", ""},
		{"/files/loc", "http://" + sandbox + "//evil.example/x", ""},
		{"/files/loc", "http://" + sandbox + "/files/loc/", "loc/"},
		{"/files/loc", "http://" + sandbox + "/files/loc/?a=1", "loc/?a=1"},
		{"/files/sub/loc", "http://" + sandbox + "/files/sub/loc/", "loc/"},
		// Port proxy: Open WebUI changes these into the port prefix.
		{"/proxy/3000/loc", "/login", "/login"},
		{"/proxy/3000/loc", "http://localhost:3000/x", "http://localhost:3000/x"},
		{"/proxy/3000/loc", "http://" + sandbox + "/proxy/3000/y", "/proxy/3000/y"},
		{"/proxy/3000/loc", "http://" + sandbox + "//evil.example/x", "/evil.example/x"},
		{"/proxy/3000/loc", `/\evil.example/x`, ""},
		// Browser view: only a relative redirect below the view prefix.
		{view + "/loc", "../other.example/page", "../other.example/page"},
		{view + "/loc", "next", "next"},
		{view + "/loc", "/api/v1/users/victim", ""},
		{view + "/loc", "../../../../../../../api/v1/users/victim", ""},
		{view + "/loc", "../../../../other-token/frame/https/x/", ""},
		{view + "/loc", "https://evil.example/x", ""},
		{view + "/loc", "//evil.example/x", ""},
	}
	for _, c := range cases {
		resp := e.request("DELETE", c.path+"?to="+url.QueryEscape(c.to), "alice", nil)
		got := resp.Header.Get("Location")
		if got != c.want {
			t.Errorf("%s with Location %q: client got %q, want %q", c.path, c.to, got, c.want)
		}
		if strings.Contains(got, sandbox) {
			t.Errorf("%s: the address of the sandbox went to the client: %q", c.path, got)
		}
	}
}

func TestInformationalResponsesAndTrailers(t *testing.T) {
	e := newEnv(t)
	// Read the raw reply, because an HTTP client hides informational
	// responses.
	u, _ := url.Parse(e.url)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, path := range []string{"/early", "/trailer"} {
		io.WriteString(conn, "GET "+path+" HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+connectionKey+
			"\r\nX-User-Id: alice\r\nX-Terminal-Template-Id: personal\r\n\r\n")
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(raw) != "ok" {
			t.Errorf("%s: first response = %d %q, want 200 ok", path, resp.StatusCode, raw)
		}
		if resp.Header.Get("X-Request-Id") == "" {
			t.Errorf("%s: the response has no request ID", path)
		}
		if v := resp.Header.Values("Set-Cookie"); len(v) != 0 {
			t.Errorf("%s: cookie in the header: %q", path, v)
		}
		if len(resp.Trailer) != 0 {
			t.Errorf("%s: the client got the trailer %v", path, resp.Trailer)
		}
	}
	if !strings.Contains(e.logs.String(), `"path":"/early","status":200`) {
		t.Errorf("the audit record of /early has a wrong status:\n%s", e.logs.String())
	}
}

func TestAuditRecordForResponseThatIsNotComplete(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest("GET", e.url+"/cut", nil)
	req.Header.Set("Authorization", "Bearer "+connectionKey)
	req.Header.Set("X-User-Id", "alice")
	req.Header.Set("X-Terminal-Template-Id", "personal")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	waitFor(t, "audit record for the response that was cut", func() bool {
		return strings.Contains(e.logs.String(), `"path":"/cut"`)
	})
	if !strings.Contains(e.logs.String(), `"user":"alice"`) {
		t.Errorf("the record has no user:\n%s", e.logs.String())
	}
}

func TestAuditLogContent(t *testing.T) {
	e := newEnv(t)
	e.request("GET", "/browser/view/sid123/secret-view-token/runtime.js?user_id=x&secret=query-secret", "alice", nil)
	e.request("GET", "/files/list", "alice", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong-key-wrong-key-wrong-key-wrong-key") })
	conn, _, err := e.dial("/browser/view/sid123/secret-view-token/ws", "alice", http.Header{"Authorization": {"Bearer " + connectionKey}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "audit record for the open WebSocket", func() bool {
		return strings.Contains(e.logs.String(), `"msg":"websocket open"`)
	})
	// Warren writes "websocket open" before it connects to the sandbox. A
	// close message in that interval stops the connection, and the audit
	// record then says "dial sandbox". Wait for an echo: it shows that the
	// relay to the sandbox operates.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, data, err := conn.ReadMessage(); err != nil || string(data) != "echo:ping" {
		t.Fatalf("echo through the relay = %q, %v", data, err)
	}
	conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
	closeCode(t, conn)
	conn.Close()
	waitFor(t, "audit record for the closed WebSocket", func() bool {
		return strings.Contains(e.logs.String(), "closed by client, code 1000")
	})
	logs := e.logs.String()
	for _, secret := range []string{connectionKey, "pod-key-alice", "secret-view-token", "query-secret", "wrong-key"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log contains the secret %q", secret)
		}
	}
	name := scope.Ref{TemplateID: "personal", Kind: scope.KindUser, Value: "alice"}.Name()
	for _, want := range []string{
		`"path":"/browser/view/sid123/REDACTED/runtime.js"`, `"user":"alice"`, `"template":"personal"`,
		`"context":"user:alice"`, `"sandbox":"` + name + `"`, `"status":401`, `"status":101`, `"log":"audit"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log does not contain %s", want)
		}
	}
}

// The chat scope and the model scope must use the headers that Open WebUI
// verified, and not the X-Session-Id header that the browser selects.
func TestChatAndModelScopes(t *testing.T) {
	e := newEnv(t)
	resp := e.request("GET", "/files/list", "alice", func(r *http.Request) {
		r.Header.Set("X-Terminal-Template-Id", "by-chat")
		r.Header.Set("X-Terminal-Chat-Id", "chat-7")
		r.Header.Set("X-Session-Id", "other-chat")
	})
	if resp.StatusCode != 200 || len(e.chat.seen()) != 1 {
		t.Fatalf("chat scope: status %d, chat terminal got %d requests", resp.StatusCode, len(e.chat.seen()))
	}
	// A different user in the same chat uses the same sandbox.
	e.request("GET", "/files/list", "bob", func(r *http.Request) {
		r.Header.Set("X-Terminal-Template-Id", "by-chat")
		r.Header.Set("X-Terminal-Chat-Id", "chat-7")
	})
	if len(e.chat.seen()) != 2 {
		t.Error("chat scope: a second user of the chat did not get the chat sandbox")
	}
	// X-Session-Id alone must not select the chat sandbox.
	resp = e.request("GET", "/files/list", "alice", func(r *http.Request) {
		r.Header.Set("X-Terminal-Template-Id", "by-chat")
		r.Header.Set("X-Session-Id", "chat-7")
	})
	if resp.StatusCode != http.StatusConflict || len(e.chat.seen()) != 2 {
		t.Errorf("chat scope: X-Session-Id selected a sandbox (status %d)", resp.StatusCode)
	}

	resp = e.request("GET", "/files/list", "alice", func(r *http.Request) {
		r.Header.Set("X-Terminal-Template-Id", "by-model")
		r.Header.Set("X-Terminal-Model-Id", "model-7")
	})
	if resp.StatusCode != 200 || len(e.model.seen()) != 1 {
		t.Fatalf("model scope: status %d, model terminal got %d requests", resp.StatusCode, len(e.model.seen()))
	}
	resp = e.request("GET", "/files/list", "alice", func(r *http.Request) { r.Header.Set("X-Terminal-Template-Id", "by-model") })
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("model scope with no model: status = %d, want 409", resp.StatusCode)
	}
	// The backend gets the user for its limits, also when the user is not
	// part of the sandbox reference.
	e.resolver.mu.Lock()
	last := e.resolver.calls[len(e.resolver.calls)-1]
	e.resolver.mu.Unlock()
	if last.UserID != "alice" || last.Ref.Kind != scope.KindModel {
		t.Errorf("backend request = %+v", last)
	}
}

// The sandbox must get an exact set of headers, on HTTP and on a WebSocket.
func TestExactSandboxHeaders(t *testing.T) {
	e := newEnv(t)
	noise := func(h http.Header) {
		h.Set("Cookie", "token=user-session")
		h.Set("X-OpenWebUI-User-Jwt", "a.b.c")
		h.Set("X-Workspace-Id", "w")
		h.Set("X-Custom", "v")
		h.Set("X-Session-Id", "chat-1")
		h.Set("User-Agent", "test-agent")
	}
	e.request("GET", "/files/list", "alice", func(r *http.Request) { noise(r.Header) })
	names := func(h http.Header) string {
		var out []string
		for name := range h {
			out = append(out, name)
		}
		// sort for a stable comparison
		for i := range out {
			for j := i + 1; j < len(out); j++ {
				if out[j] < out[i] {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
		return strings.Join(out, ",")
	}
	if got := names(e.alice.last().Header); got != "Accept-Encoding,Authorization,User-Agent,X-Session-Id,X-User-Id" {
		t.Errorf("HTTP headers for the sandbox = %s", got)
	}

	h := http.Header{}
	noise(h)
	conn, _, err := e.dial("/events", "alice", h)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth","token":"`+connectionKey+`"}`))
	conn.WriteMessage(websocket.TextMessage, []byte("x"))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	got := e.alice.last().Header
	for _, name := range []string{"Cookie", "X-Openwebui-User-Jwt", "X-Workspace-Id", "X-Custom", "Authorization", "X-Terminal-Template-Id", "Origin"} {
		if v := got.Values(name); len(v) != 0 {
			t.Errorf("WebSocket: the sandbox got the header %s: %q", name, v)
		}
	}
	if got.Get("X-User-Id") != "alice" || got.Get("X-Session-Id") != "chat-1" {
		t.Errorf("WebSocket: owner headers = %q, %q", got.Get("X-User-Id"), got.Get("X-Session-Id"))
	}
}

func TestWebSocketBadPathIsRefused(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/api/terminals/..", "/execute/../status", "/api/terminals/a%5Cb"} {
		_, resp, err := e.dial(path, "alice", nil)
		if err == nil || resp == nil || (resp.StatusCode != 400 && resp.StatusCode != 404) {
			t.Errorf("%s: err = %v, response = %v; want status 400 or 404", path, err, resp)
		}
	}
	if n := len(e.alice.seen()); n != 0 {
		t.Errorf("%d upgrades with a bad path went to the terminal", n)
	}
}

func TestUnauthorizedReplyClosesTheConnection(t *testing.T) {
	e := newEnv(t)
	resp := e.request("GET", "/files/list", "alice", func(r *http.Request) { r.Header.Del("Authorization") })
	if !resp.Close {
		t.Error("the connection of a client with no key stays open")
	}
}

func TestLeaseIsReleased(t *testing.T) {
	e := newEnv(t)
	e.request("GET", "/files/list", "alice", nil)
	e.request("GET", "/openapi.json", "alice", nil)
	conn, _, err := e.dial("/events", "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth","token":"`+connectionKey+`"}`))
	conn.WriteMessage(websocket.TextMessage, []byte("x"))
	conn.ReadMessage()
	if n := e.resolver.openLeases(); n != 1 {
		t.Errorf("open leases while the WebSocket is open = %d, want 1", n)
	}
	e.resolver.mu.Lock()
	ws := e.resolver.calls[len(e.resolver.calls)-1].WebSocket
	e.resolver.mu.Unlock()
	if !ws {
		t.Error("the backend request of a WebSocket is not marked")
	}
	conn.Close()
	waitFor(t, "release of all leases", func() bool { return e.resolver.openLeases() == 0 })
}

// A sandbox that stops reading must not hold a relay after the client is
// gone.
func TestWebSocketSandboxThatStopsReading(t *testing.T) {
	e := newEnvWith(t, func(o *proxy.Options) { o.WriteTimeout = 300 * time.Millisecond })
	conn, _, err := e.dial("/execute/stall/status", "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth","token":"`+connectionKey+`"}`))
	// Send until the buffers of the two connections are full. The write
	// of the client then stops, and thus the relay waits in a write to
	// the sandbox.
	chunk := make([]byte, 1<<20)
	blocked := false
	for i := 0; i < 4096 && !blocked; i++ {
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
			blocked = true
		}
	}
	if !blocked {
		t.Skip("the socket buffers of this system are too large for this test")
	}
	conn.Close()
	waitFor(t, "end of the relay to a sandbox that does not read", func() bool { return e.resolver.openLeases() == 0 })
}

// A message that is cut in the middle must not arrive as a complete message.
func TestWebSocketTruncatedMessage(t *testing.T) {
	e := newEnv(t)
	conn, _, err := e.dial("/execute/truncate/status", "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth","token":"`+connectionKey+`"}`))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != websocket.CloseInternalServerErr {
				t.Errorf("end of the connection = %v, want close 1011", err)
			}
			return
		}
		t.Errorf("the client got a complete message of %d bytes", len(data))
	}
}

// rawSandbox is a TCP server that answers a WebSocket upgrade with bytes
// that the test selects.
func rawSandbox(t *testing.T, answer func(conn net.Conn)) backend.Target {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				answer(conn)
			}()
		}
	}()
	u, _ := url.Parse("http://" + ln.Addr().String())
	return backend.Target{URL: u, APIKey: "pod-key-raw"}
}

func (e *env) useTarget(user string, target backend.Target) {
	e.resolver.mu.Lock()
	defer e.resolver.mu.Unlock()
	e.resolver.targets[scope.Ref{TemplateID: "personal", Kind: scope.KindUser, Value: user}.Name()] = target
}

func (e *env) authenticatedSocket(t *testing.T, path, user string) *websocket.Conn {
	t.Helper()
	conn, _, err := e.dial(path, user, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth","token":"`+connectionKey+`"}`))
	return conn
}

func TestWebSocketHandshakeReplyHasASizeLimit(t *testing.T) {
	e := newEnv(t)
	e.useTarget("raw", rawSandbox(t, func(conn net.Conn) {
		// A status line with no end.
		io.WriteString(conn, "HTTP/1.1 101 ")
		junk := []byte(strings.Repeat("A", 64<<10))
		for {
			if _, err := conn.Write(junk); err != nil {
				return
			}
		}
	}))
	start := time.Now()
	conn := e.authenticatedSocket(t, "/events", "raw")
	if code := closeCode(t, conn); code != websocket.CloseInternalServerErr {
		t.Errorf("close code = %d, want 1011", code)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("warren read the reply for %v", d)
	}
	waitFor(t, "audit record", func() bool { return strings.Contains(e.logs.String(), "dial sandbox") })
	for _, line := range strings.Split(e.logs.String(), "\n") {
		if len(line) > 2000 {
			t.Errorf("a log record has %d bytes", len(line))
		}
	}
}

func TestWebSocketRefusesCompressionThatItDidNotOffer(t *testing.T) {
	e := newEnv(t)
	e.useTarget("raw", rawSandbox(t, func(conn net.Conn) {
		// The accept key is not correct, but the test needs only the
		// extension header. Use the library to make a correct reply.
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: x\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
		time.Sleep(2 * time.Second)
	}))
	conn := e.authenticatedSocket(t, "/events", "raw")
	if code := closeCode(t, conn); code != websocket.CloseInternalServerErr {
		t.Errorf("close code = %d, want 1011", code)
	}
}

func TestWebSocketLimitBeforeAuthentication(t *testing.T) {
	e := newEnvWith(t, func(o *proxy.Options) {
		o.MaxPreAuthSockets = 2
		o.AuthTimeout = time.Second
	})
	var held []*websocket.Conn
	for i := 0; i < 2; i++ {
		conn, _, err := e.dial("/events", "alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		held = append(held, conn)
	}
	_, resp, err := e.dial("/events", "alice", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third socket with no key: err = %v, response = %v; want status 503", err, resp)
	}
	// A connection that has the key in its header does not use a place.
	conn, _, err := e.dial("/browser/view/sid/tok/ws", "alice", http.Header{"Authorization": {"Bearer " + connectionKey}})
	if err != nil {
		t.Fatalf("socket with a key in the header: %v", err)
	}
	conn.Close()
	// After authentication, a place is free again.
	held[0].WriteMessage(websocket.TextMessage, []byte(`{"type":"auth","token":"`+connectionKey+`"}`))
	held[0].WriteMessage(websocket.TextMessage, []byte("x"))
	if _, _, err := held[0].ReadMessage(); err != nil {
		t.Fatal(err)
	}
	conn, _, err = e.dial("/events", "alice", nil)
	if err != nil {
		t.Fatalf("socket after a place became free: %v", err)
	}
	conn.Close()
}

func TestShutdownClosesWebSockets(t *testing.T) {
	e := newEnv(t)
	conn := e.authenticatedSocket(t, "/events", "alice")
	conn.WriteMessage(websocket.TextMessage, []byte("x"))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := contextWithTimeout(5 * time.Second)
		defer cancel()
		done <- e.proxy.Shutdown(ctx)
	}()
	if code := closeCode(t, conn); code != websocket.CloseServiceRestart {
		t.Errorf("close code = %d, want 1012", code)
	}
	conn.Close()
	if err := <-done; err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	if !strings.Contains(e.logs.String(), "closed by warren") {
		t.Errorf("no audit record for the WebSocket that warren closed:\n%s", e.logs.String())
	}
	// No new WebSocket after the stop.
	_, resp, err := e.dial("/events", "alice", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("socket after shutdown: err = %v, response = %v; want status 503", err, resp)
	}
}

// While a sandbox starts, warren must see that the client went away.
func TestClientGoesAwayDuringSandboxStart(t *testing.T) {
	e := newEnv(t)
	e.resolver.mu.Lock()
	e.resolver.block = make(chan struct{})
	e.resolver.mu.Unlock()

	conn := e.authenticatedSocket(t, "/events", "alice")
	waitFor(t, "start of the backend call", func() bool { return e.resolver.callCount() == 1 })
	// A ping gets an answer during the wait.
	pong := make(chan struct{}, 1)
	conn.SetPongHandler(func(string) error { pong <- struct{}{}; return nil })
	conn.WriteControl(websocket.PingMessage, []byte("p"), time.Now().Add(time.Second))
	go conn.ReadMessage()
	select {
	case <-pong:
	case <-time.After(3 * time.Second):
		t.Error("no pong while the sandbox starts")
	}
	conn.Close()
	waitFor(t, "audit record after the client went away", func() bool {
		return strings.Contains(e.logs.String(), `"msg":"access"`) && strings.Contains(e.logs.String(), "/events")
	})
}
