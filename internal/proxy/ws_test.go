package proxy

import (
	"context"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/coderpunch/warren/internal/backend"
)

// stopLease is a lease that has the stop signal when reason is not empty.
type stopLease struct {
	backend.Lease
	reason string
}

func (l stopLease) Stopped() <-chan struct{} {
	if l.reason == "" {
		return nil
	}
	c := make(chan struct{})
	close(c)
	return c
}

func (l stopLease) StopReason() string { return l.reason }

// A sandbox that stops sends its own close code, or it breaks the
// connection. The relay can see that event before it sees the stop signal of
// the lease. The client must get the code of warren also in that sequence.
func TestStopOfTheBackendIsTheCauseWhenTheSandboxSideEnds(t *testing.T) {
	stopped := &session{lease: stopLease{reason: backend.StopIdle}}
	want := closeInfo{side: sideWarren, code: closeNotFound, text: "The terminal stopped after its idle time", reason: backend.StopIdle}
	for name, first := range map[string]closeInfo{
		"close message of the sandbox": {side: sideSandbox, code: websocket.CloseServiceRestart},
		"connection that broke":        {side: sideSandbox, code: websocket.CloseAbnormalClosure},
		"write that failed":            {side: sideSandbox, code: websocket.CloseAbnormalClosure, writeFailed: true},
	} {
		if got := stopped.cause(first); got != want {
			t.Errorf("%s, with a stop signal: cause = %+v, want %+v", name, got, want)
		}
		// With no stop signal, the close of the sandbox is the cause.
		running := &session{lease: stopLease{}}
		if got := running.cause(first); got != first {
			t.Errorf("%s, with no stop signal: cause = %+v, want %+v", name, got, first)
		}
	}
	// A client that closes at the same time is the cause of the end. The
	// sandbox then gets the close code of the client.
	client := closeInfo{side: sideClient, code: websocket.CloseNormalClosure}
	if got := stopped.cause(client); got != client {
		t.Errorf("close of the client, with a stop signal: cause = %+v, want %+v", got, client)
	}
}

func TestBackendStopNeedsTheSignal(t *testing.T) {
	if _, ok := backendStop(stopLease{}); ok {
		t.Error("a lease with no stop signal gave a stop")
	}
	// The development backend stops nothing.
	lease, err := backend.Static{}.Acquire(context.Background(), backend.Request{WebSocket: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := backendStop(lease); ok {
		t.Error("the lease of the development backend gave a stop")
	}
	got, ok := backendStop(stopLease{reason: backend.StopAPI})
	want := closeInfo{side: sideWarren, code: websocket.CloseServiceRestart, text: "An operation stopped or restarted the terminal", reason: backend.StopAPI}
	if !ok || got != want {
		t.Errorf("stop by an operation = %+v, %v; want %+v", got, ok, want)
	}
}
