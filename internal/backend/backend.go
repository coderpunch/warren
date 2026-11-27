// Package backend gives the proxy access to sandboxes.
package backend

import (
	"context"
	"errors"
	"net/url"

	"github.com/coderpunch/warren/internal/scope"
)

// Target is one Open Terminal server that warren can call.
type Target struct {
	// URL is the base URL of the server, for example http://10.0.0.5:8000.
	URL *url.URL
	// APIKey is the private key of this server. Warren never sends it to
	// a client.
	APIKey string
}

// Request asks for the use of one sandbox.
type Request struct {
	// Ref identifies the sandbox.
	Ref scope.Ref
	// UserID is the user that Open WebUI gave for the request. For a shared
	// or model scope it is not part of Ref. A backend uses it for limits.
	UserID string
	// WebSocket is true for a connection that can stay open for a long time.
	WebSocket bool
	// Passive is true for a request that only reads the state of a sandbox
	// (docs/protocol.md, section 4.1.1). Such a request never starts the
	// sandbox, and it is not a use of the sandbox: the backend does not count
	// it as activity, and the lease does not keep the sandbox. The request
	// waits only for a start that a different request or operation began.
	Passive bool
}

// Lease is the permission to use a sandbox for one request or connection.
// While a lease is open, the backend must not stop the sandbox. The lease of
// a passive request is different: it does not keep the sandbox.
type Lease interface {
	Target() Target
	// Release ends the lease. The caller must call it one time.
	Release()
}

// Errors that a backend can return. The proxy changes them into an HTTP
// status or a WebSocket close code.
var (
	// ErrNotReady means that the sandbox did not become ready in time. The
	// client can try again.
	ErrNotReady = errors.New("sandbox is not ready")
	// ErrLimit means that the user has the maximum number of sandboxes.
	ErrLimit = errors.New("sandbox limit of the user reached")
	// ErrVolumeLimit means that the user has the maximum number of volumes.
	// A new sandbox would need one more.
	ErrVolumeLimit = errors.New("volume limit of the user reached")
	// ErrCapacity means that warren has its maximum number of sandboxes.
	// The user cannot correct this.
	ErrCapacity = errors.New("no capacity for a new sandbox")
	// ErrStopped means that the sandbox does not operate and that no start
	// is in progress. Only a passive request gets this error.
	ErrStopped = errors.New("sandbox is stopped")
)

// Backend gives leases on sandboxes. An implementation can start a sandbox
// and wait until it is ready. The context of Acquire has the deadline for
// that wait.
type Backend interface {
	Acquire(ctx context.Context, req Request) (Lease, error)
}

// Static is a Backend that sends all sandboxes to one server. It gives no
// isolation. Use it only for development.
type Static struct {
	Server Target
}

type staticLease struct{ target Target }

func (l staticLease) Target() Target { return l.target }
func (staticLease) Release()         {}

// Acquire returns a lease on the one server. The one server always operates,
// thus a passive request gets the same lease.
func (s Static) Acquire(context.Context, Request) (Lease, error) {
	return staticLease{s.Server}, nil
}
