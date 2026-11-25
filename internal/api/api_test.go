package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/kube"
	"github.com/h3xcat/warren/internal/policy"
	"github.com/h3xcat/warren/internal/scope"
)

// fakeInstances records the calls of the handler.
type fakeInstances struct {
	ready    bool
	status   string
	err      error
	calls    []string
	requests []backend.Request
	waited   bool
}

func (f *fakeInstances) InstanceStatus(scope.Ref) string { return f.status }
func (f *fakeInstances) Instances(string) []kube.Instance {
	return []kube.Instance{{ID: "wr-1", ContextID: "user:alice", Status: "running", CreatedBy: "alice"}}
}
func (f *fakeInstances) Start(_ context.Context, req backend.Request) error {
	f.calls, f.requests = append(f.calls, "start"), append(f.requests, req)
	return f.err
}
func (f *fakeInstances) Stop(_ context.Context, ref scope.Ref) error {
	f.calls, f.requests = append(f.calls, "stop"), append(f.requests, backend.Request{Ref: ref})
	return f.err
}
func (f *fakeInstances) Restart(_ context.Context, req backend.Request) error {
	f.calls, f.requests = append(f.calls, "restart"), append(f.requests, req)
	return f.err
}
func (f *fakeInstances) Delete(_ context.Context, ref scope.Ref) error {
	f.calls, f.requests = append(f.calls, "delete"), append(f.requests, backend.Request{Ref: ref})
	return f.err
}
func (f *fakeInstances) WaitReady(context.Context, scope.Ref) { f.waited = true }
func (f *fakeInstances) Ready() bool                          { return f.ready }

