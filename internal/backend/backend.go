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
	// NoStart is true for a request that does not begin a start of the
	// sandbox. On a sandbox that operates, it is a usual request, with a
	// usual lease. On a sandbox that does not operate, it waits until a
	// different request or operation starts the sandbox, or until its context
	// ends. In that wait it has no lease, and it is not a use of the sandbox.
	// The proxy sets it for the WebSocket routes that only watch a sandbox
	// (docs/policies.md, section 10.3).
	NoStart bool
	// Hold is true for a WebSocket whose open connection is a use of the
	// sandbox, as an HTTP request in progress is: the backend does not stop
	// the sandbox for its idle time while the connection is open. The proxy
	// sets it for the WebSocket of an agent session, which the Open WebUI
	// backend keeps open for one chat turn (docs/policies.md, section 10.4).
	// It has an effect only together with WebSocket.
	Hold bool
}

// Lease is the permission to use a sandbox for one request or connection.
//
// While the lease of a request that is not a WebSocket is open, the backend
// does not stop the sandbox because of its idle time. The same applies to
// the lease of a WebSocket request with Hold. Each other stop reason applies
// also to a sandbox with such a lease.
//
// The lease of each other WebSocket request does not keep the sandbox. Its
// start is a use of the sandbox. After that, only a call of Active is a use:
// the backend can stop the sandbox for its idle time while the WebSocket is
// open, and the lease then gets the stop signal.
//
// The lease of a passive request does not keep the sandbox, and it is not a
// use of the sandbox.
type Lease interface {
	Target() Target
	// Active tells the backend that the client used the sandbox at this time
	// through the WebSocket of this lease. The proxy calls it for each
	// message of the client that is a use (docs/policies.md, section 10.4),
	// thus an implementation must make the call cheap, and safe at the same
	// time as each other method. It has an effect only for the lease of a
	// WebSocket request with no Hold, and only until Stopped closes or
	// Release returns.
	Active()
	// Stopped returns a channel that closes when the backend stops the
	// sandbox of this lease, or when it sees that a different actor deletes
	// it. The caller must then close its connection to the sandbox and call
	// Release. Only the lease of a WebSocket request gets this signal. For
	// each other lease, the channel never closes.
	Stopped() <-chan struct{}
	// StopReason returns the cause of the stop: one of the Stop values, or a
	// different text of the backend. It is empty until Stopped closes.
	StopReason() string
	// Release ends the lease. The caller must call it one time.
	Release()
}

// Stop reasons for which the proxy has a special close code. A backend can
// give more reasons. The proxy uses one code for each reason that is not in
// this list.
const (
	// StopAPI is a stop, restart, reset, or delete operation of the
	// management API.
	StopAPI = "api"
	// StopIdle is the stop of a sandbox that had no use for its idle time.
	StopIdle = "idle"
	// StopNoPolicy means that the policy of the sandbox is not in the
	// policy file, or that it has a different scope.
	StopNoPolicy = "no_policy"
	// StopNetworkChanged means that the policy file removed a network
	// permission of the sandbox.
	StopNetworkChanged = "network_changed"
	// StopVolumeGone means that the volume of the sandbox is absent or in
	// deletion.
	StopVolumeGone = "volume_gone"
	// StopExternal means that a different actor deletes the sandbox, for
	// example a person with kubectl, or Kubernetes when it drains a node.
	StopExternal = "external"
)

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

// Active does nothing: this backend has no idle time.
func (staticLease) Active() {}

// Stopped returns a channel that never closes: this backend stops nothing.
func (staticLease) Stopped() <-chan struct{} { return nil }
func (staticLease) StopReason() string       { return "" }
func (staticLease) Release()                 {}

// Acquire returns a lease on the one server. The one server always operates
// and has no idle time, thus the marks Passive, NoStart, and Hold of a
// request have no effect: each request gets the same lease.
func (s Static) Acquire(context.Context, Request) (Lease, error) {
	return staticLease{s.Server}, nil
}
