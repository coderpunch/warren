package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/httpx"
	"github.com/h3xcat/warren/internal/scope"
)

// wsAuth is the method by which a WebSocket route gets its credential.
type wsAuth int

const (
	wsUnknown wsAuth = iota
	// wsFirstMessage: the first text message is a JSON object with
	// type "auth" and the key in "token"
	// (docs/open-terminal-api.md, section 4.3).
	wsFirstMessage
	// wsHeader: the upgrade request has the key in the Authorization
	// header. The browser view routes use this method
	// (docs/protocol.md, section 4.2.2).
	wsHeader
)

var (
	firstMessageRoutes = regexp.MustCompile(
		`^/(events|execute|execute/[^/]+/status|api/terminals/[^/]+|desktop/stream|desktop/audio|agents/sessions/[^/]+/connect)$`)
	headerRoutes = regexp.MustCompile(`^/browser/view/[^/]+/[^/]+/(ws|stream|vnc)$`)
)

func classifyWS(path string) wsAuth {
	switch {
	case firstMessageRoutes.MatchString(path):
		return wsFirstMessage
	case headerRoutes.MatchString(path):
		return wsHeader
	}
	return wsUnknown
}

// Close codes that warren sends. Open WebUI and its browser client give
// these codes a meaning (docs/protocol.md, section 4.2).
const (
	closeAuth      = 4001
	closeForbidden = 4003
	closeNotFound  = 4004
	closeBad       = 4000
)

const (
	maxAuthMessageBytes = 64 << 10
	closeWriteWait      = 5 * time.Second
	handshakeTimeout    = 15 * time.Second
)

var upgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	ReadBufferSize:   32 << 10,
	WriteBufferSize:  32 << 10,
	// Only the Open WebUI backend calls warren. A backend sends no Origin
	// header. A request with an Origin header comes from a browser, and
	// warren refuses it.
	CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
	Error: func(w http.ResponseWriter, _ *http.Request, status int, reason error) {
		httpx.Error(w, status, "WebSocket handshake failed")
	},
}

// ServeWS handles a WebSocket upgrade request. It does the authentication
// itself, because six routes have their credential in the first message and
// not in a header.
func (p *Proxy) ServeWS(w http.ResponseWriter, r *http.Request) {
	info := httpx.InfoFrom(r.Context())
	info.WebSocket = true

	kind := classifyWS(r.URL.Path)
	if kind == wsUnknown {
		httpx.Error(w, http.StatusNotFound, "Not found")
		return
	}
	if err := validatePath(r.URL.Path); err != nil {
		httpx.Error(w, http.StatusBadRequest, "Invalid path")
		return
	}
	id, sessionID, rerr := readIdentity(r.Header)
	if rerr != nil {
		httpx.Error(w, rerr.status, rerr.detail)
		return
	}

	// A request that has an Authorization header must have a correct key.
	headerAuth := false
	if _, present := r.Header["Authorization"]; present {
		if !p.opts.Keys.RequestAuthorized(r) {
			info.Detail = "bad key in header"
			unauthorized(w)
			return
		}
		headerAuth = true
	}
	if kind == wsHeader && !headerAuth {
		info.Detail = "no key in header"
		unauthorized(w)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		info.Detail = "upgrade: " + err.Error()
		return
	}
	defer conn.Close()

	var firstMessage map[string]json.RawMessage
	if kind == wsFirstMessage {
		var ok bool
		firstMessage, ok = p.readAuthMessage(conn, headerAuth)
		if !ok {
			info.Detail = "first message authentication failed"
			closeConn(conn, closeAuth, "Invalid API key")
			return
		}
	}
	conn.SetReadLimit(p.opts.MaxMessageBytes)

	// The caller is authenticated from this point.
	rt, rerr := p.resolveRoute(id, sessionID)
	if rerr != nil {
		info.Detail = rerr.detail
		closeConn(conn, rerr.close, rerr.detail)
		return
	}
	fillInfo(r.Context(), rt)

	target, err := p.opts.Resolver.Resolve(r.Context(), rt.ref)
	if err != nil {
		if errors.Is(err, backend.ErrNotReady) || errors.Is(err, context.DeadlineExceeded) {
			info.Detail = "sandbox not ready"
			// The browser client tries again after code 1013.
			closeConn(conn, websocket.CloseTryAgainLater, "The terminal is not ready")
		} else {
			info.Detail = "resolve: " + err.Error()
			closeConn(conn, websocket.CloseInternalServerErr, "The terminal is not available")
		}
		return
	}

	up, code, err := p.dialSandbox(r, rt, target)
	if err != nil {
		info.Detail = "dial sandbox: " + err.Error()
		closeConn(conn, code, "The terminal refused the connection")
		return
	}
	defer up.Close()
	up.SetReadLimit(p.opts.MaxMessageBytes)

	if firstMessage != nil {
		// Put the private key of the sandbox in place of the connection key.
		token, _ := json.Marshal(target.APIKey)
		firstMessage["token"] = token
		data, _ := json.Marshal(firstMessage)
		if err := up.WriteMessage(websocket.TextMessage, data); err != nil {
			info.Detail = "send first message: " + err.Error()
			closeConn(conn, websocket.CloseInternalServerErr, "The terminal is not available")
			return
		}
	}

	relay(conn, up)
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	httpx.Error(w, http.StatusUnauthorized, "Invalid API key")
}

