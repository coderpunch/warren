package server_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/coderpunch/warren/internal/backend"
	"github.com/coderpunch/warren/internal/scope"
)

// A policy with a user list gives a sandbox only to the users of the list.
// Warren must refuse the other users before it asks for a sandbox: a request
// for a sandbox can start a pod and make a volume.
func TestUserListOfAPolicy(t *testing.T) {
	e := newEnv(t)
	limited := func(r *http.Request) { r.Header.Set("X-Terminal-Template-Id", "limited") }

	if resp := e.request("GET", "/files/cwd", "alice", limited); resp.StatusCode != 200 {
		t.Fatalf("a user of the list: status = %d, want 200", resp.StatusCode)
	}
	calls := e.resolver.callCount()
	for _, path := range []string{"/files/cwd", "/openapi.json", "/execute", "/proxy/8080/"} {
		resp := e.request("GET", path, "bob", limited)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: a user that is not in the list: status = %d, want 403", path, resp.StatusCode)
		}
		// Open WebUI reads the detail field of an error.
		if b := body(t, resp); !strings.Contains(b, `"detail"`) {
			t.Errorf("%s: body = %s", path, b)
		}
	}
	// A user ID is an exact match.
	for _, user := range []string{"Alice", "alice2", "alic"} {
		if resp := e.request("GET", "/files/cwd", user, limited); resp.StatusCode != http.StatusForbidden {
			t.Errorf("user %q: status = %d, want 403", user, resp.StatusCode)
		}
	}
	conn, _, err := e.dial("/events", "bob", http.Header{"X-Terminal-Template-Id": {"limited"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth","token":"`+connectionKey+`"}`))
	if code := closeCode(t, conn); code != 4003 {
		t.Errorf("WebSocket of a user that is not in the list: close code = %d, want 4003", code)
	}
	if got := e.resolver.callCount(); got != calls {
		t.Errorf("warren asked for a sandbox %d times for users that the policy does not permit", got-calls)
	}
	// The list is for one policy. The same user can use a different policy.
	if resp := e.request("GET", "/files/cwd", "bob", nil); resp.StatusCode != 200 {
		t.Errorf("bob on a policy with no list: status = %d, want 200", resp.StatusCode)
	}
}

func TestVolumeLimitGivesAClearError(t *testing.T) {
	e := newEnv(t)
	name := scope.Ref{TemplateID: "personal", Kind: scope.KindUser, Value: "carol"}.Name()
	e.resolver.mu.Lock()
	e.resolver.errors = map[string]error{name: backend.ErrVolumeLimit}
	e.resolver.mu.Unlock()

	resp := e.request("GET", "/files/cwd", "carol", nil)
	if b := body(t, resp); resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(b, "saved files") {
		t.Errorf("status = %d, body = %s", resp.StatusCode, b)
	}
	conn, _, err := e.dial("/events", "carol", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth","token":"`+connectionKey+`"}`))
	if code := closeCode(t, conn); code != 4003 {
		t.Errorf("close code = %d, want 4003", code)
	}
}
