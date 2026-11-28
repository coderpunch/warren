package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/coderpunch/warren/internal/backend"
	"github.com/coderpunch/warren/internal/httpx"
	"github.com/coderpunch/warren/internal/scope"
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

// watcherRoutes are the WebSocket routes on which a client only watches a
// sandbox: the events of files and ports, the process list, and the output of
// one process. The browser client of Open WebUI opens them with no action of
// a person, sends no data on them after the first messages, and connects
// again with no limit after most close codes (docs/protocol.md, section
// 4.2.3). Each other WebSocket route is a viewer route: a person or an agent
// uses the sandbox through it.
var watcherRoutes = regexp.MustCompile(`^/(events|execute|execute/[^/]+/status)$`)

// watcherRoute reports whether a decoded request path is a watcher route. A
// watcher does not begin a start of its sandbox, and it has its own close
// code for the idle stop (docs/policies.md, section 10.3).
func watcherRoute(path string) bool { return watcherRoutes.MatchString(path) }

// holdRoutes is the WebSocket route of an agent session. Its only client is
// the Open WebUI backend, which keeps it open for one chat turn of an agent
// and sends no HTTP request to the sandbox in that time (docs/protocol.md,
// section 4.7). No browser can open it, thus a forgotten page cannot keep it
// open.
var holdRoutes = regexp.MustCompile(`^/agents/sessions/[^/]+/connect$`)

// holdRoute reports whether a decoded request path is the route of an agent
// session. The open WebSocket on that route is a use of the sandbox, and the
// idle stop waits for its end (docs/policies.md, section 10.4).
func holdRoute(path string) bool { return holdRoutes.MatchString(path) }

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
	closeBad       = 4000
	closeAuth      = 4001
	closeForbidden = 4003
	closeNotFound  = 4004
)

const (
	maxAuthMessageBytes = 64 << 10
	maxHandshakeBytes   = 64 << 10
	handshakeTimeout    = 15 * time.Second
	// wsStartTimeout is the maximum time that a WebSocket waits for its
	// sandbox.
	wsStartTimeout = 50 * time.Second
	// closeWait is the maximum time that warren waits for the reply to a
	// close message.
	closeWait = 3 * time.Second
)

var upgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	ReadBufferSize:   32 << 10,
	WriteBufferSize:  32 << 10,
	// Only the Open WebUI backend calls warren. A backend sends no Origin
	// header. A request with an Origin header comes from a browser, and
	// warren refuses it.
	CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
	Error: func(w http.ResponseWriter, _ *http.Request, status int, _ error) {
		httpx.Error(w, status, "WebSocket handshake failed")
	},
}

