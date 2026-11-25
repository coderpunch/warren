// Package api implements the management API that Open WebUI calls on an
// orchestrator connection (docs/protocol.md, section 4.6).
//
// Templates are read-only. They come from the policy file.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/httpx"
	"github.com/h3xcat/warren/internal/kube"
	"github.com/h3xcat/warren/internal/policy"
	"github.com/h3xcat/warren/internal/scope"
)

// Prefix is the path prefix of the management API. Warren never sends a
// path with this prefix to a sandbox.
const Prefix = "/api/v1/"

const templatesPath = "/api/v1/terminals/templates"

// Policies gives the current policies.
type Policies interface {
	Get(id string) (policy.Policy, bool)
	List() []policy.Policy
}

// Instances gives access to the sandboxes of the templates.
type Instances interface {
	InstanceStatus(ref scope.Ref) string
	Instances(templateID string) []kube.Instance
	Start(ctx context.Context, req backend.Request) error
	Stop(ctx context.Context, ref scope.Ref) error
	Restart(ctx context.Context, req backend.Request) error
	// Delete stops a sandbox and deletes its data. The sandbox is then not
	// in the instance list.
	Delete(ctx context.Context, ref scope.Ref) error
	// Reset stops a sandbox and clears its files. The sandbox stays in the
	// instance list, and it does not start again.
	Reset(ctx context.Context, ref scope.Ref) error
	// WaitReady waits until the sandbox is ready, or until it is clear
	// that it does not start.
	WaitReady(ctx context.Context, ref scope.Ref)
	// Ready reports whether the data about the sandboxes is complete.
	Ready() bool
}

// operationWait is the maximum time that a start or restart operation
// waits for the sandbox. Open WebUI waits a maximum of 180 seconds for a
// management call (docs/protocol.md, section 4.6).
const operationWait = 90 * time.Second

// Handler serves the management API. The caller must authenticate the
// request before it calls the handler.
type Handler struct {
	Policies Policies
	// Instances is nil in development mode. The instance operations are
	// then not available.
	Instances Instances
	// DefaultImage is the image of a policy that sets no image.
	DefaultImage string
}

// instanceContext is the body of an instance operation
// (docs/protocol.md, section 4.6).
type instanceContext struct {
	UserID  *string `json:"user_id"`
	ChatID  *string `json:"chat_id"`
	ModelID *string `json:"model_id"`
}

type instance struct {
	ID        string `json:"id"`
	ContextID string `json:"context_id"`
	Status    string `json:"status"`
}

const maxBodyBytes = 64 << 10

// templateData has the fields that the Open WebUI user interface reads.
// A null value means "the default of the server".
type templateData struct {
	Name           string            `json:"name"`
	Scope          *string           `json:"scope"`
	Image          *string           `json:"image"`
	CPULimit       *string           `json:"cpu_limit"`
	MemoryLimit    *string           `json:"memory_limit"`
	Storage        *string           `json:"storage"`
	NetworkProfile *string           `json:"network_profile"`
	Env            map[string]string `json:"env"`
}

type template struct {
	ID   string       `json:"id"`
	Data templateData `json:"data"`
}

type status struct {
	AllowedImages   string   `json:"allowed_images"`
	MaxCPU          *string  `json:"max_cpu"`
	MaxMemory       *string  `json:"max_memory"`
	MaxStorage      *string  `json:"max_storage"`
	NetworkProfiles []string `json:"network_profiles"`
}

