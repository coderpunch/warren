package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/h3xcat/warren/internal/backend"
)

// A start of the management API for a new sandbox must not use up the volume
// limit of the user whose ID is in the body.
func TestManagerStartDoesNotUseTheVolumeLimitOfAUser(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxVolumesPerUser = 2 })
	for i := 0; i < 3; i++ {
		req := chatKeptRef("victim", fmt.Sprintf("m%d", i))
		if err := e.m.Start(context.Background(), req); err != nil {
			t.Fatalf("management start %d: %v", i, err)
		}
		waitFor(t, "pod of the management start", func() bool { return e.m.pod(req.Ref.Name()) != nil })
		waitFor(t, "volume of the management start", func() bool { return e.m.volume(req.Ref.Name()) != nil })
		e.stop(req)
	}
	if n := len(e.volumes()); n != 3 {
		t.Fatalf("volumes = %d, want 3", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := e.m.Acquire(ctx, chatKeptRef("victim", "own-chat")); errors.Is(err, backend.ErrVolumeLimit) {
		t.Errorf("the user cannot start a chat after the starts of a manager: %v", err)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Each delete of a volume has an audit record.
func TestAuditRecordForTheVolumeOfAFailedStart(t *testing.T) {
	logs := &syncBuffer{}
	e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	e.client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("pods"), "", errors.New("exceeded quota"))
	})
	req := templateRef("kept", "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.m.Acquire(ctx, req); err == nil {
		t.Fatal("the start must fail")
	}
	waitFor(t, "delete of the new volume", func() bool { return e.volumeFromAPI(req.Ref.Name()) == nil })
	out := logs.String()
	created := strings.Count(out, `msg="volume created" log=audit`)
	deleted := strings.Count(out, `msg="volume deleted" log=audit`)
	if created == 0 {
		t.Fatalf("no audit record for the new volume:\n%s", out)
	}
	if deleted != created {
		t.Errorf("audit records: %d volume created, %d volume deleted; the volume is gone", created, deleted)
	}
}