// ServeWS handles a WebSocket upgrade request. It does the authentication
// itself, because six routes have their credential in the first message and
// not in a header.
//
// ServeWS calls finished one time before it returns. Shutdown waits until
// that call is complete. Thus the caller can write its audit record there,
// and the record is not lost when warren stops.
func (p *Proxy) ServeWS(w http.ResponseWriter, r *http.Request, finished func()) {
	if finished == nil {
		finished = func() {}
	}
	counted := false
	defer func() {
		finished()
		if counted {
			p.sockets.Done()
		}
	}()

	info := httpx.InfoFrom(r.Context())
	info.WebSocket = true

	kind := classifyWS(r.URL.Path)
	if kind == wsUnknown {
		info.Detail = "no WebSocket route"
		httpx.Error(w, http.StatusNotFound, "Not found")
		return
	}
	if err := validatePath(r.URL.Path); err != nil {
		info.Detail = "invalid path"
		httpx.Error(w, http.StatusBadRequest, "Invalid path")
		return
	}
	id, sessionID, rerr := readIdentity(r.Header)
	if rerr != nil {
		info.Detail = rerr.detail
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

	// Warren must accept a WebSocket before it can read the key from the
	// first message. A limit on the number of such sockets prevents a
	// client with no key from the use of all memory.
	preAuth := !headerAuth
	if preAuth {
		select {
		case p.preAuth <- struct{}{}:
		default:
			info.Detail = "too many WebSockets wait for authentication"
			w.Header().Set("Retry-After", "1")
			httpx.Error(w, http.StatusServiceUnavailable, "Too many connections")
			return
		}
	}
	releasePreAuth := func() {
		if preAuth {
			<-p.preAuth
			preAuth = false
		}
	}
	defer releasePreAuth()

	// Count the socket before the upgrade. Shutdown waits for all sockets.
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		info.Detail = "warren stops"
		w.Header().Set("Retry-After", "5")
		httpx.Error(w, http.StatusServiceUnavailable, "The server restarts")
		return
	}
	p.sockets.Add(1)
	counted = true
	p.mu.Unlock()

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		info.Detail = "upgrade: " + truncate(err.Error(), maxErrorText)
		return
	}
	defer conn.Close()

	var firstMessage map[string]json.RawMessage
	if kind == wsFirstMessage {
		var ok bool
		firstMessage, ok = p.readAuthMessage(conn, headerAuth)
		if !ok {
			info.Detail = "first message authentication failed"
			closeAndWait(conn, closeAuth, "Invalid API key")
			return
		}
	}
	releasePreAuth()
	conn.SetReadLimit(p.opts.MaxMessageBytes)

	// The caller is authenticated from this point.
	rt, rerr := p.resolveRoute(id, sessionID)
	if rerr != nil {
		info.Detail = rerr.detail
		closeAndWait(conn, rerr.close, rerr.detail)
		return
	}
	fillInfo(r.Context(), rt)
	p.opts.Metrics.WebSocketOpened()
	defer p.opts.Metrics.WebSocketClosed()
	p.opts.Logger.Info("websocket open", "log", "audit", "request_id", info.RequestID,
		"path", httpx.RedactPath(r.URL.Path), "user", info.UserID, "template", info.Template,
		"context", info.Context, "sandbox", info.Sandbox)

	// After the upgrade, the HTTP server does not monitor the connection.
	// The context ends when the client goes away, when warren stops, or
	// when the wait for the sandbox is too long.
	ctx, cancel := context.WithTimeout(r.Context(), wsStartTimeout)
	defer cancel()
	go func() {
		select {
		case <-p.closing:
			cancel()
		case <-ctx.Done():
		}
	}()

	// A watcher does not begin a start of its sandbox. It waits here until a
	// different request or operation starts the sandbox, for the same maximum
	// time. At the end of that time it gets the code 1013, as each WebSocket
	// whose sandbox is not ready.
	watcher := watcherRoute(r.URL.Path)
	s := &session{proxy: p, client: conn, watcher: watcher}
	// From this point only the client pump reads from the client. It finds
	// a client that goes away, and it answers pings, while the sandbox
	// starts.
	sandboxReady := make(chan *websocket.Conn, 1)
	clientDone := make(chan closeInfo, 1)
	go func() { clientDone <- s.pumpClient(sandboxReady, cancel) }()

	// refuse stops a connection that has no sandbox connection.
	refuse := func(code int, text, detail string) {
		sandboxReady <- nil
		info.Detail = detail
		closeConn(conn, code, text)
		_ = conn.NetConn().SetReadDeadline(time.Now().Add(closeWait))
		<-clientDone
	}

	lease, err := p.opts.Backend.Acquire(ctx, backend.Request{
		Ref: rt.ref, UserID: rt.id.UserID, WebSocket: true, NoStart: watcher, Hold: holdRoute(r.URL.Path),
	})
	if err != nil {
		switch {
		case errors.Is(err, backend.ErrCapacity):
			p.opts.Metrics.ProxyError("limit")
			refuse(websocket.CloseTryAgainLater, "The server has no capacity for a new terminal", "sandbox capacity")
		case errors.Is(err, backend.ErrLimit):
			p.opts.Metrics.ProxyError("limit")
			refuse(closeForbidden, "You have too many terminals", "sandbox limit")
		case errors.Is(err, backend.ErrVolumeLimit):
			p.opts.Metrics.ProxyError("limit")
			refuse(closeForbidden, "You have too many terminals with saved files", "volume limit")
		case errors.Is(err, backend.ErrNotReady), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			// The browser client tries again after code 1013.
			p.opts.Metrics.ProxyError("not_ready")
			refuse(websocket.CloseTryAgainLater, "The terminal is not ready", "sandbox not ready")
		default:
			p.opts.Metrics.ProxyError("backend")
			refuse(websocket.CloseInternalServerErr, "The terminal is not available",
				"acquire: "+truncate(err.Error(), maxErrorText))
		}
		return
	}
	defer lease.Release()
	s.lease = lease
	target := lease.Target()

	up, code, err := p.dialSandbox(ctx, r, rt, target)
	if err != nil {
		p.opts.Metrics.ProxyError("upstream")
		refuse(code, "The terminal refused the connection", "dial sandbox: "+truncate(err.Error(), maxErrorText))
		return
	}
	defer up.Close()
	up.SetReadLimit(p.opts.MaxMessageBytes)
	s.sandbox = up

	if firstMessage != nil {
		// Put the private key of the sandbox in place of the connection key.
		token, _ := json.Marshal(target.APIKey)
		firstMessage["token"] = token
		data, _ := json.Marshal(firstMessage)
		_ = up.SetWriteDeadline(time.Now().Add(p.opts.WriteTimeout))
		if err := up.WriteMessage(websocket.TextMessage, data); err != nil {
			refuse(websocket.CloseInternalServerErr, "The terminal is not available",
				"send first message: "+truncate(err.Error(), maxErrorText))
			return
		}
	}
	sandboxReady <- up
	sandboxDone := make(chan closeInfo, 1)
	go func() { sandboxDone <- s.copyMessages(conn, up, false, 0, nil) }()

	info.Detail = s.finish(clientDone, sandboxDone)
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

