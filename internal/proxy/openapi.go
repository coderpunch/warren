package proxy

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/httpx"
	"github.com/h3xcat/warren/internal/openapi"
	"github.com/h3xcat/warren/internal/scope"
)

const (
	maxOpenAPIBytes = 4 << 20
	// openAPIStartWait is the time that the tool list waits for a sandbox.
	// Open WebUI stops the chat turn if the tool list does not come in 10
	// seconds (docs/protocol.md, section 9.4). Thus warren does not wait
	// for a sandbox that starts. It sends its complete tool list, and the
	// start continues.
	openAPIStartWait = 1500 * time.Millisecond
	// openAPIFetchTimeout is the maximum time to read the document of a
	// sandbox.
	openAPIFetchTimeout = 2 * time.Second
)

// serveOpenAPI sends the tool list of warren. If the sandbox is ready, the
// list has only the tools that the sandbox also has. Warren never sends
// text from the document of a sandbox (refer to the package openapi).
func (p *Proxy) serveOpenAPI(w http.ResponseWriter, r *http.Request, rt route) {
	info := httpx.InfoFrom(r.Context())
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		httpx.Error(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	body := p.opts.OpenAPI.Full()
	info.Detail = "complete tool list"

	startCtx, cancel := context.WithTimeout(r.Context(), openAPIStartWait)
	lease, err := p.opts.Backend.Acquire(startCtx, backend.Request{Ref: rt.ref, UserID: rt.id.UserID})
	cancel()
	switch {
	case err == nil:
		if ops, ok := p.sandboxOperations(r.Context(), rt, lease.Target()); ok {
			body = p.opts.OpenAPI.Filtered(ops)
			info.Detail = "tool list of the sandbox"
		}
		lease.Release()
	case r.Context().Err() != nil:
		info.Detail = "client went away"
		w.WriteHeader(499)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// sandboxOperations reads the operations that a sandbox has.
func (p *Proxy) sandboxOperations(ctx context.Context, rt route, target backend.Target) (openapi.Operations, bool) {
	ctx, cancel := context.WithTimeout(ctx, openAPIFetchTimeout)
	defer cancel()
	u := *target.URL
	u.Path = "/openapi.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+target.APIKey)
	req.Header.Set(scope.HeaderUserID, rt.id.UserID)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxOpenAPIBytes+1))
	if err != nil || len(data) > maxOpenAPIBytes {
		return nil, false
	}
	return openapi.OperationsOf(data)
}
