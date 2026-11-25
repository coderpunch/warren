package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/h3xcat/warren/internal/backend"
)

var volumeResource = corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims")

const protection = "kubernetes.io/pvc-protection"

// storedVolume returns the volume that the simulated API server has. It
// does not use the client: a reactor cannot call the client.
func (e *testEnv) storedVolume(name string) *corev1.PersistentVolumeClaim {
	obj, err := e.client.Tracker().Get(volumeResource, testNamespace, name)
	if err != nil {
		return nil
	}
	return obj.(*corev1.PersistentVolumeClaim).DeepCopy()
}

// storeVolume writes a volume to the simulated API server with a new
// resource version.
func (e *testEnv) storeVolume(v *corev1.PersistentVolumeClaim) {
	e.t.Helper()
	v.ResourceVersion = strconv.FormatInt(e.version.Add(1), 10)
	if err := e.client.Tracker().Update(volumeResource, v, testNamespace); err != nil {
		e.t.Errorf("write of a volume to the simulated API server: %v", err)
	}
}

// mergeJSON applies a JSON merge patch (RFC 7386) to an object.
func mergeJSON(object, patch map[string]any) {
	for key, value := range patch {
		part, isObject := value.(map[string]any)
		switch {
		case value == nil:
			delete(object, key)
		case isObject:
			target, ok := object[key].(map[string]any)
			if !ok {
				target = map[string]any{}
				object[key] = target
			}
			mergeJSON(target, part)
		default:
			object[key] = value
		}
	}
}

// strictVolumes makes the simulated API server do for a volume what the API
// server of Kubernetes does, and what the simulated cluster of client-go
// does not do:
//
//   - Each write gives the volume a new resource version.
//   - An update or a patch with a resource version that is not the current
//     version gets the status 409.
//   - A delete with a UID or a resource version that is not the current one
//     gets the status 409.
//   - A delete of a volume that has a finalizer sets the deletion time. The
//     volume stays until a test removes it.
//
// Without these rules, a test cannot find a write that lost its condition.
func (e *testEnv) strictVolumes() {
	conflict := func(name, text string) error {
		e.conflicts.Add(1)
		return apierrors.NewConflict(corev1.Resource("persistentvolumeclaims"), name, errors.New(text))
	}
	e.client.PrependReactor("create", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		v := action.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
		v.ResourceVersion = strconv.FormatInt(e.version.Add(1), 10)
		return false, nil, nil
	})
	e.client.PrependReactor("update", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		v := action.(k8stesting.UpdateAction).GetObject().(*corev1.PersistentVolumeClaim)
		cur := e.storedVolume(v.Name)
		if cur == nil {
			return false, nil, nil
		}
		if v.ResourceVersion != "" && v.ResourceVersion != cur.ResourceVersion {
			return true, nil, conflict(v.Name, "the object has been modified")
		}
		v.ResourceVersion = strconv.FormatInt(e.version.Add(1), 10)
		return false, nil, nil
	})
	e.client.PrependReactor("patch", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch := action.(k8stesting.PatchAction)
		cur := e.storedVolume(patch.GetName())
		if cur == nil {
			return false, nil, nil
		}
		if patch.GetPatchType() != types.MergePatchType {
			return true, nil, errors.New("the simulated API server has only the merge patch for a volume")
		}
		var change map[string]any
		if err := json.Unmarshal(patch.GetPatch(), &change); err != nil {
			return true, nil, err
		}
		if meta, ok := change["metadata"].(map[string]any); ok {
			if version, ok := meta["resourceVersion"]; ok && version != cur.ResourceVersion {
				return true, nil, conflict(cur.Name, "the object has been modified")
			}
		}
		raw, _ := json.Marshal(cur)
		var object map[string]any
		_ = json.Unmarshal(raw, &object)
		mergeJSON(object, change)
		raw, _ = json.Marshal(object)
		next := &corev1.PersistentVolumeClaim{}
		if err := json.Unmarshal(raw, next); err != nil {
			return true, nil, err
		}
		e.storeVolume(next)
		return true, next, nil
	})
	e.client.PrependReactor("delete", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del := action.(k8stesting.DeleteAction)
		cur := e.storedVolume(del.GetName())
		if cur == nil {
			return false, nil, nil
		}
		if pre := del.GetDeleteOptions().Preconditions; pre != nil {
			if pre.UID != nil && *pre.UID != cur.UID {
				return true, nil, conflict(cur.Name, "the UID in the precondition is not the UID of the object")
			}
			if pre.ResourceVersion != nil && *pre.ResourceVersion != cur.ResourceVersion {
				return true, nil, conflict(cur.Name, "the resource version in the precondition is not the version of the object")
			}
		}
		if len(cur.Finalizers) > 0 {
			if cur.DeletionTimestamp == nil {
				now := metav1.NewTime(e.clock.Now())
				cur.DeletionTimestamp = &now
				e.storeVolume(cur)
			}
			return true, nil, nil
		}
		return false, nil, nil
	})
}

