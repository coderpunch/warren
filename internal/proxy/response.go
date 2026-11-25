package proxy

import (
	"net/http"
	"net/url"
	"path"
	"strings"
)

// isApplicationPath reports whether a path carries the content of an
// application and not a reply of Open Terminal: the port proxy and the
// browser view (docs/open-terminal-api.md, section 7).
func isApplicationPath(p string) bool {
	return strings.HasPrefix(p, "/proxy/") || strings.HasPrefix(p, "/browser/view/")
}

// terminalResponseHeaders are the response headers that warren passes for
// the routes of Open Terminal. Open WebUI sends the headers of a terminal
// server to the browser, where they apply to the origin of Open WebUI
// (docs/protocol.md, section 4.1). Thus all other headers stay in warren.
var terminalResponseHeaders = map[string]bool{
	"Accept-Ranges":          true,
	"Allow":                  true,
	"Cache-Control":          true,
	"Content-Disposition":    true,
	"Content-Encoding":       true,
	"Content-Language":       true,
	"Content-Length":         true,
	"Content-Range":          true,
	"Content-Type":           true,
	"Date":                   true,
	"Etag":                   true,
	"Expires":                true,
	"Last-Modified":          true,
	"Location":               true,
	"Retry-After":            true,
	"Vary":                   true,
	"X-Content-Type-Options": true,
}

// originHeaders are response headers that change the state of a browser for
// a full origin. An application in a sandbox must not set them for the
// origin of Open WebUI.
var originHeaders = []string{
	"Alt-Svc",
	"Clear-Site-Data",
	"Expect-Ct",
	"Nel",
	"Origin-Agent-Cluster",
	"Public-Key-Pins",
	"Public-Key-Pins-Report-Only",
	"Report-To",
	"Reporting-Endpoints",
	"Service-Worker-Allowed",
	"Strict-Transport-Security",
}

// filterResponseHeaders removes the response headers that a sandbox must
// not send to Open WebUI.
func filterResponseHeaders(h http.Header, reqPath string, allowSetCookie bool) {
	if !isApplicationPath(reqPath) {
		for name := range h {
			if !terminalResponseHeaders[name] {
				h.Del(name)
			}
		}
		return
	}
	if !allowSetCookie {
		h.Del("Set-Cookie")
	}
	for _, name := range originHeaders {
		h.Del(name)
	}
	for name := range h {
		// A reverse proxy in front of Open WebUI can obey these headers.
		if strings.HasPrefix(name, "X-Accel-") || strings.HasPrefix(name, "X-Sendfile") {
			h.Del(name)
		}
	}
}

// safeLocation examines the Location header of a sandbox response. It
// returns the value that warren sends, or false if warren must remove the
// header.
//
// Open WebUI sends a redirect of a terminal server to the browser. The
// browser then sends the request again, with the session token of the user,
// to the new address. Thus a sandbox must not select that address.
func safeLocation(loc string, reqURL, target *url.URL) (string, bool) {
	u, err := url.Parse(loc)
	if err != nil || strings.ContainsAny(loc, "\\\r\n\t") {
		return "", false
	}
	toSandbox := false
	if u.Scheme != "" || u.Host != "" {
		toSandbox = (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, target.Host)
	}
	reqPath := reqURL.Path

	// The framework of Open Terminal redirects a path that has a missing or
	// an extra "/" at its end. It makes an absolute URL from the Host
	// header, thus the URL has the address of the sandbox
	// (docs/open-terminal-api.md, section 7.3). Warren changes it into a
	// reference that is relative to the request path.
	if toSandbox {
		switch {
		case u.Path == reqPath+"/":
			return withQuery(path.Base(reqPath)+"/", u), true
		case reqPath != "/" && u.Path+"/" == reqPath:
			return withQuery("../"+path.Base(u.Path), u), true
		}
	}

	switch {
	case strings.HasPrefix(reqPath, "/proxy/"):
		// Open WebUI changes each redirect of the port proxy that stays on
		// the application into a path below the port prefix
		// (docs/protocol.md, section 8.3). The address of the sandbox must
		// not go to the client.
		if toSandbox {
			return withQuery("/"+strings.TrimLeft(u.Path, "/"), u), true
		}
		return loc, true

	case strings.HasPrefix(reqPath, "/browser/view/"):
		// Open Terminal makes a relative redirect that stays below the
		// view prefix (docs/open-terminal-api.md, section 7.2).
		if u.Scheme != "" || u.Host != "" || strings.HasPrefix(loc, "/") {
			return "", false
		}
		prefix := viewPrefix(reqPath)
		if prefix == "" || !strings.HasPrefix(reqURL.ResolveReference(u).Path, prefix) {
			return "", false
		}
		return loc, true
	}
	return "", false
}

// withQuery adds the query and the fragment of u to a reference.
func withQuery(ref string, u *url.URL) string {
	out := url.URL{Path: ref, RawQuery: u.RawQuery, Fragment: u.Fragment}
	return out.String()
}

// viewPrefix returns "/browser/view/{sid}/{token}/" for a browser view path.
func viewPrefix(p string) string {
	parts := strings.SplitN(p, "/", 6)
	// parts: "", "browser", "view", sid, token, rest
	if len(parts) < 6 || parts[3] == "" || parts[4] == "" {
		return ""
	}
	return strings.Join(parts[:5], "/") + "/"
}
