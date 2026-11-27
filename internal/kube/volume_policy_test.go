package kube

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A volume that never got storage is empty. A start with a corrected policy
// must not use it again.
func TestStartReplacesAVolumeThatNeverBound(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice") // The policy has the class "fast".
	name := req.Ref.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// The pod never gets ready: its volume cannot bind.
	_, _ = e.m.Acquire(ctx, req)
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	e.waitVolume(name)
	first := e.volumeFromAPI(name)
	if first == nil || first.Spec.VolumeName != "" {
		t.Fatalf("volume = %+v, want a volume that is not bound", first)
	}
	e.clock.Advance(6 * time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the pod that is not ready", func() bool { return e.m.pod(name) == nil })

	// The administrator corrects the storage class.
	e.usePolicies("policies:\n  - {id: kept, name: Kept, scope: user, storage: {size: 5Gi, storageClassName: standard, retention: 24h}}\n")
	e.acquire(req).Release()
	v := e.volumeFromAPI(name)
	if v == nil || v.Spec.StorageClassName == nil || *v.Spec.StorageClassName != "standard" {
		t.Errorf("volume after the corrected policy = %+v, want the class standard", v.Spec)
	}
}

// After a change from persistent to ephemeral storage, the new pods do not
// use the volume. Its retention time must run while such a pod operates.
func TestRetentionRunsWhenThePodDoesNotUseTheVolume(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	e.stop(req)
	e.waitVolume(name)

	e.usePolicies("policies:\n  - {id: kept, name: Kept, scope: user, storage: {type: ephemeral, retention: 24h}}\n")
	lease := e.acquire(req)
	defer lease.Release()
	if usesVolume(e.m.pod(name)) {
		t.Fatal("the pod of an ephemeral policy uses a volume")
	}
	e.m.Reap(context.Background())
	e.waitVolume(name)
	if e.m.volume(name).Annotations[AnnotationUnusedSince] == "" {
		t.Fatal("no mark on a volume that no pod uses")
	}
	e.clock.Advance(25 * time.Hour)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(name) != nil {
		t.Error("the volume stays after its retention time, because a pod that does not use it operates")
	}
	_ = metav1.Now
}
