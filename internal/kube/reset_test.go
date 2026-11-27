package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/coderpunch/warren/internal/backend"
)

// The client of a reset can go away while Kubernetes removes the old volume.
// The reset must still make the new volume. If not, the result is a delete.
func TestResetMakesTheVolumeWhenTheClientGoesAway(t *testing.T) {
	e := newTestEnv(t, nil)
	req := chatKeptRef("alice", "c1")
	name := e.stoppedVolume(req)
	old := e.volumeFromAPI(name).UID
	e.protect(name)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.m.Reset(ctx, req.Ref) }()
	waitFor(t, "volume in deletion", func() bool { v := e.volumeFromAPI(name); return v != nil && v.DeletionTimestamp != nil })
	cancel()
	select {
	case err := <-done:
		t.Fatalf("the reset stopped between the delete and the new volume: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	// Kubernetes removes the old volume.
	if err := e.client.Tracker().Delete(volumeResource, testNamespace, name); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v := e.volumeFromAPI(name); v == nil || v.UID == old || v.Annotations[AnnotationCreatedBy] != "alice" {
		t.Fatalf("volume after the reset = %+v, want a new volume of alice", v)
	}
	waitFor(t, "one stopped instance in the list", func() bool {
		list := e.m.Instances("chat-kept")
		return len(list) == 1 && list[0].Status == StatusStopped
	})
}

// One refused create request must not change a reset into a delete. A
// ResourceQuota can count the deleted volume for a short time.
func TestResetTriesTheNewVolumeAgain(t *testing.T) {
	e := newTestEnv(t, nil)
	req := chatKeptRef("alice", "c1")
	name := e.stoppedVolume(req)
	refused := false
	e.client.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		if refused {
			return false, nil, nil
		}
		refused = true
		return true, nil, errors.New("exceeded quota")
	})
	if err := e.m.Reset(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	if !refused || e.volumeFromAPI(name) == nil {
		t.Errorf("refused = %v, volume = %+v, want a volume after the second create request", refused, e.volumeFromAPI(name))
	}
}

// When a reset ends, the cache has the new volume. A start immediately
// after the reset reads the account of the sandbox from the cache.
func TestStartImmediatelyAfterAReset(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxVolumesPerUser = 1 })
	req := modelKeptRef("alice", "m1")
	name := e.stoppedVolume(req)
	// bob is at the volume limit.
	e.stoppedVolume(chatKeptRef("bob", "b1"))

	if err := e.m.Reset(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	if v := e.m.volume(name); v == nil || v.DeletionTimestamp != nil {
		t.Fatalf("volume in the cache at the end of the reset = %+v, want the new volume", v)
	}
	// The sandbox has its volume, thus the volume limit of bob does not apply.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.m.Acquire(ctx, modelKeptRef("bob", "m1")); errors.Is(err, backend.ErrVolumeLimit) {
		t.Errorf("request for a sandbox that has its volume: %v", err)
	}
	waitFor(t, "pod of the request", func() bool { return e.m.pod(name) != nil })
	e.stop(req)
	// The start of a manager keeps the account.
	if err := e.m.Start(context.Background(), modelKeptRef("manager", "m1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod after the start", func() bool { return e.m.pod(name) != nil })
	if got := e.m.pod(name).Annotations[AnnotationCreatedBy]; got != "alice" {
		t.Errorf("account of the pod after a reset and a start = %q, want alice", got)
	}
}

// The instance keeps its place in the volume limit during the reset, while
// Kubernetes removes the old volume.
func TestResetKeepsItsPlaceInTheVolumeLimit(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxVolumesPerUser = 1 })
	req := chatKeptRef("alice", "c1")
	name := e.stoppedVolume(req)
	e.protect(name)

	done := make(chan error, 1)
	go func() { done <- e.m.Reset(context.Background(), req.Ref) }()
	waitFor(t, "volume in deletion in the cache", func() bool { v := e.m.volume(name); return v != nil && v.DeletionTimestamp != nil })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := e.m.Acquire(ctx, chatKeptRef("alice", "c2")); !errors.Is(err, backend.ErrVolumeLimit) {
		t.Errorf("second chat volume during a reset: %v, want ErrVolumeLimit", err)
	}
	if err := e.client.Tracker().Delete(volumeResource, testNamespace, name); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(e.volumes()); n != 1 {
		t.Errorf("volumes of alice after the reset = %d, want 1", n)
	}
}

// The empty volume of a reset has no storage until the next start. If the
// size of the policy changes in that time, the start replaces the volume.
// The new volume must stay on the account of the same user.
func TestReplacedVolumeKeepsItsAccount(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxVolumesPerUser = 1 })
	req := modelKeptRef("alice", "m1")
	name := e.stoppedVolume(req)
	// bob is at the volume limit.
	e.stoppedVolume(chatKeptRef("bob", "b1"))
	if err := e.m.Reset(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	e.waitVolumeVersion(name)
	old := e.volumeFromAPI(name).UID
	e.protect(name)
	// Kubernetes removes the volume a short time after the delete request.
	go func() {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if v := e.storedVolume(name); v != nil && v.DeletionTimestamp != nil {
				_ = e.client.Tracker().Delete(volumeResource, testNamespace, name)
				return
			}
		}
	}()

	e.usePolicies("policies:\n" +
		"  - {id: model-kept, name: Model with a volume, scope: model, storage: {type: persistent, size: 2Gi, retention: 48h}}\n" +
		"  - {id: chat-kept, name: Chat with a volume, scope: chat, storage: {type: persistent, retention: 48h}}\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, modelKeptRef("bob", "m1"))
	waitFor(t, "pod or refusal", func() bool { return e.m.pod(name) != nil || len(res) == 1 })
	if len(res) == 1 {
		t.Fatalf("request for a sandbox that has a volume: %v", (<-res).err)
	}
	v := e.volumeFromAPI(name)
	if v == nil || v.UID == old {
		t.Fatalf("volume after the start = %+v, want a new volume", v)
	}
	if got := v.Annotations[AnnotationCreatedBy]; got != "alice" || v.Labels[LabelOwner] != ownerDigest("alice") {
		t.Errorf("account of the replaced volume = %q, want alice", got)
	}
	if size := v.Spec.Resources.Requests["storage"]; size.String() != "2Gi" {
		t.Errorf("size of the replaced volume = %s, want 2Gi", size.String())
	}
}
