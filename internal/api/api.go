// Package api implements the management API that Open WebUI calls on an
// orchestrator connection (docs/protocol.md, section 4.6).
//
// Templates are read-only. They come from the policy file.
package api

import (
	"net/http"
	"strings"

	"github.com/h3xcat/warren/internal/httpx"
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

// Handler serves the management API. The caller must authenticate the
// request before it calls the handler.
type Handler struct {
	Policies Policies
}

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

func toTemplate(p policy.Policy) template {
	t := template{ID: p.ID, Data: templateData{Name: p.Name}}
	// Open WebUI uses null for the shared scope.
	if p.Scope != scope.KindShared {
		s := string(p.Scope)
		t.Data.Scope = &s
	}
	return t
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
			out = append(out, toTemplate(p))
		}
		httpx.WriteJSON(w, http.StatusOK, out)

	case strings.HasPrefix(path, templatesPath+"/"):
		h.serveTemplate(w, r, strings.TrimPrefix(path, templatesPath+"/"))

	default:
		httpx.Error(w, http.StatusNotFound, "Not found")
	}
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
			httpx.WriteJSON(w, http.StatusOK, toTemplate(p))
		case http.MethodPatch, http.MethodDelete:
			readOnly(w)
		default:
			methodNotAllowed(w, "GET")
		}
		return
	}
	switch sub {
	case "instance", "instances", "restart", "stop", "start", "reset":
		// The instance calls come with the lifecycle controller.
		httpx.Error(w, http.StatusNotImplemented, "Instance operations are not available in this version")
	default:
		httpx.Error(w, http.StatusNotFound, "Not found")
	}
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