// toTemplate shows a policy in the form of an Open WebUI template. It does
// not show the environment variables: each person who manages the connection
// in Open WebUI can read a template.
func (h *Handler) toTemplate(p policy.Policy) template {
	t := template{ID: p.ID, Data: templateData{Name: p.Name}}
	// Open WebUI uses null for the shared scope.
	if p.Scope != scope.KindShared {
		s := string(p.Scope)
		t.Data.Scope = &s
	}
	text := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	image := p.Image
	if image == "" {
		image = h.DefaultImage
	}
	t.Data.Image = text(image)
	if cpu, ok := p.Resources.Limits[corev1.ResourceCPU]; ok {
		// The user interface of Open WebUI reads a decimal number of
		// processors (docs/protocol.md, section 4.6).
		// A division gives the nearest number. AsApproximateFloat64
		// multiplies, and it gives 0.7000000000000001 for 700m.
		t.Data.CPULimit = text(strconv.FormatFloat(float64(cpu.MilliValue())/1000, 'f', -1, 64))
	}
	if memory, ok := p.Resources.Limits[corev1.ResourceMemory]; ok {
		t.Data.MemoryLimit = text(memory.String())
	}
	if p.Storage.Size != nil {
		// All requests share this quantity, and String writes to it.
		size := p.Storage.Size.DeepCopy()
		t.Data.Storage = text(size.String())
	}
	return t
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Open WebUI sends no identity header on a management call
	// (docs/protocol.md, section 4.6). A request with an identity header is
	// a tool call or an agent call. A sandbox can change the path of such a
	// call, thus it must never get to the management API.
	if HasIdentityHeader(r.Header) {
		httpx.InfoFrom(r.Context()).Detail = "management path on a sandbox request"
		httpx.Error(w, http.StatusForbidden, "Forbidden")
		return
	}
	// A template ID is a DNS label. No management path needs an encoded
	// character, and an encoded "/" must not become a path separator.
	if strings.Contains(r.URL.EscapedPath(), "%") {
		httpx.Error(w, http.StatusNotFound, "Not found")
		return
	}
	path := r.URL.Path
	switch {
	case path == "/api/v1/terminals/status":
		if !allow(w, r, http.MethodGet) {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, status{NetworkProfiles: []string{}})

	case path == templatesPath:
		if r.Method == http.MethodPost {
			readOnly(w)
			return
		}
		if !allow(w, r, http.MethodGet) {
			return
		}
		list := h.Policies.List()
		out := make([]template, 0, len(list))
		for _, p := range list {
			out = append(out, h.toTemplate(p))
		}
		httpx.WriteJSON(w, http.StatusOK, out)

	case strings.HasPrefix(path, templatesPath+"/"):
		h.serveTemplate(w, r, strings.TrimPrefix(path, templatesPath+"/"))

	default:
		httpx.Error(w, http.StatusNotFound, "Not found")
	}
}

// HasIdentityHeader reports whether a request has a header that Open WebUI
// sends only on sandbox traffic.
func HasIdentityHeader(h http.Header) bool {
	for name := range h {
		if name == scope.HeaderUserID || name == scope.HeaderSessionID || strings.HasPrefix(name, "X-Terminal-") {
			return true
		}
	}
	return false
}