// budgetConn stops a read when the peer sent more bytes than the budget.
// The WebSocket library reads the handshake reply with no size limit. A
// hostile sandbox can send a reply that has no end.
type budgetConn struct {
	net.Conn
	// left is the number of bytes that the peer can send. A negative
	// value means no limit.
	left atomic.Int64
}

var errHandshakeTooLarge = errors.New("the handshake reply of the sandbox is too large")

func (c *budgetConn) Read(p []byte) (int, error) {
	left := c.left.Load()
	if left < 0 {
		return c.Conn.Read(p)
	}
	if left == 0 {
		return 0, errHandshakeTooLarge
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	n, err := c.Conn.Read(p)
	c.left.Add(-int64(n))
	return n, err
}

// dialSandbox opens the WebSocket to the sandbox. On a failure it returns
// the close code for the client.
func (p *Proxy) dialSandbox(ctx context.Context, r *http.Request, rt route, target backend.Target) (*websocket.Conn, int, error) {
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

	var limited *budgetConn
	dialer := websocket.Dialer{
		Proxy: nil,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := p.dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			limited = &budgetConn{Conn: conn}
			limited.left.Store(maxHandshakeBytes)
			return limited, nil
		},
		HandshakeTimeout: handshakeTimeout,
		ReadBufferSize:   32 << 10,
		WriteBufferSize:  32 << 10,
	}
	up, resp, err := dialer.DialContext(ctx, u.String(), h)
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
	// Warren offers no extension and no subprotocol. The library accepts
	// compression that a server selects with no offer, and the size limit
	// then counts only compressed bytes.
	if resp.Header.Get("Sec-WebSocket-Extensions") != "" || resp.Header.Get("Sec-WebSocket-Protocol") != "" {
		_ = up.Close()
		return nil, websocket.CloseInternalServerErr,
			errors.New("the sandbox selected an extension or a subprotocol that warren did not offer")
	}
	limited.left.Store(-1)
	return up, 0, nil
}

// side is one end of a relay.
type side int

const (
	sideClient side = iota
	sideSandbox
	sideWarren
)

func (s side) String() string {
	switch s {
	case sideClient:
		return "client"
	case sideSandbox:
		return "sandbox"
	}
	return "warren"
}

// closeInfo describes why one direction of a relay stopped.
type closeInfo struct {
	// side is the end that stopped.
	side side
	code int
	text string
	// writeFailed: a write to this side failed. The reader of this side
	// can have the real close code.
	writeFailed bool
	// reason is the stop reason of the backend, when the backend stopped
	// the sandbox. It is empty in all other conditions.
	reason string
}