func newHandler(t *testing.T, inst *fakeInstances) *Handler {
	t.Helper()
	set, err := policy.NewSet([]policy.Policy{
		{ID: "personal", Name: "Personal", Scope: scope.KindUser},
		{ID: "by-chat", Name: "Chat", Scope: scope.KindChat},
		{ID: "team", Name: "Team", Scope: scope.KindShared},
		{ID: "limited", Name: "Limited", Scope: scope.KindUser, Users: []string{"alice"}},
		{ID: "tuned", Name: "Tuned", Scope: scope.KindChat, Spec: policy.Spec{
			Image: "registry.example/tuned:2",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("3Gi"),
			}},
			Env: map[string]string{"SECRET_LOOKING": "value-that-must-not-show"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{Policies: set, Instances: inst, DefaultImage: "registry.example/default:1"}
}

func do(h *Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

const base = "/api/v1/terminals/templates/"

func TestInstanceOperations(t *testing.T) {
	inst := &fakeInstances{ready: true, status: "running"}
	h := newHandler(t, inst)

	// The body of Open WebUI has the three keys. A key with no value is null.
	rec, out := do(h, "POST", base+"personal/start", `{"user_id":"alice","chat_id":null,"model_id":null}`)
	if rec.Code != 200 || out["status"] != "running" || !inst.waited {
		t.Errorf("start = %d %v, waited = %v", rec.Code, out, inst.waited)
	}
	if got := inst.requests[0]; got.UserID != "alice" || got.Ref.ContextID() != "user:alice" || got.Ref.TemplateID != "personal" {
		t.Errorf("start request = %+v", got)
	}
	// For the shared sandbox of a template, Open WebUI sends the ID of the
	// manager. The context is "default".
	do(h, "POST", base+"team/restart", `{"user_id":"manager"}`)
	if got := inst.requests[1]; got.Ref.ContextID() != "default" || got.UserID != "manager" {
		t.Errorf("restart request = %+v", got)
	}
	do(h, "POST", base+"by-chat/stop", `{"user_id":"manager","chat_id":"c1"}`)
	do(h, "DELETE", base+"by-chat/instance", `{"user_id":"manager","chat_id":"c1"}`)
	// A stop keeps the files. A delete removes them.
	if got := strings.Join(inst.calls, ","); got != "start,restart,stop,delete" {
		t.Errorf("calls = %s", got)
	}
	if got := inst.requests[2].Ref.ContextID(); got != "chat:c1" {
		t.Errorf("stop context = %s", got)
	}

	rec, _ = do(h, "GET", base+"personal/instances", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"context_id":"user:alice"`) {
		t.Errorf("instances = %d %s", rec.Code, rec.Body)
	}
	// The ID of the user that started a sandbox is not in the list.
	if strings.Contains(rec.Body.String(), "created") {
		t.Errorf("instances shows more data than Open WebUI reads: %s", rec.Body)
	}
}

func TestInstanceOperationErrors(t *testing.T) {
	cases := []struct {
		name   string
		inst   fakeInstances
		method string
		path   string
		body   string
		status int
	}{
		{"limit of the user", fakeInstances{ready: true, err: backend.ErrLimit}, "POST", "personal/start", `{"user_id":"a"}`, 429},
		{"no capacity", fakeInstances{ready: true, err: backend.ErrCapacity}, "POST", "personal/start", `{"user_id":"a"}`, 503},
		{"backend not ready", fakeInstances{ready: true, err: backend.ErrNotReady}, "POST", "personal/restart", `{"user_id":"a"}`, 503},
		{"stop that Kubernetes refuses", fakeInstances{ready: true, err: errors.New("forbidden")}, "POST", "personal/stop", `{"user_id":"a"}`, 502},
		{"delete that Kubernetes refuses", fakeInstances{ready: true, err: errors.New("forbidden")}, "DELETE", "personal/instance", `{"user_id":"a"}`, 502},
		{"cache not ready, operation", fakeInstances{ready: false}, "POST", "personal/start", `{"user_id":"a"}`, 503},
		{"cache not ready, status", fakeInstances{ready: false}, "GET", "personal/instance?user_id=a", "", 503},
		{"cache not ready, list", fakeInstances{ready: false}, "GET", "personal/instances", "", 503},
		{"chat scope with no chat", fakeInstances{ready: true}, "POST", "by-chat/start", `{"user_id":"a"}`, 409},
		{"no user", fakeInstances{ready: true}, "POST", "personal/start", `{}`, 409},
		{"body is not JSON", fakeInstances{ready: true}, "POST", "personal/start", `user_id=a`, 400},
		{"control character", fakeInstances{ready: true}, "POST", "personal/start", `{"user_id":"a\u0000b"}`, 400},
		{"volume limit", fakeInstances{ready: true, err: backend.ErrVolumeLimit}, "POST", "personal/start", `{"user_id":"a"}`, 429},
		{"reset that Kubernetes refuses", fakeInstances{ready: true, err: errors.New("forbidden")}, "POST", "personal/reset", `{"user_id":"a"}`, 502},
		{"reset with the wrong method", fakeInstances{ready: true}, "GET", "personal/reset", "", 405},
		{"start for a user that the policy does not permit", fakeInstances{ready: true}, "POST", "limited/start", `{"user_id":"mallory"}`, 403},
		{"restart for a user that the policy does not permit", fakeInstances{ready: true}, "POST", "limited/restart", `{"user_id":"mallory"}`, 403},
		{"wrong method", fakeInstances{ready: true}, "PUT", "personal/start", `{"user_id":"a"}`, 405},
	}
	for _, c := range cases {
		inst := c.inst
		rec, out := do(newHandler(t, &inst), c.method, base+c.path, c.body)
		if rec.Code != c.status {
			t.Errorf("%s: status = %d, want %d (%s)", c.name, rec.Code, c.status, rec.Body)
		}
		// Open WebUI reads the body of each response as JSON.
		if out == nil || (rec.Code >= 300 && out["detail"] == "") {
			t.Errorf("%s: the body is not a JSON object with a detail field: %s", c.name, rec.Body)
		}
		if rec.Code >= 400 && rec.Code != 502 && len(inst.calls) != 0 && inst.err == nil {
			t.Errorf("%s: the handler called the backend: %v", c.name, inst.calls)
		}
	}
}

// The template read calls are in the path of each user request. They must
// answer also when the data about the sandboxes is not ready.
func TestTemplateReadsNeedNoSandboxData(t *testing.T) {
	h := newHandler(t, &fakeInstances{ready: false})
	for _, path := range []string{"/api/v1/terminals/templates", base + "personal", "/api/v1/terminals/status"} {
		if rec, _ := do(h, http.MethodGet, path, ""); rec.Code != 200 {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
	}
}

func TestResetOperation(t *testing.T) {
	inst := &fakeInstances{ready: true, status: "stopped"}
	h := newHandler(t, inst)
	// A reset starts nothing, thus the handler does not wait for a sandbox.
	rec, out := do(h, "POST", base+"personal/reset", `{"user_id":"alice"}`)
	if rec.Code != 200 || out["status"] != "stopped" || inst.waited {
		t.Errorf("reset = %d %v, waited = %v", rec.Code, out, inst.waited)
	}
	if len(inst.calls) != 1 || inst.calls[0] != "delete" || inst.requests[0].Ref.ContextID() != "user:alice" {
		t.Errorf("calls = %v, requests = %+v", inst.calls, inst.requests)
	}
}

// A policy with a user list gives a sandbox of the user scope only to the
// users of the list. The stop and status operations stay possible, because
// they give no access.
func TestUserListOnInstanceOperations(t *testing.T) {
	inst := &fakeInstances{ready: true, status: "running"}
	h := newHandler(t, inst)
	if rec, _ := do(h, "POST", base+"limited/start", `{"user_id":"alice"}`); rec.Code != 200 {
		t.Errorf("start for a permitted user = %d", rec.Code)
	}
	if rec, _ := do(h, "POST", base+"limited/stop", `{"user_id":"mallory"}`); rec.Code != 200 {
		t.Errorf("stop for a user that is not in the list = %d, want 200", rec.Code)
	}
	if rec, _ := do(h, "GET", base+"limited/instance?user_id=mallory", ""); rec.Code != 200 {
		t.Errorf("status for a user that is not in the list = %d, want 200", rec.Code)
	}
	// A reset deletes the data of a user that was in the list before.
	if rec, _ := do(h, "POST", base+"limited/reset", `{"user_id":"mallory"}`); rec.Code != 200 {
		t.Errorf("reset for a user that is not in the list = %d, want 200", rec.Code)
	}
	if got := strings.Join(inst.calls, ","); got != "start,stop,delete" {
		t.Errorf("calls = %s", got)
	}
}

func TestTemplateShowsThePolicySettings(t *testing.T) {
	h := newHandler(t, &fakeInstances{ready: true})
	rec, out := do(h, "GET", base+"tuned", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	data, _ := out["data"].(map[string]any)
	want := map[string]any{
		"name": "Tuned", "scope": "chat", "image": "registry.example/tuned:2",
		// Open WebUI reads the CPU limit as a decimal number.
		"cpu_limit": "0.5", "memory_limit": "3Gi", "storage": "1Gi",
		"network_profile": nil, "env": nil,
	}
	for key, w := range want {
		if got, ok := data[key]; !ok || got != w {
			t.Errorf("%s = %v (present: %v), want %v", key, got, ok, w)
		}
	}
	if strings.Contains(rec.Body.String(), "value-that-must-not-show") || strings.Contains(rec.Body.String(), "SECRET_LOOKING") {
		t.Errorf("the template shows the environment variables: %s", rec.Body)
	}
	// A policy with no image shows the default image of warren, and the
	// built-in limits.
	_, out = do(h, "GET", base+"personal", "")
	data, _ = out["data"].(map[string]any)
	if data["image"] != "registry.example/default:1" || data["cpu_limit"] != "2" || data["memory_limit"] != "2Gi" {
		t.Errorf("defaults = %v", data)
	}
	// The list has the same data.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/terminals/templates", nil))
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 5 {
		t.Fatalf("list = %s (%v)", rec.Body, err)
	}
	for _, item := range list {
		data, _ := item["data"].(map[string]any)
		if data["image"] == nil || data["cpu_limit"] == nil || data["memory_limit"] == nil {
			t.Errorf("list item %v has no image or limits", item["id"])
		}
	}
}