// beforeVolumeRequest changes a volume in the simulated API server at the
// time of the next request with the given verb, before the API server
// examines the request. That is the work of a different process between a
// read and a write of warren.
func (e *testEnv) beforeVolumeRequest(verb, name string, change func(*corev1.PersistentVolumeClaim)) {
	var once sync.Once
	e.client.PrependReactor(verb, "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		once.Do(func() {
			if v := e.storedVolume(name); v != nil {
				change(v)
				e.storeVolume(v)
			}
		})
		return false, nil, nil
	})
}

// waitVolumeVersion waits until the cache has the volume in the version of
// the API server.
func (e *testEnv) waitVolumeVersion(name string) {
	e.t.Helper()
	waitFor(e.t, "current volume in the cache", func() bool {
		want, got := e.storedVolume(name), e.m.volume(name)
		if want == nil || got == nil {
			return want == nil && got == nil
		}
		return want.ResourceVersion == got.ResourceVersion && want.UID == got.UID
	})
}

// podDeletes returns the number of delete requests for a pod.
func (e *testEnv) podDeletes(name string) int {
	n := 0
	for _, a := range e.client.Actions() {
		if a.Matches("delete", "pods") && a.(k8stesting.DeleteAction).GetName() == name {
			n++
		}
	}
	return n
}

// volumeDeletes returns the number of delete requests for a volume.
func (e *testEnv) volumeDeletes(name string) int {
	n := 0
	for _, a := range e.client.Actions() {
		if a.Matches("delete", "persistentvolumeclaims") && a.(k8stesting.DeleteAction).GetName() == name {
			n++
		}
	}
	return n
}

// stoppedVolume makes a sandbox with a volume and stops its pod.
func (e *testEnv) stoppedVolume(req backend.Request) string {
	e.t.Helper()
	name := req.Ref.Name()
	e.acquire(req).Release()
	e.stop(req)
	e.waitVolumeVersion(name)
	return name
}

// The reaper must delete only the volume that it examined, in the state
// that it examined. A different warren process can start the sandbox after
// the cache of this process got the volume.
func TestReaperDoesNotDeleteAVolumeThatChanged(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := e.stoppedVolume(req)
	e.m.Reap(context.Background())
	e.waitVolumeVersion(name)
	e.clock.Advance(25 * time.Hour)

	// The start of the different process removes the mark. The cache of
	// this process shows the mark, and the retention time is at its end.
	e.beforeVolumeRequest("delete", name, func(v *corev1.PersistentVolumeClaim) {
		delete(v.Annotations, AnnotationUnusedSince)
	})
	e.m.Reap(context.Background())
	if e.volumeDeletes(name) != 1 {
		t.Fatalf("delete requests of the reaper = %d, want 1", e.volumeDeletes(name))
	}
	if e.volumeFromAPI(name) == nil {
		t.Fatal("the reaper deleted a volume that a different process took into use")
	}
	if n := e.conflicts.Load(); n != 1 {
		t.Errorf("refused requests = %d, want 1", n)
	}

	// The same for a volume that went away and came back with the same
	// name: the mark of the cache is not the mark of the new volume.
	e.waitVolumeVersion(name)
	e.m.Reap(context.Background())
	e.waitVolumeVersion(name)
	e.clock.Advance(25 * time.Hour)
	e.beforeVolumeRequest("delete", name, func(v *corev1.PersistentVolumeClaim) { v.UID = "uid-of-a-new-volume" })
	e.m.Reap(context.Background())
	if v := e.volumeFromAPI(name); v == nil || v.UID != "uid-of-a-new-volume" {
		t.Errorf("volume after the reaper = %+v, want the new volume", v)
	}
}

// The reaper must not write its mark to a volume that changed after the
// cache read it.
func TestReaperDoesNotMarkAVolumeThatChanged(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := e.stoppedVolume(req)
	e.beforeVolumeRequest("patch", name, func(v *corev1.PersistentVolumeClaim) {
		v.Annotations["example.test/changed"] = "yes"
	})
	e.m.Reap(context.Background())
	if v := e.volumeFromAPI(name); v == nil || v.Annotations[AnnotationUnusedSince] != "" {
		t.Fatalf("the reaper marked a volume that changed after the cache read it: %+v", v)
	}
	if n := e.conflicts.Load(); n != 1 {
		t.Errorf("refused requests = %d, want 1", n)
	}
	// The next run has the current volume and marks it.
	e.waitVolumeVersion(name)
	e.m.Reap(context.Background())
	if v := e.volumeFromAPI(name); v == nil || v.Annotations[AnnotationUnusedSince] == "" {
		t.Errorf("the reaper did not mark the current volume: %+v", v)
	}
}

