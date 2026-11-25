// Package server connects the authentication, the management API, and the
// proxy into one HTTP handler.
package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/h3xcat/warren/internal/api"
	"github.com/h3xcat/warren/internal/auth"
	"github.com/h3xcat/warren/internal/httpx"
	"github.com/h3xcat/warren/internal/proxy"
)

// Server is the handler for the port that Open WebUI calls. Each request on
// this port must have the connection key.
type Server struct {
	Keys   *auth.Keys
	API    http.Handler
	Proxy  *proxy.Proxy
	Logger *slog.Logger
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	info := &httpx.Info{RequestID: httpx.NewRequestID()}
	r = r.WithContext(httpx.WithInfo(r.Context(), info))
	w.Header().Set("X-Request-Id", info.RequestID)

	if httpx.IsWebSocketUpgrade(r) {
		// The WebSocket library needs the initial ResponseWriter. The
		// proxy does the authentication for WebSockets.
		s.Proxy.ServeWS(w, r)
		s.log(r, info, 0, start)
		return
	}

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.serve(rec, r, info)
	s.log(r, info, rec.status, start)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, info *httpx.Info) {
	if !s.Keys.RequestAuthorized(r) {
		info.Detail = "bad or absent key"
		w.Header().Set("WWW-Authenticate", "Bearer")
		httpx.Error(w, http.StatusUnauthorized, "Invalid API key")
		return
	}
	if strings.HasPrefix(r.URL.Path, api.Prefix) {
		s.API.ServeHTTP(w, r)
		return
	}
	s.Proxy.ServeHTTP(w, r)
}

// log writes the audit record of one request: who used which sandbox.
// The record has the path but not the query string, because a query string
// can contain secrets.
func (s *Server) log(r *http.Request, info *httpx.Info, status int, start time.Time) {
	attrs := []any{
		"log", "audit",
		"request_id", info.RequestID,
		"method", r.Method,
		"path", httpx.RedactPath(r.URL.Path),
		"duration_ms", time.Since(start).Milliseconds(),
		"remote", r.RemoteAddr,
	}
	if info.WebSocket {
		attrs = append(attrs, "websocket", true)
	} else {
		attrs = append(attrs, "status", status)
	}
	if info.UserID != "" {
		attrs = append(attrs, "user", info.UserID)
	}
	if info.Sandbox != "" {
		attrs = append(attrs, "template", info.Template, "context", info.Context, "sandbox", info.Sandbox)
	}
	if info.Detail != "" {
		attrs = append(attrs, "detail", info.Detail)
	}
	s.Logger.Info("access", attrs...)
}

// statusRecorder keeps the status of a response for the log.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController find the initial writer. The reverse
// proxy uses it to send streamed data immediately.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush sends buffered data to the client.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
