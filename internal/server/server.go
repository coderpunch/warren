// Package server connects the authentication, the management API, and the
// proxy into one HTTP handler.
package server

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/h3xcat/warren/internal/api"
	"github.com/h3xcat/warren/internal/auth"
	"github.com/h3xcat/warren/internal/httpx"
	"github.com/h3xcat/warren/internal/metrics"
	"github.com/h3xcat/warren/internal/proxy"
)

// Server is the handler for the port that Open WebUI calls. Each request on
// this port must have the connection key.
type Server struct {
	Keys   *auth.Keys
	API    http.Handler
	Proxy  *proxy.Proxy
	Logger *slog.Logger
	// Metrics can be nil.
	Metrics *metrics.Metrics
}

// refusalWriteTimeout is the maximum time to send a refusal to a client
// that has no key. If such a client does not read, its connection stops.
const refusalWriteTimeout = 10 * time.Second

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	info := &httpx.Info{RequestID: httpx.NewRequestID()}
	r = r.WithContext(httpx.WithInfo(r.Context(), info))
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK, requestID: info.RequestID}
	rec.Header().Set("X-Request-Id", info.RequestID)

	// Write the audit record in all cases. The reverse proxy stops with a
	// panic when a response body is not complete, for example when the
	// client stops a download.
	logged := false
	write := func() {
		if !logged {
			logged = true
			s.log(r, info, rec, start)
		}
	}
	defer func() {
		if v := recover(); v != nil {
			if info.Detail == "" {
				info.Detail = "response not complete"
			}
			write()
			panic(v) // the HTTP server handles http.ErrAbortHandler
		}
		write()
	}()

	if httpx.IsWebSocketUpgrade(r) {
		// The proxy does the authentication for WebSockets, because six
		// routes have the key in the first message. The proxy calls write
		// before it reports the WebSocket as closed.
		s.Proxy.ServeWS(rec, r, write)
		return
	}
	if !s.Keys.RequestAuthorized(r) {
		info.Detail = "bad or absent key"
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(refusalWriteTimeout))
		rec.Header().Set("Connection", "close")
		rec.Header().Set("WWW-Authenticate", "Bearer")
		httpx.Error(rec, http.StatusUnauthorized, "Invalid API key")
		return
	}
	if r.URL.Path == strings.TrimSuffix(api.Prefix, "/") || strings.HasPrefix(r.URL.Path, api.Prefix) {
		s.API.ServeHTTP(rec, r)
		return
	}
	s.Proxy.ServeHTTP(rec, r)
}

// log writes the audit record of one request: who used which sandbox.
// The record has the path but not the query string, because a query string
// can contain secrets.
func (s *Server) log(r *http.Request, info *httpx.Info, rec *statusRecorder, start time.Time) {
	s.Metrics.Request(rec.status)
	attrs := []any{
		"log", "audit",
		"request_id", info.RequestID,
		"method", r.Method,
		"path", httpx.RedactPath(r.URL.Path),
		"status", rec.status,
		"duration_ms", time.Since(start).Milliseconds(),
		"remote", r.RemoteAddr,
	}
	if info.WebSocket {
		attrs = append(attrs, "websocket", true)
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
	requestID   string
}

func (r *statusRecorder) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		// An informational response of a sandbox. Its headers did not go
		// through the response rules of the proxy. Warren does not send it.
		return
	}
	// The reverse proxy clears the header map after an informational
	// response.
	if r.Header().Get("X-Request-Id") == "" {
		r.Header().Set("X-Request-Id", r.requestID)
	}
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
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

// Hijack gives the connection to the WebSocket library.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("the connection does not permit a WebSocket")
	}
	conn, rw, err := h.Hijack()
	if err == nil {
		r.status = http.StatusSwitchingProtocols
		r.wroteHeader = true
	}
	return conn, rw, err
}