// readAuthMessage reads the first message of a client. It returns the JSON
// object and true if the message is an "auth" message and the caller has a
// correct key.
func (p *Proxy) readAuthMessage(conn *websocket.Conn, headerAuth bool) (map[string]json.RawMessage, bool) {
	conn.SetReadLimit(maxAuthMessageBytes)
	_ = conn.SetReadDeadline(time.Now().Add(p.opts.AuthTimeout))
	defer conn.SetReadDeadline(time.Time{})

	mt, data, err := conn.ReadMessage()
	if err != nil || mt != websocket.TextMessage {
		return nil, false
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(data, &msg); err != nil || msg == nil {
		return nil, false
	}
	var typ string
	if err := json.Unmarshal(msg["type"], &typ); err != nil || typ != "auth" {
		return nil, false
	}
	if headerAuth {
		return msg, true
	}
	var token string
	if err := json.Unmarshal(msg["token"], &token); err != nil {
		return nil, false
	}
	return msg, p.opts.Keys.Match(token)
}

// dialSandbox opens the WebSocket to the sandbox. On a failure it returns
// the close code for the client.
func (p *Proxy) dialSandbox(r *http.Request, rt route, target backend.Target) (*websocket.Conn, int, error) {
	u := *target.URL
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = r.URL.Path
	u.RawPath = r.URL.RawPath
	u.RawQuery = filterQuery(r.URL.Path, r.URL.RawQuery)

	// The sandbox reads its credential from the first message or from the
	// path token, and not from a header. Thus warren sends only the two
	// owner headers.
	h := http.Header{}
	h.Set(scope.HeaderUserID, rt.id.UserID)
	if rt.sessionID != "" {
		h.Set(scope.HeaderSessionID, rt.sessionID)
	}

	dialer := websocket.Dialer{
		Proxy:            nil,
		NetDialContext:   p.dial,
		HandshakeTimeout: handshakeTimeout,
		ReadBufferSize:   32 << 10,
		WriteBufferSize:  32 << 10,
	}
	up, resp, err := dialer.DialContext(r.Context(), u.String(), h)
	if err != nil {
		code := websocket.CloseInternalServerErr
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				code = closeAuth
			case http.StatusForbidden, http.StatusNotFound:
				// Open Terminal refuses an unknown session or a bad path
				// token with status 403 before it accepts the WebSocket
				// (docs/open-terminal-api.md, section 4.3).
				code = closeNotFound
			}
		}
		return nil, code, err
	}
	return up, 0, nil
}

// closeInfo describes why one direction of a relay stopped.
type closeInfo struct {
	fromClient bool
	code       int
	text       string
}

// relay copies messages in the two directions until one side stops. It then
// sends the close code to the other side.
func relay(client, sandbox *websocket.Conn) {
	done := make(chan closeInfo, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		done <- copyMessages(sandbox, client, true)
	}()
	go func() {
		defer wg.Done()
		done <- copyMessages(client, sandbox, false)
	}()

	first := <-done
	if first.fromClient {
		closeConn(sandbox, clientCloseCode(first.code), first.text)
	} else {
		closeConn(client, sandboxCloseCode(first.code), first.text)
	}
	// Stop the other direction.
	_ = client.Close()
	_ = sandbox.Close()
	wg.Wait()
}

// copyMessages copies each message from src to dst as a stream. It does not
// keep a message in memory, and it does not group messages. Open Terminal
// closes a terminal when too much input waits
// (docs/open-terminal-api.md, section 3.4).
func copyMessages(dst, src *websocket.Conn, fromClient bool) closeInfo {
	buf := make([]byte, 32<<10)
	for {
		mt, reader, err := src.NextReader()
		if err != nil {
			info := closeInfo{fromClient: fromClient, code: websocket.CloseAbnormalClosure}
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				info.code, info.text = ce.Code, ce.Text
			}
			return info
		}
		writer, err := dst.NextWriter(mt)
		if err != nil {
			// The other side is gone. Report it as the cause.
			return closeInfo{fromClient: !fromClient, code: websocket.CloseAbnormalClosure}
		}
		if _, err := io.CopyBuffer(writer, reader, buf); err != nil {
			_ = writer.Close()
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				return closeInfo{fromClient: fromClient, code: ce.Code, text: ce.Text}
			}
			return closeInfo{fromClient: fromClient, code: websocket.CloseAbnormalClosure}
		}
		if err := writer.Close(); err != nil {
			return closeInfo{fromClient: !fromClient, code: websocket.CloseAbnormalClosure}
		}
	}
}

// reserved reports whether a close code must not be in a close message.
func reserved(code int) bool {
	return code == websocket.CloseNoStatusReceived ||
		code == websocket.CloseAbnormalClosure ||
		code == websocket.CloseTLSHandshake ||
		code < 1000 || code > 4999
}

// sandboxCloseCode gives the code that the client gets when the sandbox
// side stops.
func sandboxCloseCode(code int) int {
	if reserved(code) {
		return websocket.CloseInternalServerErr
	}
	return code
}

// clientCloseCode gives the code that the sandbox gets when the client side
// stops.
func clientCloseCode(code int) int {
	if code == websocket.CloseNoStatusReceived {
		return websocket.CloseNormalClosure
	}
	if reserved(code) {
		return websocket.CloseGoingAway
	}
	return code
}

// closeConn sends a close message. The reason text has a maximum of 123
// bytes in the protocol.
func closeConn(conn *websocket.Conn, code int, text string) {
	if len(text) > 120 {
		text = text[:120]
	}
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, text), time.Now().Add(closeWriteWait))
}
