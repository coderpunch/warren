// Package httpx has small HTTP helpers that the other packages share.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// WriteJSON sends a JSON response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error sends a JSON error. Open WebUI reads the "detail" field
// (docs/protocol.md, section 4.6).
func Error(w http.ResponseWriter, status int, detail string) {
	WriteJSON(w, status, map[string]string{"detail": detail})
}

// IsWebSocketUpgrade reports whether the request asks for a WebSocket.
func IsWebSocketUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") &&
		headerHasToken(r.Header, "Upgrade", "websocket")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

var browserViewToken = regexp.MustCompile(`^(/browser/view/[^/]+/)[^/]+`)

// RedactPath removes the browser view token from a path. The token is a
// credential (docs/open-terminal-api.md, section 7.2), thus it must not be in
// a log.
func RedactPath(path string) string {
	return browserViewToken.ReplaceAllString(path, "${1}REDACTED")
}

// NewRequestID makes a random identifier for one request.
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// Info collects the data of one request for the access log. The handlers
// fill it. The server writes the log record when the request is complete.
type Info struct {
	RequestID string
	UserID    string
	Template  string
	Context   string
	Sandbox   string
	WebSocket bool
	Detail    string
}

type infoKey struct{}

// WithInfo puts an Info into a context.
func WithInfo(ctx context.Context, info *Info) context.Context {
	return context.WithValue(ctx, infoKey{}, info)
}

// InfoFrom returns the Info of a context. If the context has none, it
// returns an Info that is not connected to a log record.
func InfoFrom(ctx context.Context) *Info {
	if info, ok := ctx.Value(infoKey{}).(*Info); ok {
		return info
	}
	return &Info{}
}
