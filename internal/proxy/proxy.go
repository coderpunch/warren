// Package proxy sends authenticated requests to the sandbox of the caller.
//
// The proxy is the only path to a sandbox. It makes a new set of request
// headers for the sandbox, puts the private key of the sandbox in place of
// the connection key, and never sends a credential of Open WebUI to a
// sandbox. A sandbox is hostile: it runs the code of users and models. Thus
// the proxy also examines each response before Open WebUI gets it.
package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/h3xcat/warren/internal/auth"
	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/httpx"
	"github.com/h3xcat/warren/internal/policy"
	"github.com/h3xcat/warren/internal/scope"
)

// Policies gives the policy for a template ID.
type Policies interface {
	Get(id string) (policy.Policy, bool)
}

// Options configures a Proxy.
type Options struct {
	Keys     *auth.Keys
	Policies Policies
	Backend  backend.Backend
	Logger   *slog.Logger

	// AllowSetCookie lets a sandbox set cookies in the browser of the user.
	// The default is false, because the cookies would belong to the origin
	// of Open WebUI.
	AllowSetCookie bool
	// AuthTimeout is the maximum time for the first WebSocket message.
	AuthTimeout time.Duration
	// MaxMessageBytes is the maximum size of one WebSocket message.
	MaxMessageBytes int64
	// MaxPreAuthSockets is the maximum number of WebSockets that wait for
	// their first message at the same time.
	MaxPreAuthSockets int
	// WriteTimeout is the maximum time for one write to a sandbox or to a
	// WebSocket client. A peer that does not read for this time is stopped.
	WriteTimeout time.Duration
}

// Proxy is an http.Handler for sandbox traffic.
type Proxy struct {
	opts   Options
	rp     *httputil.ReverseProxy
	client *http.Client
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)

	preAuth chan struct{}

	// mu guards stopped. No socket starts after stopped is true.
	mu      sync.Mutex
	stopped bool
	closing chan struct{}
	sockets sync.WaitGroup
}

const (
	// maxErrorText is the maximum number of bytes of an error text in a
	// log record. A sandbox controls a part of these texts.
	maxErrorText = 256
	// maxSandboxHeaderBytes is the maximum size of the response header of
	// a sandbox.
	maxSandboxHeaderBytes = 64 << 10
)

// New makes a Proxy.
func New(opts Options) *Proxy {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.AuthTimeout <= 0 {
		opts.AuthTimeout = 10 * time.Second
	}
	if opts.MaxMessageBytes <= 0 {
		// Open WebUI permits 20 MiB on the agent WebSocket
		// (docs/protocol.md, section 4.7).
		opts.MaxMessageBytes = 32 << 20
	}
	if opts.MaxPreAuthSockets <= 0 {
		opts.MaxPreAuthSockets = 256
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 60 * time.Second
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	p := &Proxy{
		opts:    opts,
		dial:    dialer.DialContext,
		preAuth: make(chan struct{}, opts.MaxPreAuthSockets),
		closing: make(chan struct{}),
	}
	transport := &http.Transport{
		// No environment proxy: a sandbox address is always direct.
		Proxy: nil,
		// Each write to a sandbox has a time limit. If not, a sandbox that
		// stops reading holds an upload, and its goroutines, with no end.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &writeDeadlineConn{Conn: conn, timeout: opts.WriteTimeout}, nil
		},
		DisableCompression:  true,
		MaxIdleConnsPerHost: 8,
		// The keep-alive time of the Open Terminal server is 5 seconds by
		// default. Warren must stop the use of an idle connection before
		// that time (docs/open-terminal-api.md, section 9).
		IdleConnTimeout:        3 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxSandboxHeaderBytes,
	}
	p.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite:   p.rewrite,
		Transport: transport,
		// A negative value sends each write immediately. Downloads, long
		// requests, and event streams need this.
		FlushInterval:  -1,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
		ErrorLog:       slog.NewLogLogger(opts.Logger.Handler(), slog.LevelDebug),
	}
	return p
}

// writeDeadlineConn sets a new deadline before each write.
type writeDeadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *writeDeadlineConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// truncate cuts a text to a maximum number of bytes. It does not cut in the
// middle of a character.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "..."
}

// route is the result of the identity and policy checks for one request.
type route struct {
	id        scope.Identity
	sessionID string
	ref       scope.Ref
}

// routeError is a failure that the proxy can report with an HTTP status or
// with a WebSocket close code.
type routeError struct {
	status int
	close  int
	detail string
}

