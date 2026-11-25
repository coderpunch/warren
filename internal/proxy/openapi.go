package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/h3xcat/warren/internal/httpx"
	"github.com/h3xcat/warren/internal/scope"
)

const (
	maxOpenAPIBytes = 4 << 20
	openAPITimeout  = 8 * time.Second
)

// toolPath is the form of a path key that warren permits in an OpenAPI
// document. Open WebUI makes the URL of a tool call from the connection URL
// and the path key, with no check (docs/protocol.md, section 4.3). A key
// that does not start with "/", or that has other characters, can change
// the host of that URL. Open WebUI then sends the connection key to that
// host.
var toolPath = regexp.MustCompile(`^(/[A-Za-z0-9._~{}-]+)+/?$`)

func safeToolPath(key string) bool {
	if !toolPath.MatchString(key) {
		return false
	}
	// A tool call must not go to the management API.
	if key == "/api/v1" || strings.HasPrefix(key, "/api/v1/") {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// sanitizeOpenAPI removes from an OpenAPI document all parts with which a
// sandbox can send a tool call of Open WebUI to a different address.
func sanitizeOpenAPI(data []byte) ([]byte, bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
		return nil, false
	}
	delete(doc, "servers")
	var paths map[string]map[string]json.RawMessage
	if raw, ok := doc["paths"]; ok {
		if err := json.Unmarshal(raw, &paths); err != nil {
			return nil, false
		}
	}
	for key, item := range paths {
		if !safeToolPath(key) || item == nil {
			delete(paths, key)
			continue
		}
		delete(item, "servers")
		delete(item, "$ref")
		for method, rawOp := range item {
			var op map[string]json.RawMessage
			if err := json.Unmarshal(rawOp, &op); err != nil || op == nil {
				continue // not an operation object, for example "parameters"
			}
			if _, has := op["servers"]; has {
				delete(op, "servers")
				item[method], _ = json.Marshal(op)
			}
		}
	}
	if paths == nil {
		paths = map[string]map[string]json.RawMessage{}
	}
	doc["paths"], _ = json.Marshal(paths)
	out, err := json.Marshal(doc)
	return out, err == nil
}

// serveOpenAPI gets the OpenAPI document of a sandbox, examines it, and
// sends the safe parts. Warren never relays this document directly.
func (p *Proxy) serveOpenAPI(w http.ResponseWriter, r *http.Request, pc *proxyCtx) {
	info := httpx.InfoFrom(r.Context())
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		httpx.Error(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	u := *pc.target.URL
	u.Path = "/openapi.json"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, "The terminal is not available")
		return
	}
	req.Header.Set("Authorization", "Bearer "+pc.target.APIKey)
	req.Header.Set(scope.HeaderUserID, pc.route.id.UserID)
	req.Header.Set("Accept", "application/json")

	client := *p.client
	client.Timeout = openAPITimeout
	resp, err := client.Do(req)
	if err != nil {
		info.Detail = "openapi: " + truncate(err.Error(), maxErrorText)
		httpx.Error(w, http.StatusBadGateway, "The terminal is not available")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		info.Detail = "openapi: sandbox status " + resp.Status
		httpx.Error(w, http.StatusBadGateway, "The terminal gave no tool list")
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxOpenAPIBytes+1))
	if err != nil || len(data) > maxOpenAPIBytes {
		info.Detail = "openapi: document too large or not complete"
		httpx.Error(w, http.StatusBadGateway, "The terminal gave no tool list")
		return
	}
	clean, ok := sanitizeOpenAPI(data)
	if !ok {
		info.Detail = "openapi: document is not valid"
		httpx.Error(w, http.StatusBadGateway, "The terminal gave no tool list")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(clean)
}