// stopClose gives the close code and the reason text that the client gets
// when the backend stops the sandbox of a WebSocket (docs/policies.md,
// section 10.2). The client of Open WebUI connects again after the codes
// 1011 and 1012, and it does not connect again after a code from 4000 to
// 4999 (docs/protocol.md, section 4.2.3). Open WebUI does not send the text
// to the browser. The text is for other clients.
//
// The two classes of routes are different only for the idle stop. A viewer that
// connects again starts the sandbox, thus it gets a code after which it
// stops. A watcher that connects again starts nothing, and a watcher that
// stopped stays stopped when the sandbox operates again. Thus a watcher gets
// a code after which it connects again.
func stopClose(reason string, watcher bool) (code int, text string) {
	switch reason {
	case backend.StopAPI:
		return websocket.CloseServiceRestart, "An operation stopped or restarted the terminal"
	case backend.StopIdle:
		if watcher {
			return websocket.CloseServiceRestart, "The terminal stopped after its idle time"
		}
		return closeNotFound, "The terminal stopped after its idle time"
	case backend.StopNoPolicy:
		// For a policy that is not in the file, a new connection gets the
		// same code and text from the route check. For a policy with a
		// different scope, a new connection is for the sandbox of the new
		// scope, and it does not get this code (docs/policies.md,
		// section 10.2).
		return closeNotFound, "Template not found"
	case backend.StopNetworkChanged, backend.StopVolumeGone, backend.StopExternal:
		return websocket.CloseServiceRestart, "The terminal restarts"
	}
	return websocket.CloseInternalServerErr, "The terminal failed"
}

// backendStop returns the close data for a stop of the sandbox by the
// backend, for a watcher route or for a viewer route. It reports false if
// the lease does not have the stop signal.
func backendStop(lease backend.Lease, watcher bool) (closeInfo, bool) {
	select {
	case <-lease.Stopped():
	default:
		return closeInfo{}, false
	}
	reason := lease.StopReason()
	code, text := stopClose(reason, watcher)
	return closeInfo{side: sideWarren, code: code, text: text, reason: reason}, true
}

// cause returns the close data for the end of a relay, from the data of the
// side that stopped first. When the sandbox side stopped and the lease has
// the stop signal, the stop by the backend is the cause: a sandbox that stops
// sends its own close code, or it breaks the connection. The client then gets
// the code of warren, thus the result does not depend on the sequence of the
// two events.
func (s *session) cause(first closeInfo) closeInfo {
	if first.side != sideSandbox {
		return first
	}
	if stop, ok := backendStop(s.lease, s.watcher); ok {
		return stop
	}
	return first
}

// session is one WebSocket relay.
type session struct {
	proxy   *Proxy
	client  *websocket.Conn
	sandbox *websocket.Conn
	// lease is the lease on the sandbox. It is nil until the backend gives
	// it.
	lease backend.Lease
	// watcher is true for a watcher route: refer to watcherRoute.
	watcher bool
	// closing is true after warren sent a close message. The copy loops
	// then read and discard, until the reply to the close message comes.
	closing atomic.Bool
}

// pumpClient reads the client. Before the sandbox connection exists, it
// only waits for the first message: thus it sees a client that goes away,
// and the library answers pings. When the sandbox connection is ready, it
// copies messages to the sandbox.
func (s *session) pumpClient(sandboxReady <-chan *websocket.Conn, clientGone func()) closeInfo {
	mt, reader, err := s.client.NextReader()
	if err != nil {
		clientGone()
		<-sandboxReady
		return readFailure(sideClient, err)
	}
	up := <-sandboxReady
	if up == nil {
		// No sandbox. Read until the client answers the close message.
		for {
			if _, _, err := s.client.NextReader(); err != nil {
				return readFailure(sideClient, err)
			}
		}
	}
	return s.copyMessages(up, s.client, true, mt, reader)
}

func otherSide(fromClient bool) (src, dst side) {
	if fromClient {
		return sideClient, sideSandbox
	}
	return sideSandbox, sideClient
}

func readFailure(src side, err error) closeInfo {
	info := closeInfo{side: src, code: websocket.CloseAbnormalClosure}
	var ce *websocket.CloseError
	switch {
	case errors.As(err, &ce):
		info.code, info.text = ce.Code, ce.Text
	case errors.Is(err, websocket.ErrReadLimit):
		info.code, info.text = websocket.CloseMessageTooBig, "Message too large"
	}
	return info
}