// readIdentity examines the identity headers. It does not look at policies,
// thus it is safe before authentication.
func readIdentity(h http.Header) (scope.Identity, string, *routeError) {
	id, err := scope.IdentityFromHeaders(h)
	if err != nil {
		return scope.Identity{}, "", &routeError{http.StatusBadRequest, closeBad, err.Error()}
	}
	sessionID, err := scope.SingleHeader(h, scope.HeaderSessionID)
	if err != nil {
		return scope.Identity{}, "", &routeError{http.StatusBadRequest, closeBad, err.Error()}
	}
	return id, sessionID, nil
}

// resolveRoute finds the sandbox for an identity. Call it only after
// authentication, because the result shows which templates exist.
func (p *Proxy) resolveRoute(id scope.Identity, sessionID string) (route, *routeError) {
	if id.TemplateID == "" {
		return route{}, &routeError{http.StatusBadRequest, closeBad, scope.HeaderTemplateID + " is necessary"}
	}
	pol, ok := p.opts.Policies.Get(id.TemplateID)
	if !ok {
		return route{}, &routeError{http.StatusNotFound, closeNotFound, "Template not found"}
	}
	ref, err := scope.Resolve(pol.Scope, id)
	switch {
	case err == nil:
	case errors.Is(err, scope.ErrMissingChat), errors.Is(err, scope.ErrMissingModel):
		// Open WebUI uses status 409 for a context that is absent.
		return route{}, &routeError{http.StatusConflict, closeForbidden, capitalize(err.Error())}
	default:
		return route{}, &routeError{http.StatusBadRequest, closeBad, capitalize(err.Error())}
	}
	return route{id: id, sessionID: sessionID, ref: ref}, nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func fillInfo(ctx context.Context, rt route) {
	info := httpx.InfoFrom(ctx)
	info.UserID = rt.id.UserID
	info.Template = rt.ref.TemplateID
	info.Context = rt.ref.ContextID()
	info.Sandbox = rt.ref.Name()
}

// startTimeout gives the maximum time that a request can wait for its
// sandbox. Open WebUI has a different time limit for each call
// (docs/protocol.md, section 9.4). A reply after that limit has no use.
func startTimeout(path string) time.Duration {
	switch path {
	case "/openapi.json":
		return 8 * time.Second // limit of Open WebUI: 10 seconds
	case "/files/cwd":
		return 4 * time.Second // 5 seconds
	case "/api/config", "/system":
		return 2 * time.Second // 3 seconds for the two calls
	}
	return 50 * time.Second // 60 seconds for helper calls
}

type proxyCtxKey struct{}

type proxyCtx struct {
	target backend.Target
	route  route
}

// ServeHTTP sends one HTTP request to a sandbox. The caller must
// authenticate the request first.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	info := httpx.InfoFrom(r.Context())
	if err := validatePath(r.URL.Path); err != nil {
		info.Detail = "invalid path"
		httpx.Error(w, http.StatusBadRequest, "Invalid path")
		return
	}
	// The pages of the framework show the OpenAPI document of a sandbox in
	// a browser. Warren does not relay them.
	switch r.URL.Path {
	case "/docs", "/redoc", "/docs/oauth2-redirect":
		httpx.Error(w, http.StatusNotFound, "Not found")
		return
	}
	id, sessionID, rerr := readIdentity(r.Header)
	if rerr == nil {
		var rt route
		rt, rerr = p.resolveRoute(id, sessionID)
		if rerr == nil {
			p.serveRoute(w, r, rt)
			return
		}
	}
	info.Detail = rerr.detail
	httpx.Error(w, rerr.status, rerr.detail)
}

