// Package backend finds the network address and the key of a sandbox.
package backend

import (
	"context"
	"errors"
	"net/url"

	"github.com/h3xcat/warren/internal/scope"
)

// Target is one Open Terminal server that warren can call.
type Target struct {
	// URL is the base URL of the server, for example http://10.0.0.5:8000.
	URL *url.URL
	// APIKey is the private key of this server. Warren never sends it to
	// a client.
	APIKey string
}

// ErrNotReady means that the sandbox did not become ready in time. The
// client can try again.
var ErrNotReady = errors.New("sandbox is not ready")

// Resolver returns the target for a sandbox. An implementation can start the
// sandbox and wait until it is ready.
type Resolver interface {
	Resolve(ctx context.Context, ref scope.Ref) (Target, error)
}

// Static is a Resolver that sends all sandboxes to one server. It gives no
// isolation. Use it only for development.
type Static struct {
	Target Target
}

// Resolve returns the one target.
func (s Static) Resolve(context.Context, scope.Ref) (Target, error) {
	return s.Target, nil
}
