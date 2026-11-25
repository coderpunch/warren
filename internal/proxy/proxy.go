// Package proxy sends authenticated requests to the sandbox of the caller.
//
// The proxy is the only path to a sandbox. It makes a new set of request
// headers for the sandbox, puts the private key of the sandbox in place of
// the connection key, and never sends a credential of Open WebUI to a
// sandbox.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

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
	Resolver backend.Resolver
	Logger   *slog.Logger

	// AllowSetCookie lets a sandbox set cookies in the browser of the user.
	// The default is false, because the cookies would belong to the origin
	// of Open WebUI.
	AllowSetCookie bool
	// AuthTimeout is the maximum time for the first WebSocket message.
	AuthTimeout time.Duration
	// MaxMessageBytes is the maximum size of one WebSocket message.
	MaxMessageBytes int64
}

// Proxy is an http.Handler for sandbox traffic.
type Proxy struct {
	opts Options
	rp   *httputil.ReverseProxy
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

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
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	p := &Proxy{opts: opts, dial: dialer.DialContext}
	p.rp = &httputil.ReverseProxy{
		Rewrite: p.rewrite,
		Transport: &http.Transport{
			// No environment proxy: a sandbox address is always direct.
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			DisableCompression:  true,
			MaxIdleConnsPerHost: 8,
			// The keep-alive time of the Open Terminal server is 5 seconds
			// by default. Warren must stop the use of an idle connection
			// before that time (docs/open-terminal-api.md, section 9).
			IdleConnTimeout:       3 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		// A negative value sends each write immediately. Downloads, long
		// requests, and event streams need this.
		FlushInterval:  -1,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
	}
	return p
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
		return scope.Identity{}, "", &routeError{http.StatusBadRequest, 4000, err.Error()}
	}
	sessionID, err := scope.SingleHeader(h, scope.HeaderSessionID)
	if err != nil {
		return scope.Identity{}, "", &routeError{http.StatusBadRequest, 4000, err.Error()}
	}
	return id, sessionID, nil
}

// resolveRoute finds the sandbox for an identity. Call it only after
// authentication, because the result shows which templates exist.
func (p *Proxy) resolveRoute(id scope.Identity, sessionID string) (route, *routeError) {
	if id.TemplateID == "" {
		return route{}, &routeError{http.StatusBadRequest, 4000, scope.HeaderTemplateID + " is necessary"}
	}
	pol, ok := p.opts.Policies.Get(id.TemplateID)
	if !ok {
		return route{}, &routeError{http.StatusNotFound, 4004, "Template not found"}
	}
	ref, err := scope.Resolve(pol.Scope, id)
	switch {
	case err == nil:
	case errors.Is(err, scope.ErrMissingChat), errors.Is(err, scope.ErrMissingModel):
		// Open WebUI uses status 409 for a context that is absent.
		return route{}, &routeError{http.StatusConflict, 4003, capitalize(err.Error())}
	default:
		return route{}, &routeError{http.StatusBadRequest, 4000, capitalize(err.Error())}
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

type proxyCtxKey struct{}

type proxyCtx struct {
	target backend.Target
	route  route
}

// ServeHTTP sends one HTTP request to a sandbox. The caller must
// authenticate the request first.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := validatePath(r.URL.Path); err != nil {
		httpx.Error(w, http.StatusBadRequest, "Invalid path")
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
	httpx.InfoFrom(r.Context()).Detail = rerr.detail
	httpx.Error(w, rerr.status, rerr.detail)
}

func (p *Proxy) serveRoute(w http.ResponseWriter, r *http.Request, rt route) {
	fillInfo(r.Context(), rt)
	target, err := p.opts.Resolver.Resolve(r.Context(), rt.ref)
	if err != nil {
		p.resolveFailed(w, r, err)
		return
	}
	ctx := context.WithValue(r.Context(), proxyCtxKey{}, &proxyCtx{target: target, route: rt})
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

func (p *Proxy) resolveFailed(w http.ResponseWriter, r *http.Request, err error) {
	info := httpx.InfoFrom(r.Context())
	switch {
	case errors.Is(err, context.Canceled):
		info.Detail = "client went away"
		// The client is not there. The status is for the log only.
		w.WriteHeader(499)
	case errors.Is(err, backend.ErrNotReady), errors.Is(err, context.DeadlineExceeded):
		info.Detail = "sandbox not ready"
		w.Header().Set("Retry-After", "5")
		httpx.Error(w, http.StatusServiceUnavailable, "The terminal is not ready. Try again in a short time.")
	default:
		info.Detail = "resolve: " + err.Error()
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
	if !p.opts.AllowSetCookie {
		resp.Header.Del("Set-Cookie")
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		resp.Header.Set("Location", relativeLocation(loc, pc.target.URL))
	}
	return nil
}

// relativeLocation changes a redirect that points to the sandbox address
// into a relative redirect. The framework of Open Terminal makes absolute
// redirects from the Host header (docs/open-terminal-api.md, section 7.3).
// The sandbox address must not go to a client.
func relativeLocation(loc string, target *url.URL) string {
	u, err := url.Parse(loc)
	if err != nil || !u.IsAbs() || !strings.EqualFold(u.Host, target.Host) {
		return loc
	}
	rel := url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery, Fragment: u.Fragment}
	if rel.Path == "" {
		rel.Path = "/"
	}
	return rel.String()
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	info := httpx.InfoFrom(r.Context())
	if errors.Is(err, context.Canceled) {
		info.Detail = "client went away"
		w.WriteHeader(499)
		return
	}
	info.Detail = "upstream: " + err.Error()
	p.opts.Logger.Warn("proxy error", "request_id", info.RequestID, "sandbox", info.Sandbox, "error", err.Error())
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		httpx.Error(w, http.StatusGatewayTimeout, "The terminal did not answer in time")
		return
	}
	httpx.Error(w, http.StatusBadGateway, "The terminal is not available")
}
