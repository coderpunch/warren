package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/coderpunch/warren/internal/backend"
	"github.com/coderpunch/warren/internal/scope"
)

// These tests are for the system information of a sandbox. Open WebUI reads
// it again each 5 seconds while a person has it open (docs/protocol.md,
// section 4.1.1). The request must get data only from a sandbox that
// operates. It must not start a sandbox.

// stoppedBody is the exact reply for a sandbox that does not operate. Open
// WebUI shows the text of the detail field to the user.
const stoppedBody = `{"detail":"The terminal is stopped. It starts when you use it."}`

// requests returns the backend requests of the test.
func (m *mapBackend) requests() []backend.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]backend.Request(nil), m.calls...)
}

// terminalRequests returns the number of requests that all terminals got.
func (e *env) terminalRequests() int {
	return len(e.alice.seen()) + len(e.bob.seen()) + len(e.shared.seen()) + len(e.chat.seen()) + len(e.model.seen())
}

func TestSystemInfoComesFromARunningSandbox(t *testing.T) {
	e := newEnv(t)
	resp := e.request("GET", "/system/info", "alice", func(r *http.Request) { r.Header.Set("X-Session-Id", "chat-1") })
	if b := body(t, resp); resp.StatusCode != http.StatusOK || b != systemInfoBody {
		t.Fatalf("status = %d, body = %s, want 200 and the body of the sandbox", resp.StatusCode, b)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want the header of the sandbox", got)
	}
	calls := e.resolver.requests()
	if len(calls) != 1 || !calls[0].Passive || calls[0].WebSocket || calls[0].UserID != "alice" {
		t.Errorf("backend requests = %+v, want one passive request of alice", calls)
	}
	if n := len(e.alice.seen()); n != 1 {
		t.Fatalf("the sandbox got %d requests, want 1", n)
	}
	got := e.alice.last()
	if got.Method != "GET" || got.URL.Path != "/system/info" {
		t.Errorf("the sandbox got %s %s", got.Method, got.URL.Path)
	}
	for name, want := range map[string]string{
		"Authorization": "Bearer pod-key-alice", "X-User-Id": "alice", "X-Session-Id": "chat-1",
	} {
		if got.Header.Get(name) != want {
			t.Errorf("%s for the sandbox = %q, want %q", name, got.Header.Get(name), want)
		}
	}
	waitFor(t, "release of the lease", func() bool { return e.resolver.openLeases() == 0 })
}

func TestSystemInfoOfAStoppedSandbox(t *testing.T) {
	e := newEnv(t)
	// carol has no sandbox.
	resp := e.request("GET", "/system/info", "carol", nil)
	// The reply has one line: the JSON object, then a line feed, as each
	// error reply of warren.
	if b := body(t, resp); resp.StatusCode != http.StatusConflict || b != stoppedBody+"\n" {
		t.Fatalf("status = %d, body = %q, want 409 and %s", resp.StatusCode, b, stoppedBody)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want no such header: the sandbox does not start", got)
	}
	calls := e.resolver.requests()
	if len(calls) != 1 || !calls[0].Passive {
		t.Errorf("backend requests = %+v, want one passive request", calls)
	}
	if n := e.terminalRequests(); n != 0 {
		t.Errorf("%d requests went to a terminal", n)
	}
	waitFor(t, "audit record", func() bool { return strings.Contains(e.logs.String(), `"detail":"sandbox stopped"`) })
	logs := e.logs.String()
	if !strings.Contains(logs, `"status":409`) {
		t.Errorf("the audit record does not have the status 409:\n%s", logs)
	}
	// A stopped sandbox is not a failure of warren.
	if strings.Contains(logs, `"level":"WARN"`) || strings.Contains(logs, `"level":"ERROR"`) {
		t.Errorf("warren wrote a warning for a stopped sandbox:\n%s", logs)
	}

	// A request that is not passive gets the reply of a sandbox that starts.
	resp = e.request("GET", "/files/cwd", "carol", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("request that is not passive: status = %d, want 503", resp.StatusCode)
	}
}