// automaticBytes is the number of bytes at the start of a text message of a
// client that warren examines to find the kind of the message.
const automaticBytes = 64

// The parts of the start of an automatic message, in their sequence, and the
// three types of an automatic message with their quotation marks.
var (
	automaticStart = [][]byte{[]byte("{"), []byte(`"type"`), []byte(":")}
	typePing       = []byte(`"ping"`)
	typeAck        = []byte(`"ack"`)
	typeFrameAck   = []byte(`"frameAck"`)
)

// messageKind is the result of the examination of one message of a client.
type messageKind int

const (
	// kindUse is a message that is a use of the sandbox.
	kindUse messageKind = iota
	// kindPing and kindFrameAck are automatic messages that are never a use.
	kindPing
	kindFrameAck
	// kindAck is an automatic message that is a use only while the client
	// shows the terminal: refer to clientUse.
	kindAck
)

// jsonSpace is the white space of JSON.
const jsonSpace = " \t\n\r"

// automaticKind examines the first bytes of a text message of a client. The
// clients of Open WebUI send three types of message with no action of a
// person: "ping" on a timer, "ack" after output of the sandbox that the
// terminal wrote to the screen, and "frameAck" after each frame of a screen
// stream (docs/protocol.md, section 4.2.4). Such a message is not proof that
// a person uses the sandbox.
//
// A message is automatic if and only if head starts with these parts, in this
// sequence: optional JSON white space, "{", optional white space, the bytes
// "type" in quotation marks, optional white space, ":", optional white space,
// and then exactly "ping", "ack", or "frameAck" in quotation marks. Each
// other message is a use message.
//
// The caller gives a maximum of automaticBytes bytes. The size of the message
// and all bytes after the type have no effect. That is necessary: a sandbox
// controls a value that the client puts into an "ack" message, and thus the
// size of that message (docs/protocol.md, section 4.2.4). A result "use" that
// is wrong is what a sandbox can use to keep itself, thus the rule must not
// depend on a value that a sandbox controls. A result "automatic" that is
// wrong harms only the client that sent the message.
//
// The function is not a JSON decoder. A key with an escape sequence, and a
// "type" member that is not the first member, give a use message.
func automaticKind(head []byte) messageKind {
	rest := head
	for _, part := range automaticStart {
		rest = bytes.TrimLeft(rest, jsonSpace)
		if !bytes.HasPrefix(rest, part) {
			return kindUse
		}
		rest = rest[len(part):]
	}
	rest = bytes.TrimLeft(rest, jsonSpace)
	switch {
	case bytes.HasPrefix(rest, typePing):
		return kindPing
	case bytes.HasPrefix(rest, typeAck):
		return kindAck
	case bytes.HasPrefix(rest, typeFrameAck):
		return kindFrameAck
	}
	return kindUse
}