func (h *Handler) serveTemplate(w http.ResponseWriter, r *http.Request, rest string) {
	id, sub, hasSub := strings.Cut(rest, "/")
	p, ok := h.Policies.Get(id)
	if !ok {
		httpx.Error(w, http.StatusNotFound, "Template not found")
		return
	}
	if !hasSub {
		switch r.Method {
		case http.MethodGet:
			httpx.WriteJSON(w, http.StatusOK, h.toTemplate(p))
		case http.MethodPatch, http.MethodDelete:
			readOnly(w)
		default:
			methodNotAllowed(w, "GET")
		}
		return
	}
	switch {
	case h.Instances != nil && !h.Instances.Ready():
		// Without the data about the sandboxes, a reply would be wrong.
		w.Header().Set("Retry-After", "5")
		httpx.Error(w, http.StatusServiceUnavailable, "The server starts. Try again in a short time.")

	case sub == "instances" && r.Method == http.MethodGet:
		out := []instance{}
		if h.Instances != nil {
			for _, i := range h.Instances.Instances(p.ID) {
				out = append(out, instance{ID: i.ID, ContextID: i.ContextID, Status: i.Status})
			}
		}
		httpx.WriteJSON(w, http.StatusOK, out)

	case sub == "instance" && r.Method == http.MethodGet:
		// Open WebUI sends the context in the query and reads "status".
		q := r.URL.Query()
		ref, ok := h.instanceRef(w, p, q.Get("user_id"), q.Get("chat_id"), q.Get("model_id"))
		if !ok {
			return
		}
		status := kube.StatusRunning
		if h.Instances != nil {
			status = h.Instances.InstanceStatus(ref)
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": status})

	case (sub == "start" || sub == "stop" || sub == "restart" || sub == "reset") && r.Method == http.MethodPost,
		sub == "instance" && r.Method == http.MethodDelete:
		h.instanceOperation(w, r, p, sub)

	case sub == "instance", sub == "instances", sub == "start", sub == "stop", sub == "restart", sub == "reset":
		methodNotAllowed(w, "GET, POST, DELETE")

	default:
		httpx.Error(w, http.StatusNotFound, "Not found")
	}
}

// instanceRef finds the sandbox for a template and a context. A context
// that is not complete gives status 409, as Open WebUI does.
func (h *Handler) instanceRef(w http.ResponseWriter, p policy.Policy, userID, chatID, modelID string) (scope.Ref, bool) {
	for _, v := range []string{userID, chatID, modelID} {
		if err := scope.ValidateID(v); err != nil {
			httpx.Error(w, http.StatusBadRequest, "Invalid context")
			return scope.Ref{}, false
		}
	}
	ref, err := scope.Resolve(p.Scope, scope.Identity{UserID: userID, TemplateID: p.ID, ChatID: chatID, ModelID: modelID})
	if err != nil {
		httpx.Error(w, http.StatusConflict, "The context of the terminal is not complete: "+err.Error())
		return scope.Ref{}, false
	}
	return ref, true
}

func (h *Handler) instanceOperation(w http.ResponseWriter, r *http.Request, p policy.Policy, op string) {
	if h.Instances == nil {
		notImplemented(w)
		return
	}
	var body instanceContext
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "The body must be a JSON object with user_id, chat_id, and model_id")
		return
	}
	value := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	userID := value(body.UserID)
	ref, ok := h.instanceRef(w, p, userID, value(body.ChatID), value(body.ModelID))
	if !ok {
		return
	}
	req := backend.Request{Ref: ref, UserID: userID}
	info := httpx.InfoFrom(r.Context())
	info.UserID, info.Template, info.Context, info.Sandbox = userID, ref.TemplateID, ref.ContextID(), ref.Name()
	info.Detail = "instance operation: " + op
	// For the user scope, the user ID of the body is the user of the
	// sandbox. A policy with a user list gives no sandbox to other users.
	// The stop, delete, and reset operations start nothing, thus they stay
	// possible for a user that is not in the list.
	if p.Scope == scope.KindUser && !p.Allows(userID) && (op == "start" || op == "restart") {
		info.Detail += ": the policy does not permit the user"
		httpx.Error(w, http.StatusForbidden, "The policy of this terminal does not permit this user")
		return
	}

	var err error
	switch op {
	case "start":
		err = h.Instances.Start(r.Context(), req)
	case "restart":
		err = h.Instances.Restart(r.Context(), req)
	case "stop":
		// The files of the sandbox stay.
		err = h.Instances.Stop(r.Context(), ref)
	case "reset":
		// The files go away, and the instance stays as a stopped instance.
		err = h.Instances.Reset(r.Context(), ref)
	default: // "instance" with DELETE
		// Open WebUI tells the person that a delete removes the instance
		// and its files (docs/protocol.md, section 4.6).
		err = h.Instances.Delete(r.Context(), ref)
	}
	switch {
	case errors.Is(err, backend.ErrLimit):
		httpx.Error(w, http.StatusTooManyRequests, "The user has too many terminals")
		return
	case errors.Is(err, backend.ErrVolumeLimit):
		httpx.Error(w, http.StatusTooManyRequests, "The user has too many terminal volumes")
		return
	case errors.Is(err, backend.ErrCapacity):
		w.Header().Set("Retry-After", "30")
		httpx.Error(w, http.StatusServiceUnavailable, "The server has no capacity for a new terminal")
		return
	case errors.Is(err, backend.ErrNotReady):
		w.Header().Set("Retry-After", "5")
		httpx.Error(w, http.StatusServiceUnavailable, "The server starts. Try again in a short time.")
		return
	case err != nil:
		info.Detail += ": " + err.Error()
		httpx.Error(w, http.StatusBadGateway, "The operation was not successful")
		return
	}
	if op == "start" || op == "restart" {
		// Open WebUI shows each status that is not "running" as stopped.
		// Wait for the sandbox, so that the reply shows the result.
		ctx, cancel := context.WithTimeout(r.Context(), operationWait)
		h.Instances.WaitReady(ctx, ref)
		cancel()
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": h.Instances.InstanceStatus(ref)})
}

func notImplemented(w http.ResponseWriter) {
	httpx.Error(w, http.StatusNotImplemented, "This operation is not available")
}

func allow(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	methodNotAllowed(w, method)
	return false
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	httpx.Error(w, http.StatusMethodNotAllowed, "Method not allowed")
}

func readOnly(w http.ResponseWriter) {
	httpx.Error(w, http.StatusForbidden, "Templates are read-only. An administrator defines them in the warren configuration.")
}