// While a different request starts the sandbox, the system information
// waits. Open WebUI stops its timer at each error status, thus an immediate
// error would stay on the screen after the sandbox is ready.
func TestSystemInfoWhileTheSandboxStarts(t *testing.T) {
	e := newEnv(t)
	block := make(chan struct{})
	e.resolver.mu.Lock()
	e.resolver.block = block
	e.resolver.mu.Unlock()

	done := make(chan *http.Response, 1)
	go func() { done <- e.request("GET", "/system/info", "alice", nil) }()
	waitFor(t, "start of the backend call", func() bool { return e.resolver.callCount() == 1 })
	select {
	case resp := <-done:
		t.Fatalf("the request did not wait for the sandbox: status = %d", resp.StatusCode)
	case <-time.After(100 * time.Millisecond):
	}
	// The sandbox is ready.
	close(block)
	resp := <-done
	if b := body(t, resp); resp.StatusCode != http.StatusOK || b != systemInfoBody {
		t.Errorf("after the start: status = %d, body = %s, want 200 and the body of the sandbox", resp.StatusCode, b)
	}
	if calls := e.resolver.requests(); len(calls) != 1 || !calls[0].Passive {
		t.Errorf("backend requests = %+v, want one passive request", calls)
	}

	// The sandbox of bob is not ready at the end of the wait.
	name := scope.Ref{TemplateID: "personal", Kind: scope.KindUser, Value: "bob"}.Name()
	e.resolver.mu.Lock()
	e.resolver.block = nil
	e.resolver.errors = map[string]error{name: backend.ErrNotReady}
	e.resolver.mu.Unlock()
	resp = e.request("GET", "/system/info", "bob", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "5" {
		t.Errorf("end of the wait: status = %d, Retry-After = %q, want 503 and 5", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if b := body(t, resp); strings.Contains(b, "stopped") {
		t.Errorf("end of the wait: body = %s, want the text of a terminal that is not ready", b)
	}
	if n := len(e.bob.seen()); n != 0 {
		t.Errorf("the terminal of bob got %d requests", n)
	}
}

// The checks of the route come before the passive rule, and they do not
// call the backend.
func TestSystemInfoRouteErrors(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name   string
		user   string
		mod    func(*http.Request)
		status int
		detail string
	}{
		{"unknown template", "alice", func(r *http.Request) { r.Header.Set("X-Terminal-Template-Id", "absent") }, 404, "Template not found"},
		{"no template", "alice", func(r *http.Request) { r.Header.Del("X-Terminal-Template-Id") }, 400, "X-Terminal-Template-Id is necessary"},
		{"user not in the list", "bob", func(r *http.Request) { r.Header.Set("X-Terminal-Template-Id", "limited") }, 403, "The policy of this terminal does not permit this user"},
		{"chat scope with no chat", "alice", func(r *http.Request) { r.Header.Set("X-Terminal-Template-Id", "by-chat") }, 409, ""},
		{"no key", "alice", func(r *http.Request) { r.Header.Del("Authorization") }, 401, "Invalid API key"},
		{"dot segment", "alice", func(r *http.Request) { r.URL.Path = "/x/../system/info" }, 400, "Invalid path"},
	}
	for _, c := range cases {
		resp := e.request("GET", "/system/info", c.user, c.mod)
		var msg map[string]string
		_ = json.Unmarshal([]byte(body(t, resp)), &msg)
		if resp.StatusCode != c.status || msg["detail"] == "" || (c.detail != "" && msg["detail"] != c.detail) {
			t.Errorf("%s: status = %d, detail = %q; want %d and %q", c.name, resp.StatusCode, msg["detail"], c.status, c.detail)
		}
		// The status 409 has two meanings. The text tells them apart.
		if strings.Contains(msg["detail"], "stopped") {
			t.Errorf("%s: detail = %q, want the text of the route error", c.name, msg["detail"])
		}
	}
	if n := e.resolver.callCount(); n != 0 {
		t.Errorf("backend calls = %d, want 0", n)
	}
	if n := e.terminalRequests(); n != 0 {
		t.Errorf("%d refused requests went to a terminal", n)
	}

	// A WebSocket has no route on this path, and it gets no sandbox.
	conn, resp, err := e.dial("/system/info", "alice", http.Header{"Authorization": {"Bearer " + connectionKey}})
	if err == nil {
		conn.Close()
		t.Fatal("warren accepted a WebSocket on the path of the system information")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("WebSocket: response = %v, want status 404", resp)
	}
	if n := e.resolver.callCount(); n != 0 {
		t.Errorf("backend calls after the WebSocket = %d, want 0", n)
	}
}

// Open Terminal has only the GET method on this path. Warren gives the
// reply for each other method, thus no method starts a sandbox.
func TestSystemInfoMethods(t *testing.T) {
	e := newEnv(t)
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		// alice has a sandbox, and carol has none.
		for _, user := range []string{"alice", "carol"} {
			resp := e.request(method, "/system/info", user, nil)
			if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET" {
				t.Errorf("%s as %s: status = %d, Allow = %q; want 405 and GET", method, user, resp.StatusCode, resp.Header.Get("Allow"))
			}
			if b := body(t, resp); method != "HEAD" && b != `{"detail":"Method not allowed"}`+"\n" {
				t.Errorf("%s as %s: body = %q", method, user, b)
			}
		}
	}
	if n := e.resolver.callCount(); n != 0 {
		t.Errorf("backend calls = %d, want 0", n)
	}
	if n := e.terminalRequests(); n != 0 {
		t.Errorf("%d requests went to a terminal", n)
	}
}

// The passive rule is for the exact decoded path, with each query string.
// Each other request is a use of the sandbox.
func TestOnlySystemInfoIsPassive(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		method, path string
		passive      bool
	}{
		{"GET", "/system/info", true},
		{"GET", "/system/info?x=1", true},
		{"GET", "/system/info?user_id=bob", true},
		// The rule uses the decoded path, as the router of Open Terminal.
		{"GET", "/system%2Finfo", true},
		{"GET", "/system", false},
		{"GET", "/system/", false},
		{"GET", "/system/info/", false},
		{"GET", "/system/info/x", false},
		{"GET", "/system/information", false},
		{"GET", "/System/Info", false},
		{"GET", "//system/info", false},
		{"GET", "/info", false},
		{"GET", "/files/cwd", false},
		{"GET", "/api/config", false},
		{"GET", "/openapi.json", false},
		{"POST", "/execute", false},
		{"PATCH", "/browser/sessions/abc", false},
	}
	for _, c := range cases {
		before := e.resolver.callCount()
		e.request(c.method, c.path, "alice", nil)
		calls := e.resolver.requests()
		if len(calls) != before+1 {
			t.Errorf("%s %s: backend calls = %d, want 1", c.method, c.path, len(calls)-before)
			continue
		}
		if got := calls[len(calls)-1].Passive; got != c.passive {
			t.Errorf("%s %s: passive = %v, want %v", c.method, c.path, got, c.passive)
		}
	}

	// A WebSocket is not passive.
	before := e.resolver.callCount()
	conn := e.authenticatedSocket(t, "/events", "alice")
	defer conn.Close()
	waitFor(t, "backend call of the WebSocket", func() bool { return e.resolver.callCount() == before+1 })
	if last := e.resolver.requests()[before]; last.Passive || !last.WebSocket {
		t.Errorf("WebSocket: backend request = %+v, want a request that is not passive", last)
	}
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
}