// A start removes the mark of the volume with a condition: the volume must
// be the volume that the start read. If not, the start must read again.
func TestStartDoesNotChangeAVolumeThatChanged(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := e.stoppedVolume(req)
	e.m.Reap(context.Background())
	e.waitVolumeVersion(name)

	e.beforeVolumeRequest("patch", name, func(v *corev1.PersistentVolumeClaim) {
		v.Annotations["example.test/changed"] = "yes"
	})
	e.acquire(req).Release()
	if n := e.conflicts.Load(); n != 1 {
		t.Errorf("refused requests = %d, want 1: the start did not send the resource version that it read", n)
	}
	if v := e.volumeFromAPI(name); v == nil || v.Annotations[AnnotationUnusedSince] != "" || v.Annotations["example.test/changed"] != "yes" {
		t.Errorf("volume after the start = %+v", v)
	}
}

// A reset must delete only the volume that it read. If a different process
// made a new volume with the same name in that time, the new volume stays.
func TestResetDeletesOnlyTheVolumeThatItRead(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := e.stoppedVolume(req)
	e.beforeVolumeRequest("delete", name, func(v *corev1.PersistentVolumeClaim) { v.UID = "uid-of-a-new-volume" })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = e.m.Delete(ctx, req.Ref)
	if v := e.volumeFromAPI(name); v == nil || v.UID != "uid-of-a-new-volume" {
		t.Errorf("volume after the reset = %+v, want the new volume", v)
	}
}

// A start that cannot make its pod deletes the volume that it made, and no
// other volume with that name.
func TestFailedStartDeletesOnlyItsOwnVolume(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("violates PodSecurity")
	})
	e.beforeVolumeRequest("delete", name, func(v *corev1.PersistentVolumeClaim) { v.UID = "uid-of-a-new-volume" })
	if _, err := e.m.Acquire(context.Background(), req); err == nil {
		t.Fatal("no error for a pod that Kubernetes refuses")
	}
	if v := e.volumeFromAPI(name); v == nil || v.UID != "uid-of-a-new-volume" {
		t.Errorf("volume after the failed start = %+v, want the new volume", v)
	}
}

// While a reset is in progress, no request gets or starts the sandbox, and
// a second reset is an error.
func TestRequestsDuringAReset(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	old := e.volumeFromAPI(name).UID

	// Stop the reset in its delete request for the volume. The simulated
	// API server answers no other request in that time, thus the checks
	// below must not use the client.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.client.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return false, nil, nil
	})
	first := make(chan error, 1)
	go func() { first <- e.m.Delete(context.Background(), req.Ref) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the reset sent no delete request for the volume")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := e.m.Acquire(ctx, req); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("request during a reset: %v, want ErrNotReady", err)
	}
	e.m.mu.Lock()
	st := e.m.sandboxes[name]
	starting := st != nil && (st.starting || st.creating)
	e.m.mu.Unlock()
	if starting {
		t.Error("a request started the sandbox during its reset")
	}
	second := make(chan error, 1)
	go func() { second <- e.m.Delete(context.Background(), req.Ref) }()
	select {
	case err := <-second:
		if err == nil || !strings.Contains(err.Error(), "in progress") {
			t.Errorf("second reset at the same time: %v, want an error", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Error("a second reset at the same time did not return an error")
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if len(e.pods()) != 0 || len(e.volumes()) != 0 {
		t.Fatalf("after the reset: pods = %d, volumes = %d, want 0 and 0", len(e.pods()), len(e.volumes()))
	}
	waitFor(t, "cache after the reset", func() bool { return e.m.volume(name) == nil })
	e.acquire(req).Release()
	if v := e.volumeFromAPI(name); v == nil || v.UID == old {
		t.Errorf("volume after the next request = %+v, want a new volume", v)
	}
}

// A reset waits for a start request that is in progress. That request can
// make a pod after the reset found none.
func TestResetWaitsForAStartInProgress(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := e.stoppedVolume(req)
	e.m.mu.Lock()
	e.m.stateLocked(name).starting = true
	e.m.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- e.m.Delete(context.Background(), req.Ref) }()
	time.Sleep(100 * time.Millisecond)
	if n := e.volumeDeletes(name); n != 0 {
		t.Errorf("the reset deleted the volume while a start request was in progress")
	}
	e.m.mu.Lock()
	e.m.stateLocked(name).starting = false
	e.m.wakeLocked()
	e.m.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(e.volumes()); n != 0 {
		t.Errorf("volumes after the reset = %d, want 0", n)
	}
}

// A reset must not delete a volume that warren did not make.
func TestResetOfAVolumeThatWarrenDidNotMake(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: req.Ref.Name(), Namespace: testNamespace}}
	if _, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	err := e.m.Delete(context.Background(), req.Ref)
	if err == nil || !strings.Contains(err.Error(), "did not make") {
		t.Errorf("reset of a volume of a different owner: %v", err)
	}
	if e.volumeFromAPI(req.Ref.Name()) == nil {
		t.Error("the reset deleted a volume that warren did not make")
	}
}