func (p *Proxy) serveRoute(w http.ResponseWriter, r *http.Request, rt route) {
	fillInfo(r.Context(), rt)

	startCtx, cancel := context.WithTimeout(r.Context(), startTimeout(r.URL.Path))
	lease, err := p.opts.Backend.Acquire(startCtx, backend.Request{Ref: rt.ref, UserID: rt.id.UserID})
	cancel()
	if err != nil {
		p.acquireFailed(w, r, err)
		return
	}
	defer lease.Release()

	pc := &proxyCtx{target: lease.Target(), route: rt}
	if r.URL.Path == "/openapi.json" {
		p.serveOpenAPI(w, r, pc)
		return
	}
	ctx := context.WithValue(r.Context(), proxyCtxKey{}, pc)
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

func (p *Proxy) acquireFailed(w http.ResponseWriter, r *http.Request, err error) {
	info := httpx.InfoFrom(r.Context())
	switch {
	case r.Context().Err() != nil:
		info.Detail = "client went away"
		// The client is not there. The status is for the log only.
		w.WriteHeader(499)
	case errors.Is(err, backend.ErrNotReady), errors.Is(err, context.DeadlineExceeded):
		info.Detail = "sandbox not ready"
		w.Header().Set("Retry-After", "5")
		httpx.Error(w, http.StatusServiceUnavailable, "The terminal is not ready. Try again in a short time.")
	case errors.Is(err, backend.ErrLimit):
		info.Detail = "sandbox limit"
		httpx.Error(w, http.StatusTooManyRequests, "You have too many terminals. Stop one, then try again.")
	default:
		info.Detail = "acquire: " + truncate(err.Error(), maxErrorText)
		httpx.Error(w, http.StatusBadGateway, "The terminal is not available")
	}
}

// forwardedHeaders are the only request headers that warren copies to a
// sandbox. All other headers stay in warren. Thus a cookie, a user token, or
// an unknown identity header never gets to a sandbox.
var forwardedHeaders = []string{
	"Accept",
	"Accept-Encoding",
	"Accept-Language",
	"Access-Control-Request-Headers",
	"Access-Control-Request-Method",
	"Content-Encoding",
	"Content-Type",
	"If-Match",
	"If-Modified-Since",
	"If-None-Match",
	"If-Range",
	"If-Unmodified-Since",
	"Last-Event-Id",
	"Origin",
	"Range",
	"User-Agent",
}

// sandboxHeaders makes the request headers for a sandbox.
func sandboxHeaders(in http.Header, rt route, target backend.Target) http.Header {
	out := make(http.Header, len(forwardedHeaders)+3)
	for _, name := range forwardedHeaders {
		if values, ok := in[name]; ok {
			out[name] = append([]string(nil), values...)
		}
	}
	// Open Terminal uses these two headers as owner keys
	// (docs/open-terminal-api.md, section 5).
	out.Set(scope.HeaderUserID, rt.id.UserID)
	if rt.sessionID != "" {
		out.Set(scope.HeaderSessionID, rt.sessionID)
	}
	out.Set("Authorization", "Bearer "+target.APIKey)
	return out
}

func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	pc := pr.In.Context().Value(proxyCtxKey{}).(*proxyCtx)
	out := pr.Out
	out.URL.Scheme = pc.target.URL.Scheme
	out.URL.Host = pc.target.URL.Host
	// The path goes to the sandbox with no change, also in its encoded form.
	out.URL.Path = pr.In.URL.Path
	out.URL.RawPath = pr.In.URL.RawPath
	out.URL.RawQuery = filterQuery(pr.In.URL.Path, pr.In.URL.RawQuery)
	out.Host = pc.target.URL.Host
	out.Header = sandboxHeaders(pr.In.Header, pc.route, pc.target)
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	pc := resp.Request.Context().Value(proxyCtxKey{}).(*proxyCtx)
	reqPath := resp.Request.URL.Path
	filterResponseHeaders(resp.Header, reqPath, p.opts.AllowSetCookie)

	if loc := resp.Header.Get("Location"); loc != "" {
		if safe, ok := safeLocation(loc, resp.Request.URL, pc.target.URL); ok {
			resp.Header.Set("Location", safe)
		} else {
			resp.Header.Del("Location")
			httpx.InfoFrom(resp.Request.Context()).Detail = "removed a redirect of the sandbox"
		}
	}

	// A trailer is a header that comes after the body. It does not go
	// through the rules above, thus warren sends no trailers.
	resp.Trailer = nil
	resp.Header.Del("Trailer")
	resp.Body = &noTrailerBody{ReadCloser: resp.Body, resp: resp}
	return nil
}

// noTrailerBody removes the trailers that the transport adds when the body
// is complete.
type noTrailerBody struct {
	io.ReadCloser
	resp *http.Response
}

func (b *noTrailerBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.resp.Trailer = nil
	}
	return n, err
}

func (b *noTrailerBody) Close() error {
	b.resp.Trailer = nil
	return b.ReadCloser.Close()
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	info := httpx.InfoFrom(r.Context())
	// A read error of the request body also cancels the context of the
	// request. Thus this test finds each failure that the client caused.
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		info.Detail = "client went away"
		w.WriteHeader(499)
		return
	}
	text := truncate(err.Error(), maxErrorText)
	info.Detail = "upstream: " + text
	p.opts.Logger.Warn("proxy error", "request_id", info.RequestID, "sandbox", info.Sandbox, "error", text)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		httpx.Error(w, http.StatusGatewayTimeout, "The terminal did not answer in time")
		return
	}
	httpx.Error(w, http.StatusBadGateway, "The terminal is not available")
}

// Shutdown closes all open WebSockets with a close message and waits until
// their handlers stop, or until the context ends.
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if !p.stopped {
		p.stopped = true
		close(p.closing)
	}
	p.mu.Unlock()
	done := make(chan struct{})
	go func() {
		p.sockets.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
