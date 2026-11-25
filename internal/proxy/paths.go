package proxy

import (
	"errors"
	"net/url"
	"strings"
)

// validatePath examines the decoded request path. Open WebUI already makes
// the path safe (docs/protocol.md, section 4.1). Warren does the check again,
// because a different client with the connection key can also call warren.
func validatePath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return errors.New("path does not start with /")
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return errors.New("path contains a forbidden character")
		}
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return errors.New("path contains a dot segment")
		}
	}
	return nil
}

// passQueryUnchanged reports whether the query string belongs to an
// application and not to Open Terminal: the port proxy and the browser view.
func passQueryUnchanged(path string) bool {
	return strings.HasPrefix(path, "/proxy/") || strings.HasPrefix(path, "/browser/view/")
}

// filterQuery removes the user_id parameter. A browser can add this
// parameter to a request (docs/protocol.md, section 6.5). Open Terminal does
// not read it at this time. Warren removes it, so that it can never select a
// user. All other parameters keep their exact encoding.
func filterQuery(path, rawQuery string) string {
	if rawQuery == "" || passQueryUnchanged(path) {
		return rawQuery
	}
	parts := strings.Split(rawQuery, "&")
	kept := parts[:0]
	for _, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		if name, err := url.QueryUnescape(key); err == nil && name == "user_id" {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "&")
}