// visibility examines a complete text message of a client. The interactive
// terminal of Open WebUI sends the message {"type":"activity","active":B}
// when it becomes visible or hidden: B is true only while the page shows the
// terminal tab and the document is not hidden (docs/protocol.md, section
// 4.2.4). The function returns B, and true if the message is such a message:
// a JSON object whose member "type" is the string "activity" and whose member
// "active" is true or false. Open Terminal has the same condition
// (docs/open-terminal-api.md, section 3.4).
func visibility(msg []byte) (active, ok bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(msg, &object) != nil {
		return false, false
	}
	var typ string
	if json.Unmarshal(object["type"], &typ) != nil || typ != "activity" {
		return false, false
	}
	switch string(object["active"]) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// clientUse finds the messages of the client of one relay that are a use of
// the sandbox, and tells the lease about each one (backend.Lease.Active). An
// open WebSocket is not a use of the sandbox: only these messages are
// (docs/policies.md, section 10.4). The rules:
//
//   - A binary message is always a use message.
//   - A text message is a use message unless it is automatic: refer to
//     automaticKind.
//   - An automatic "ping" or "frameAck" message is never a use.
//   - An automatic "ack" message is a use only while visible is true. A
//     terminal sends it after it wrote output to the screen. Thus output that
//     a visible terminal shows is a use, and output in a hidden terminal is
//     not.
//   - A message of the sandbox is never a use. The copy loop of that
//     direction has no clientUse.
//
// Only the copy loop of the client direction uses a clientUse, thus it needs
// no lock. Warren relays each message with no change: the rules decide only
// the call of Active.
type clientUse struct {
	lease backend.Lease
	// visible is the last value that the client reported with a message of
	// the type "activity". It is false until the first such message. No
	// message of the sandbox can change it.
	visible bool

	// The data of the message in progress. head has the first bytes of a text
	// message: one more than automaticBytes, to tell a message of exactly
	// that size from a longer one.
	head    [automaticBytes + 1]byte
	size    int
	decided bool
}

// start begins a new message of the client.
func (u *clientUse) start(messageType int) {
	u.size, u.decided = 0, false
	if messageType != websocket.TextMessage {
		// Terminal input and the data of a remote desktop are binary.
		u.decided = true
		u.lease.Active()
	}
}

// data gets each part of the message, in sequence.
func (u *clientUse) data(part []byte) {
	if u.decided {
		return
	}
	u.size += copy(u.head[u.size:], part)
	if u.size > automaticBytes {
		// The message is longer than the bytes that warren examines.
		u.decide(false)
	}
}

// end completes the message.
func (u *clientUse) end() {
	if !u.decided {
		u.decide(true)
	}
}

// decide makes the decision for a text message. complete is true if head has
// the full message.
func (u *clientUse) decide(complete bool) {
	u.decided = true
	head := u.head[:min(u.size, automaticBytes)]
	switch automaticKind(head) {
	case kindPing, kindFrameAck:
		return
	case kindAck:
		if u.visible {
			u.lease.Active()
		}
		return
	}
	if complete {
		if active, ok := visibility(head); ok {
			u.visible = active
		}
	}
	u.lease.Active()
}

// copyMessages copies each message from src to dst as a stream. It does not
// keep a message in memory, and it does not group messages: Open Terminal
// closes a terminal when too much input waits
// (docs/open-terminal-api.md, section 3.4). If reader is not nil, it is a
// message of src that the caller started to read.
//
// In the direction from the client, it also tells the lease about each
// message that is a use of the sandbox: refer to clientUse. The cost for one
// message is a copy of its first bytes and one call of Active.
//
// After warren sent a close message, the function writes nothing: it reads
// src and discards the data, until src answers the close message.
func (s *session) copyMessages(dst, src *websocket.Conn, fromClient bool, mt int, reader io.Reader) closeInfo {
	srcSide, dstSide := otherSide(fromClient)
	buf := make([]byte, 32<<10)
	var use *clientUse
	if fromClient {
		use = &clientUse{lease: s.lease}
	}
	for {
		if reader == nil {
			var err error
			mt, reader, err = src.NextReader()
			if err != nil {
				return readFailure(srcSide, err)
			}
		}
		if !s.closing.Load() {
			written, err := s.copyMessage(dst, mt, reader, use, buf)
			if err != nil {
				return readFailure(srcSide, err)
			}
			if written {
				reader = nil
				continue
			}
			if !s.closing.Load() {
				return closeInfo{side: dstSide, code: websocket.CloseAbnormalClosure, writeFailed: true}
			}
			// Warren sent its close messages while this message was in the
			// copy. After a close message, each write to that connection
			// fails, thus the failure does not show that dst went away.
			// Continue to read src. If this loop stopped, nothing would read
			// src. The connection to a client would then close with data that
			// nothing read, and the client can lose the close message and its
			// code.
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return readFailure(srcSide, err)
		}
		reader = nil
	}
}

// copyMessage copies one message from reader to dst. written is false if a
// write to dst failed: reader can then have more data of the message. err is
// an error of reader.
func (s *session) copyMessage(dst *websocket.Conn, mt int, reader io.Reader, use *clientUse, buf []byte) (written bool, err error) {
	timeout := s.proxy.opts.WriteTimeout
	// Each write has a time limit. If not, a peer that stops reading holds
	// the relay, and nothing sees that the other peer went away.
	_ = dst.SetWriteDeadline(time.Now().Add(timeout))
	writer, err := dst.NextWriter(mt)
	if err != nil {
		return false, nil
	}
	if use != nil {
		use.start(mt)
	}
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			if use != nil {
				use.data(buf[:n])
			}
			_ = dst.SetWriteDeadline(time.Now().Add(timeout))
			if _, werr := writer.Write(buf[:n]); werr != nil {
				return false, nil
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			// Do not close the writer. A close sends the last frame, and
			// the part would arrive as a complete message.
			return false, rerr
		}
	}
	if use != nil {
		use.end()
	}
	return writer.Close() == nil, nil
}