// protect gives a volume the finalizer that Kubernetes gives to each
// volume. With it, a delete request does not remove the volume.
func (e *testEnv) protect(name string) {
	e.t.Helper()
	v := e.storedVolume(name)
	if v == nil {
		e.t.Fatalf("no volume %s", name)
	}
	v.Finalizers = []string{protection}
	e.storeVolume(v)
	e.waitVolumeVersion(name)
}

// Kubernetes keeps a volume in deletion while a pod uses it. If a different
// warren process makes a pod after the delete request of a reset, the reset
// must stop that pod. If not, the volume and the reset stay for all time.
func TestResetStopsAPodThatComesAfterItsDeleteRequest(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := e.stoppedVolume(req)
	e.protect(name)

	done := make(chan error, 1)
	go func() { done <- e.m.Delete(context.Background(), req.Ref) }()
	waitFor(t, "volume in deletion", func() bool { v := e.volumeFromAPI(name); return v != nil && v.DeletionTimestamp != nil })
	select {
	case err := <-done:
		t.Fatalf("the reset did not wait for the end of the volume: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// The pod of the different process.
	pod := e.m.buildPod(name, name+"-00000000", req, e.policy(req))
	if _, err := e.client.CoreV1().Pods(testNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stop of the pod that uses the volume in deletion", func() bool { return len(e.pods()) == 0 })
	// Kubernetes removes the volume when no pod uses it.
	if err := e.client.Tracker().Delete(volumeResource, testNamespace, name); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// After the delete request of a reset, a different process can make a new
// volume with the same name. The reset is then complete: the new volume is
// empty, and the reset must not wait for its end.
func TestResetEndsWhenANewVolumeHasTheName(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := e.stoppedVolume(req)
	e.protect(name)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.m.Delete(ctx, req.Ref) }()
	waitFor(t, "volume in deletion", func() bool { v := e.volumeFromAPI(name); return v != nil && v.DeletionTimestamp != nil })
	e.waitVolumeVersion(name)

	// The volume goes away, and the start of a different process makes a
	// new one. The cache gets the two changes as one.
	v := e.storedVolume(name)
	v.UID = "uid-of-a-new-volume"
	v.DeletionTimestamp, v.Finalizers = nil, nil
	e.storeVolume(v)
	if err := <-done; err != nil {
		t.Fatalf("reset with a new volume of the same name: %v", err)
	}
	if got := e.volumeFromAPI(name); got == nil || got.UID != "uid-of-a-new-volume" {
		t.Errorf("volume after the reset = %+v, want the new volume", got)
	}
}

// After a change of the policy file, warren deletes no volume for the same
// time as after its start.
func TestNoVolumeDeleteAfterAChangeOfThePolicies(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.VolumeDeleteDelay = 10 * time.Minute })
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	// The policy store uses the clock of the computer for the time of a
	// change. Set the clock of the test to that time. The start of warren
	// is then a long time in the past.
	e.clock.Advance(time.Since(e.clock.Now()))
	old := e.m.buildVolume(name, req, e.policy(req))
	old.Annotations[AnnotationUnusedSince] = e.clock.Now().Add(-100 * time.Hour).UTC().Format(timeFormat)
	if _, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "volume in the cache", func() bool { return e.m.volume(name) != nil })

	e.usePolicies("policies:\n  - {id: kept, name: Kept, scope: user, storage: {retention: 24h}}\n")
	e.clock.Advance(9 * time.Minute)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(name) == nil {
		t.Fatal("warren deleted a volume in the delay after a change of the policy file")
	}
	e.clock.Advance(2 * time.Minute)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(name) != nil {
		t.Error("warren did not delete the volume after the delay")
	}
}