// finish waits until one side of a relay stops, until the backend stops the
// sandbox, or until warren stops. It then sends the close code to the other
// side, or to the two sides, waits for the reply, and closes the
// connections. It returns a text for the audit record.
//
// When the backend stops the sandbox, the client gets the code of
// stopClose, and the sandbox gets the code 1001. This is also the result when
// the sandbox side ends first and the lease has the stop signal at that time:
// refer to cause.
func (s *session) finish(clientDone, sandboxDone <-chan closeInfo) string {
	var first closeInfo
	clientRunning, sandboxRunning := true, true
	take := func(info closeInfo, fromClientPump bool) {
		first = info
		if fromClientPump {
			clientRunning = false
		} else {
			sandboxRunning = false
		}
	}
	select {
	case info := <-clientDone:
		take(info, true)
	case info := <-sandboxDone:
		take(info, false)
	case <-s.lease.Stopped():
		first, _ = backendStop(s.lease, s.watcher)
	case <-s.proxy.closing:
		first = closeInfo{side: sideWarren, code: websocket.CloseServiceRestart, text: "The server restarts"}
	}

	// A failed write says only that a side is gone. The reader of that
	// side gets the close code. Wait a short time for it.
	if first.writeFailed {
		timer := time.NewTimer(time.Second)
		select {
		case info := <-clientDone:
			clientRunning = false
			if !info.writeFailed {
				first = info
			}
		case info := <-sandboxDone:
			sandboxRunning = false
			if !info.writeFailed {
				first = info
			}
		case <-timer.C:
		}
		timer.Stop()
	}
	first = s.cause(first)

	s.closing.Store(true)
	switch first.side {
	case sideClient:
		closeConn(s.sandbox, clientCloseCode(first.code), first.text)
	case sideSandbox:
		closeConn(s.client, sandboxCloseCode(first.code), first.text)
	default:
		closeConn(s.client, first.code, first.text)
		closeConn(s.sandbox, websocket.CloseGoingAway, "")
	}

	// Wait for the reply to the close message. A client loses the close
	// code if the connection closes immediately after the message.
	deadline := time.Now().Add(closeWait)
	_ = s.client.NetConn().SetReadDeadline(deadline)
	_ = s.sandbox.NetConn().SetReadDeadline(deadline)
	timer := time.NewTimer(closeWait + time.Second)
	defer timer.Stop()
	for clientRunning || sandboxRunning {
		select {
		case <-clientDone:
			clientRunning = false
		case <-sandboxDone:
			sandboxRunning = false
		case <-timer.C:
			// A copy loop can wait in a write. Stop it.
			_ = s.client.Close()
			_ = s.sandbox.Close()
			if clientRunning {
				<-clientDone
			}
			if sandboxRunning {
				<-sandboxDone
			}
			clientRunning, sandboxRunning = false, false
		}
	}
	detail := fmt.Sprintf("closed by %s, code %d", first.side, first.code)
	if first.reason != "" {
		detail += ", reason " + first.reason
	}
	return detail
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

// closeConn sends a close message. The protocol permits a maximum of 123
// bytes of valid UTF-8 for the reason text.
func closeConn(conn *websocket.Conn, code int, text string) {
	if len(text) > 123 {
		text = text[:123]
	}
	for len(text) > 0 && !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, text), time.Now().Add(closeWait))
}

// closeAndWait sends a close message and reads until the peer answers, or
// until a time limit. Use it only when no other goroutine reads from conn.
func closeAndWait(conn *websocket.Conn, code int, text string) {
	closeConn(conn, code, text)
	_ = conn.SetReadDeadline(time.Now().Add(closeWait))
	for {
		if _, _, err := conn.NextReader(); err != nil {
			return
		}
	}
}
