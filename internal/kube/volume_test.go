package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/coderpunch/warren/internal/backend"
	"github.com/coderpunch/warren/internal/scope"
)

func (e *testEnv) volumes() []corev1.PersistentVolumeClaim {
	e.t.Helper()
	list, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return list.Items
}

func (e *testEnv) volumeFromAPI(name string) *corev1.PersistentVolumeClaim {
	e.t.Helper()
	for _, v := range e.volumes() {
		if v.Name == name {
			return &v
		}
	}
	return nil
}

// stop deletes the pod of a sandbox and waits until the cache shows it.
func (e *testEnv) stop(req backend.Request) {
	e.t.Helper()
	if err := e.m.Stop(context.Background(), req.Ref); err != nil {
		e.t.Fatal(err)
	}
	waitFor(e.t, "stop of the sandbox", func() bool { return e.m.pod(req.Ref.Name()) == nil })
}

// waitVolume waits until the cache has the volume in the state of the API
// server.
func (e *testEnv) waitVolume(name string) {
	e.t.Helper()
	waitFor(e.t, "volume in the cache", func() bool {
		want := e.volumeFromAPI(name)
		got := e.m.volume(name)
		if want == nil || got == nil {
			return want == nil && got == nil
		}
		return got.Annotations[AnnotationUnusedSince] == want.Annotations[AnnotationUnusedSince]
	})
}

func chatKeptRef(user, chat string) backend.Request {
	return backend.Request{Ref: scope.Ref{TemplateID: "chat-kept", Kind: scope.KindChat, Value: chat}, UserID: user}
}

func TestPersistentPolicyMakesAVolume(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()

	vols := e.volumes()
	if len(vols) != 1 {
		t.Fatalf("volumes = %d, want 1", len(vols))
	}
	v := vols[0]
	if v.Name != name || v.Labels[LabelManagedBy] != ManagedByValue || v.Labels[LabelTemplate] != "kept" ||
		v.Labels[LabelOwner] != ownerDigest("alice") || v.Annotations[AnnotationContext] != "user:alice" ||
		v.Annotations[AnnotationCreatedBy] != "alice" {
		t.Errorf("volume metadata = %+v", v.ObjectMeta)
	}
	size := v.Spec.Resources.Requests[corev1.ResourceStorage]
	if size.String() != "5Gi" || v.Spec.StorageClassName == nil || *v.Spec.StorageClassName != "fast" ||
		len(v.Spec.AccessModes) != 1 || v.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("volume spec = %+v", v.Spec)
	}
	// No pod owns the volume. If a pod owned it, Kubernetes would delete
	// the volume with the pod.
	if len(v.OwnerReferences) != 0 {
		t.Errorf("the volume has an owner: %+v", v.OwnerReferences)
	}

	pod := e.pods()[0]
	var home *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == homeVolume {
			home = &pod.Spec.Volumes[i]
		}
	}
	if home == nil || home.PersistentVolumeClaim == nil || home.PersistentVolumeClaim.ClaimName != name || home.EmptyDir != nil {
		t.Fatalf("home volume of the pod = %+v", home)
	}
	c := pod.Spec.Containers[0]
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m.Name
	}
	if mounts["/home/user"] != homeVolume || mounts["/tmp"] != tmpVolume || c.WorkingDir != "/home/user" {
		t.Errorf("mounts = %v, working directory = %q", mounts, c.WorkingDir)
	}
	if sc := pod.Spec.SecurityContext; sc == nil || sc.FSGroup == nil || *sc.FSGroup != volumeGroup {
		t.Errorf("security context = %+v, want the group of the volume", pod.Spec.SecurityContext)
	}
	// A policy with no storage class uses the default class of the cluster.
	e.acquire(templateRef("kept-forever", "alice")).Release()
	if v := e.volumeFromAPI(templateRef("kept-forever", "alice").Ref.Name()); v == nil || v.Spec.StorageClassName != nil {
		t.Errorf("volume with no storage class = %+v", v)
	}
}

func TestEphemeralPolicyMakesNoVolume(t *testing.T) {
	e := newTestEnv(t, nil)
	e.acquire(userRef("alice")).Release()
	e.acquire(chatRef("alice", "c1")).Release()
	if n := len(e.volumes()); n != 0 {
		t.Fatalf("volumes = %d, want 0", n)
	}
	for _, pod := range e.pods() {
		if pod.Labels[LabelTemplate] != "personal" {
			continue
		}
		var home *corev1.Volume
		for i := range pod.Spec.Volumes {
			if pod.Spec.Volumes[i].Name == homeVolume {
				home = &pod.Spec.Volumes[i]
			}
		}
		if home == nil || home.EmptyDir == nil || home.EmptyDir.SizeLimit == nil || home.EmptyDir.SizeLimit.String() != "3Gi" {
			t.Fatalf("home volume = %+v, want an emptyDir with the size of the policy", home)
		}
		// The limit of the pod includes the home directory: 2Gi + 3Gi.
		limit := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage]
		if limit.String() != "5Gi" {
			t.Errorf("ephemeral storage limit = %s, want 5Gi", limit.String())
		}
	}
}

func TestVolumeStaysWhenThePodGoes(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	uid := e.volumeFromAPI(name).UID

	// The reaper stops the idle sandbox.
	e.clock.Advance(31 * time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "idle stop", func() bool { return e.m.pod(name) == nil })
	if v := e.volumeFromAPI(name); v == nil || v.UID != uid {
		t.Fatal("the volume did not stay after the idle stop")
	}

	// The next start uses the same volume.
	e.acquire(req).Release()
	vols := e.volumes()
	if len(vols) != 1 || vols[0].UID != uid {
		t.Fatalf("volumes after the second start = %+v", vols)
	}
	creates := 0
	for _, a := range e.client.Actions() {
		if a.Matches("create", "persistentvolumeclaims") {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("create requests for the volume = %d, want 1", creates)
	}

	// The stop and restart operations keep the volume.
	e.stop(req)
	if err := e.m.Restart(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod after restart", func() bool { return e.m.pod(name) != nil })
	if v := e.volumeFromAPI(name); v == nil || v.UID != uid {
		t.Error("the volume did not stay after stop and restart")
	}
}

func TestRetentionDeletesUnusedVolumes(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	forever := templateRef("kept-forever", "alice")
	e.acquire(req).Release()
	e.acquire(forever).Release()
	e.waitVolume(name)

	// While the sandbox operates, its volume has no mark, for any time.
	lease := e.acquire(req)
	e.clock.Advance(100 * time.Hour)
	e.m.Reap(context.Background())
	if v := e.volumeFromAPI(name); v == nil || v.Annotations[AnnotationUnusedSince] != "" {
		t.Fatalf("volume in use = %+v", v)
	}
	lease.Release()

	e.stop(req)
	e.stop(forever)
	stopped := e.clock.Now()
	e.m.Reap(context.Background())
	v := e.volumeFromAPI(name)
	if v == nil || v.Annotations[AnnotationUnusedSince] != stopped.UTC().Format(timeFormat) {
		t.Fatalf("mark after the stop = %+v", v)
	}
	e.waitVolume(name)
	e.waitVolume(forever.Ref.Name())

	// Before the end of the retention time (24h), the volume stays.
	e.clock.Advance(23 * time.Hour)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(name) == nil {
		t.Fatal("the reaper deleted the volume before the end of the retention time")
	}
	e.clock.Advance(2 * time.Hour)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(name) != nil {
		t.Error("the reaper did not delete the volume after the retention time")
	}
	// A policy with no retention time keeps its volume.
	e.clock.Advance(10000 * time.Hour)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(forever.Ref.Name()) == nil {
		t.Error("the reaper deleted a volume of a policy with no retention time")
	}
	if n := e.recordCount(); n != 0 {
		t.Errorf("records after the reaper = %d, want 0", n)
	}
}

// A start removes the mark. Thus the time of a previous stop does not count
// after the next stop.
func TestStartRemovesTheUnusedMark(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	e.stop(req)
	e.m.Reap(context.Background())
	e.waitVolume(name)
	if e.m.volume(name).Annotations[AnnotationUnusedSince] == "" {
		t.Fatal("no mark after the stop")
	}

	// 20 hours later the user comes back. The sandbox then operates for
	// 30 hours.
	e.clock.Advance(20 * time.Hour)
	lease := e.acquire(req)
	if v := e.volumeFromAPI(name); v == nil || v.Annotations[AnnotationUnusedSince] != "" {
		t.Fatalf("the start did not remove the mark: %+v", v)
	}
	e.waitVolume(name)
	e.clock.Advance(30 * time.Hour)
	e.m.Reap(context.Background())
	lease.Release()
	e.stop(req)
	e.m.Reap(context.Background())
	e.waitVolume(name)
	e.clock.Advance(time.Hour)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(name) == nil {
		t.Error("the reaper deleted a volume one hour after its last use")
	}
}

// A wrong mark on a volume that a pod uses must go away, and must never
// cause a delete.
func TestWrongMarkOnAVolumeInUse(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	old := e.clock.Now().Add(-1000 * time.Hour).UTC().Format(timeFormat)
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, AnnotationUnusedSince, old)
	if _, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Patch(context.Background(), name,
		types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	e.waitVolume(name)
	e.m.Reap(context.Background())
	v := e.volumeFromAPI(name)
	if v == nil {
		t.Fatal("the reaper deleted a volume that a pod uses")
	}
	if v.Annotations[AnnotationUnusedSince] != "" {
		t.Errorf("the wrong mark stays: %q", v.Annotations[AnnotationUnusedSince])
	}
}

// Warren keeps the volumes of a policy that is not in the policy file.
func TestVolumeOfAnUnknownPolicyStays(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("removed", "alice")
	pol := e.policy(templateRef("kept", "alice"))
	orphan := e.m.buildVolume(req.Ref.Name(), req, pol)
	orphan.Annotations[AnnotationUnusedSince] = e.clock.Now().Add(-10000 * time.Hour).UTC().Format(timeFormat)
	if _, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), orphan, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "volume in the cache", func() bool { return e.m.volume(req.Ref.Name()) != nil })
	e.m.Reap(context.Background())
	if e.volumeFromAPI(req.Ref.Name()) == nil {
		t.Error("the reaper deleted the volume of a policy that is not in the file")
	}
}

func modelKeptRef(user, model string) backend.Request {
	return backend.Request{Ref: scope.Ref{TemplateID: "model-kept", Kind: scope.KindModel, Value: model}, UserID: user}
}

func TestVolumeLimitForEachUser(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxVolumesPerUser = 2 })
	// The limit counts the volumes of the chat scope and the model scope
	// together.
	e.acquire(chatKeptRef("alice", "c1")).Release()
	e.acquire(modelKeptRef("alice", "agent:x")).Release()
	e.waitVolume(modelKeptRef("alice", "agent:x").Ref.Name())
	// The pods stop. The volumes stay, and they count.
	e.stop(chatKeptRef("alice", "c1"))
	e.stop(modelKeptRef("alice", "agent:x"))

	if _, err := e.m.Acquire(context.Background(), chatKeptRef("alice", "c3")); !errors.Is(err, backend.ErrVolumeLimit) {
		t.Fatalf("third volume: %v, want ErrVolumeLimit", err)
	}
	if _, err := e.m.Acquire(context.Background(), modelKeptRef("alice", "agent:y")); !errors.Is(err, backend.ErrVolumeLimit) {
		t.Fatalf("third volume, model scope: %v, want ErrVolumeLimit", err)
	}
	if len(e.volumes()) != 2 || len(e.pods()) != 0 {
		t.Errorf("after the refused starts: volumes = %d, pods = %d", len(e.volumes()), len(e.pods()))
	}
	if n := e.recordCount(); n != 0 {
		t.Errorf("records after the refused starts = %d, want 0", n)
	}
	// A sandbox that has its volume can start.
	e.acquire(chatKeptRef("alice", "c1")).Release()
	// A sandbox that keeps no data needs no volume.
	e.acquire(chatRef("alice", "c9")).Release()
	// The volumes of chats and models must not block the personal sandbox
	// of the user. A user has one such volume for each policy.
	e.acquire(templateRef("kept", "alice")).Release()
	e.acquire(templateRef("kept-forever", "alice")).Release()
	// The limit is for each user.
	e.acquire(chatKeptRef("bob", "c4")).Release()
	// A person who manages the template is exempt, as for the pod limit.
	if err := e.m.Start(context.Background(), chatKeptRef("alice", "c5")); err != nil {
		t.Errorf("start by a manager: %v", err)
	}
	// A reset gives a volume back.
	e.stop(chatKeptRef("alice", "c1"))
	e.stop(chatKeptRef("alice", "c5"))
	for _, c := range []string{"c1", "c5"} {
		if err := e.m.Delete(context.Background(), chatKeptRef("alice", c).Ref); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "volumes gone from the cache", func() bool {
		return e.m.volume(chatKeptRef("alice", "c1").Ref.Name()) == nil && e.m.volume(chatKeptRef("alice", "c5").Ref.Name()) == nil
	})
	e.acquire(chatKeptRef("alice", "c3")).Release()
}

func TestVolumeLimitCountsStartsInProgress(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxVolumesPerUser = 3 })
	results := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			_, err := e.m.Acquire(ctx, chatKeptRef("alice", fmt.Sprintf("c%d", i)))
			results <- err
		}()
	}
	refused := 0
	for i := 0; i < 10; i++ {
		if err := <-results; errors.Is(err, backend.ErrVolumeLimit) {
			refused++
		}
	}
	if n := len(e.volumes()); n != 3 || refused != 7 {
		t.Errorf("volumes = %d (want 3), refused starts = %d (want 7)", n, refused)
	}
}

// The total limit has no exemption. A person who manages a template can
// send start requests for user IDs that do not exist.
func TestTotalVolumeLimit(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxVolumes = 2 })
	e.acquire(templateRef("kept", "alice")).Release()
	e.acquire(templateRef("kept", "bob")).Release()
	e.waitVolume(templateRef("kept", "bob").Ref.Name())
	if _, err := e.m.Acquire(context.Background(), templateRef("kept", "carol")); !errors.Is(err, backend.ErrCapacity) {
		t.Errorf("third volume: %v, want ErrCapacity", err)
	}
	if err := e.m.Start(context.Background(), templateRef("kept", "invented-id")); !errors.Is(err, backend.ErrCapacity) {
		t.Errorf("third volume from the management API: %v, want ErrCapacity", err)
	}
	if n := len(e.volumes()); n != 2 {
		t.Errorf("volumes = %d, want 2", n)
	}
	// A sandbox with no volume can start.
	e.acquire(userRef("carol")).Release()
}

// A sandbox that has a volume and no pod stays on the account of the user
// that started it first, also when a manager starts it.
func TestManagerStartKeepsTheOwnerOfTheVolume(t *testing.T) {
	e := newTestEnv(t, nil)
	req := chatKeptRef("alice", "c1")
	name := req.Ref.Name()
	e.acquire(req).Release()
	e.waitVolume(name)
	e.stop(req)

	asManager := req
	asManager.UserID = "manager"
	if err := e.m.Start(context.Background(), asManager); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	pod := e.m.pod(name)
	if pod.Annotations[AnnotationCreatedBy] != "alice" || pod.Labels[LabelOwner] != ownerDigest("alice") {
		t.Errorf("the start by a manager changed the owner: %v", pod.Annotations)
	}
	e.stop(req)
	if err := e.m.Restart(context.Background(), asManager); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod after restart", func() bool { return e.m.pod(name) != nil })
	if got := e.m.pod(name).Annotations[AnnotationCreatedBy]; got != "alice" {
		t.Errorf("owner after the restart by a manager = %q", got)
	}
}

func TestResetDeletesTheVolume(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	oldVolume := e.volumeFromAPI(name).UID

	if err := e.m.Delete(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	// The reset starts nothing. It gives the volume back.
	if len(e.pods()) != 0 || len(e.volumes()) != 0 {
		t.Fatalf("after the reset: pods = %d, volumes = %d, want 0 and 0", len(e.pods()), len(e.volumes()))
	}
	waitFor(t, "cache after the reset", func() bool { return e.m.pod(name) == nil && e.m.volume(name) == nil })
	if got := e.m.InstanceStatus(req.Ref); got != StatusStopped {
		t.Errorf("status after the reset = %s", got)
	}
	if n := e.recordCount(); n != 0 {
		t.Errorf("records after the reset = %d, want 0", n)
	}

	// The next request starts the sandbox with a new volume.
	e.acquire(req).Release()
	if v := e.volumeFromAPI(name); v == nil || v.UID == oldVolume {
		t.Fatalf("volume after the next start = %+v, want a new volume", v)
	}

	// A reset of a sandbox that has no pod deletes the volume.
	e.stop(req)
	if err := e.m.Delete(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	if n := len(e.volumes()); n != 0 {
		t.Errorf("volumes after the reset of a stopped sandbox = %d", n)
	}
	// A reset of a sandbox that keeps no data stops the sandbox.
	plain := userRef("carol")
	e.acquire(plain).Release()
	if err := e.m.Delete(context.Background(), plain.Ref); err != nil {
		t.Fatal(err)
	}
	if n := len(e.pods()); n != 0 {
		t.Errorf("pods after the reset of a sandbox with no volume = %d", n)
	}
	// A reset of a sandbox that does not exist is not an error.
	if err := e.m.Delete(context.Background(), templateRef("kept", "nobody").Ref); err != nil {
		t.Errorf("reset of a sandbox that does not exist: %v", err)
	}
}

// A reset deletes the volume of a policy that keeps no data at this time.
// An administrator can change the storage type of a policy.
func TestResetDeletesTheVolumeOfAChangedPolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice") // The policy "personal" is ephemeral.
	old := e.m.buildVolume(req.Ref.Name(), req, e.policy(templateRef("kept", "alice")))
	if _, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "volume in the cache", func() bool { return e.m.volume(req.Ref.Name()) != nil })
	if err := e.m.Delete(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	if n := len(e.volumes()); n != 0 {
		t.Errorf("volumes = %d, want 0", n)
	}
}

// During a reset, no request gets the sandbox, and no request starts it.
func TestResetBlocksOtherRequests(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()

	e.m.mu.Lock()
	e.m.stateLocked(name).resetting = true
	e.m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := e.m.Acquire(ctx, req); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("lease during a reset: %v, want ErrNotReady", err)
	}
	// With no pod, a request must not start the sandbox.
	e.stop(req)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if _, err := e.m.Acquire(ctx2, req); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("start during a reset: %v, want ErrNotReady", err)
	}
	if n := len(e.pods()); n != 0 {
		t.Errorf("pods during a reset = %d, want 0", n)
	}
	// The reaper must not mark or delete the volume during a reset.
	e.m.Reap(context.Background())
	if v := e.volumeFromAPI(name); v == nil || v.Annotations[AnnotationUnusedSince] != "" {
		t.Errorf("the reaper changed the volume during a reset: %+v", v)
	}
	// A second reset at the same time is an error.
	e.m.mu.Lock()
	e.m.stateLocked(name).resetting = false
	e.m.mu.Unlock()
	e.acquire(req).Release()
}

// A volume in deletion cannot be the home directory of a new pod. The start
// waits until the volume is gone.
func TestStartWaitsForAVolumeInDeletion(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	deleted := metav1.NewTime(e.clock.Now())
	dying := e.m.buildVolume(name, req, e.policy(req))
	dying.DeletionTimestamp = &deleted
	dying.Finalizers = []string{"kubernetes.io/pvc-protection"}
	pvcs := e.client.CoreV1().PersistentVolumeClaims(testNamespace)
	if _, err := pvcs.Create(context.Background(), dying, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "volume in the cache", func() bool { return e.m.volume(name) != nil })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := e.m.Acquire(ctx, req); !errors.Is(err, backend.ErrNotReady) {
		t.Fatalf("start with a volume in deletion: %v, want ErrNotReady", err)
	}
	if n := len(e.pods()); n != 0 {
		t.Fatalf("pods = %d, want 0", n)
	}
	// The cache shows the deletion, thus the wait must send no request to
	// the API server. One attempt to start sends two read requests.
	gets := 0
	for _, a := range e.client.Actions() {
		if a.Matches("get", "persistentvolumeclaims") || a.Matches("get", "pods") {
			gets++
		}
	}
	if gets != 0 {
		t.Errorf("read requests during the wait = %d, want 0", gets)
	}

	// The volume goes away. The waiting request then starts the sandbox
	// with a new volume.
	res := e.acquireAsync(context.Background(), e.m, req)
	time.Sleep(50 * time.Millisecond)
	// Kubernetes removes the volume when no pod uses it.
	if err := e.client.Tracker().Delete(volumeResource, testNamespace, name); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	e.makeReady(name)
	if r := <-res; r.err != nil {
		t.Fatal(r.err)
	}
	if v := e.volumeFromAPI(name); v == nil || v.DeletionTimestamp != nil {
		t.Errorf("volume after the start = %+v", v)
	}
}

func TestVolumeThatWarrenDidNotMake(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: req.Ref.Name(), Namespace: testNamespace}}
	if _, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := e.m.Acquire(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "did not make") {
		t.Errorf("start on a volume of a different owner: %v", err)
	}
	if n := len(e.pods()); n != 0 {
		t.Errorf("pods = %d, want 0", n)
	}
}

// A failed start of the pod must not delete the volume, and must leave no
// Secret.
func TestPodFailureKeepsTheVolume(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	e.acquire(req).Release()
	e.stop(req)
	// The simulated cluster does not delete the Secret of the first pod.
	before := len(e.secrets())
	e.client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota exceeded")
	})
	if _, err := e.m.Acquire(context.Background(), req); err == nil {
		t.Fatal("no error for a pod that Kubernetes refuses")
	}
	if e.volumeFromAPI(req.Ref.Name()) == nil {
		t.Error("the failed start deleted the volume")
	}
	if n := len(e.secrets()); n != before {
		t.Errorf("secrets after the failed start = %d, want %d", n, before)
	}
}

// A start that makes a volume and then cannot make the pod must remove the
// volume. If not, each failed start leaves an empty volume that counts for
// the limits.
func TestFailedFirstStartRemovesItsVolume(t *testing.T) {
	e := newTestEnv(t, nil)
	e.client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("violates PodSecurity")
	})
	if _, err := e.m.Acquire(context.Background(), templateRef("kept", "alice")); err == nil {
		t.Fatal("no error for a pod that Kubernetes refuses")
	}
	if len(e.volumes()) != 0 || len(e.secrets()) != 0 {
		t.Errorf("after the failed start: volumes = %d, secrets = %d, want 0 and 0", len(e.volumes()), len(e.secrets()))
	}
}

// Warren must not remove a pod that stays in deletion when the pod has a
// volume and a node. The node can still write to the volume.
func TestStuckPodWithAVolumeIsNotForceDeleted(t *testing.T) {
	e := newTestEnv(t, nil)
	pods := e.client.CoreV1().Pods(testNamespace)
	deleted := metav1.NewTime(e.clock.Now())
	make := func(req backend.Request, node string) string {
		name := req.Ref.Name()
		pod := e.m.buildPod(name, name+"-00000000", req, e.policy(req))
		pod.DeletionTimestamp = &deleted
		pod.Finalizers = []string{"example.test/never"}
		pod.Spec.NodeName = node
		if _, err := pods.Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "pod in the cache", func() bool { return e.m.pod(name) != nil })
		return name
	}
	withVolume := make(templateRef("kept", "alice"), "node-1")
	notScheduled := make(templateRef("kept", "bob"), "")
	noVolume := make(userRef("carol"), "node-1")

	e.clock.Advance(10 * time.Minute)
	e.m.Reap(context.Background())
	left := map[string]bool{}
	for _, p := range e.pods() {
		left[p.Name] = true
	}
	if !left[withVolume] {
		t.Error("warren removed a pod with a volume on a node that does not answer")
	}
	// A pod with no node has no process, and a pod with no volume has no
	// data outside the pod.
	if left[notScheduled] || left[noVolume] {
		t.Errorf("pods left = %v, want only the pod with a volume and a node", left)
	}
	// The sandbox is not available while the pod exists.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := e.m.Acquire(ctx, templateRef("kept", "alice")); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("acquire with the previous pod in deletion: %v, want ErrNotReady", err)
	}
}

// Kubernetes replaces "$(NAME)" in the value of a variable. A policy value
// must stay as the file has it, and it must not get the key of the sandbox.
func TestPolicyVariablesAreNotExpanded(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("tuned", "alice")
	pol := e.policy(req)
	pol.Env = map[string]string{
		"LEAK":   "$(OPEN_TERMINAL_API_KEY)",
		"SCRIPT": "echo $HOME $$ $(date) $$(x)",
		// The start code must not let a policy replace these variables.
		"OPEN_TERMINAL_API_KEY":    "chosen-by-the-policy",
		"OPEN_TERMINAL_MULTI_USER": "true",
	}
	pod := e.m.buildPod(req.Ref.Name(), "secret-1", req, pol)
	got := map[string]corev1.EnvVar{}
	for _, v := range pod.Spec.Containers[0].Env {
		if _, dup := got[v.Name]; dup {
			t.Errorf("the variable %s occurs two times", v.Name)
		}
		got[v.Name] = v
	}
	// "$$" is the escape of Kubernetes for one "$".
	if got["LEAK"].Value != "$$(OPEN_TERMINAL_API_KEY)" {
		t.Errorf("LEAK = %q", got["LEAK"].Value)
	}
	if got["SCRIPT"].Value != "echo $$HOME $$$$ $$(date) $$$$(x)" {
		t.Errorf("SCRIPT = %q", got["SCRIPT"].Value)
	}
	if got["OPEN_TERMINAL_API_KEY"].ValueFrom == nil || got["OPEN_TERMINAL_API_KEY"].Value != "" || got["OPEN_TERMINAL_MULTI_USER"].Value != "false" {
		t.Errorf("a policy replaced a variable of warren: %+v", got)
	}
}

func TestInstancesIncludeStoppedSandboxesWithAVolume(t *testing.T) {
	e := newTestEnv(t, nil)
	alice, bob := templateRef("kept", "alice"), templateRef("kept", "bob")
	e.acquire(alice).Release()
	e.acquire(bob).Release()
	e.acquire(templateRef("kept-forever", "carol")).Release()
	e.waitVolume(bob.Ref.Name())
	e.stop(bob)

	got := map[string]string{}
	for _, i := range e.m.Instances("kept") {
		got[i.ContextID] = i.Status
		if i.ID == "" || i.CreatedBy == "" {
			t.Errorf("instance with no ID or no user: %+v", i)
		}
	}
	if len(got) != 2 || got["user:alice"] != StatusRunning || got["user:bob"] != StatusStopped {
		t.Errorf("instances = %v", got)
	}
}

func TestPolicySettingsAreInThePod(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("tuned", "alice")
	e.acquire(req).Release()
	pod := e.pods()[0]
	c := pod.Spec.Containers[0]
	if c.Image != "registry.example/tuned:2" || c.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("image = %s, pull policy = %s", c.Image, c.ImagePullPolicy)
	}
	if len(pod.Spec.ImagePullSecrets) != 2 || pod.Spec.ImagePullSecrets[0].Name != "pull-a" || pod.Spec.ImagePullSecrets[1].Name != "pull-b" {
		t.Errorf("pull secrets = %+v", pod.Spec.ImagePullSecrets)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Errorf("runtime class = %v", pod.Spec.RuntimeClassName)
	}
	want := map[string]string{
		"requests.cpu": "250m", "requests.memory": "512Mi", "requests.ephemeral-storage": "100Mi",
		// 1Gi of the policy and 2Gi for the home directory.
		"limits.cpu": "1500m", "limits.memory": "3Gi", "limits.ephemeral-storage": "3Gi",
	}
	for key, w := range want {
		kind, name, _ := strings.Cut(key, ".")
		list := c.Resources.Requests
		if kind == "limits" {
			list = c.Resources.Limits
		}
		q := list[corev1.ResourceName(name)]
		if q.String() != w {
			t.Errorf("%s = %s, want %s", key, q.String(), w)
		}
	}
	// The variables of warren come first. The variables of the policy
	// follow in the sequence of their names.
	var names []string
	for _, v := range c.Env {
		names = append(names, v.Name)
	}
	if got := strings.Join(names, ","); got != "OPEN_TERMINAL_API_KEY,HOME,OPEN_TERMINAL_ISOLATED,OPEN_TERMINAL_MULTI_USER,ALPHA,OPEN_TERMINAL_EXECUTE_TIMEOUT,ZED" {
		t.Errorf("variables = %s", got)
	}
	if pod.Spec.NodeSelector["pool"] != "sandboxes" || len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "dedicated" {
		t.Errorf("node selector = %v, tolerations = %+v", pod.Spec.NodeSelector, pod.Spec.Tolerations)
	}
	// A policy that sets no image uses the image of warren.
	e.acquire(userRef("bob")).Release()
	for _, p := range e.pods() {
		if p.Labels[LabelTemplate] == "personal" && p.Spec.Containers[0].Image != "example.test/terminal:1" {
			t.Errorf("default image = %s", p.Spec.Containers[0].Image)
		}
	}
}

func TestIdleTimeoutOfThePolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	quick, usual, never := templateRef("quick", "alice"), userRef("alice"), templateRef("never-idle", "alice")
	e.acquire(quick).Release()
	e.acquire(usual).Release()
	e.acquire(never).Release()
	if got := e.m.pod(quick.Ref.Name()).Annotations[AnnotationIdleTimeout]; got != "2m0s" {
		t.Errorf("idle time annotation = %q", got)
	}
	if _, ok := e.m.pod(usual.Ref.Name()).Annotations[AnnotationIdleTimeout]; ok {
		t.Error("a policy with no idle time must not set the annotation")
	}

	e.clock.Advance(3 * time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the quick sandbox", func() bool { return e.m.pod(quick.Ref.Name()) == nil })
	if e.m.pod(usual.Ref.Name()) == nil || e.m.pod(never.Ref.Name()) == nil {
		t.Fatal("the reaper stopped a sandbox before its idle time")
	}
	// The idle time of warren is 30 minutes in this test.
	e.clock.Advance(time.Hour)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the usual sandbox", func() bool { return e.m.pod(usual.Ref.Name()) == nil })
	if e.m.pod(never.Ref.Name()) == nil {
		t.Error("the reaper stopped a sandbox whose policy has the idle time 0")
	}
}

func TestStartWithNoPolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	_, err := e.m.Acquire(context.Background(), templateRef("absent", "alice"))
	if err == nil || len(e.pods()) != 0 {
		t.Errorf("start with no policy: err = %v, pods = %d", err, len(e.pods()))
	}
	if n := e.recordCount(); n != 0 {
		t.Errorf("records = %d, want 0", n)
	}
}

// After its start, warren deletes no volume for some time. A retention time
// that is too short in a new policy file must not delete volumes before an
// administrator can see the log record.
func TestNoVolumeDeleteAfterTheStartOfWarren(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.VolumeDeleteDelay = 10 * time.Minute })
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	old := e.m.buildVolume(name, req, e.policy(req))
	old.Annotations[AnnotationUnusedSince] = e.clock.Now().Add(-100 * time.Hour).UTC().Format(timeFormat)
	if _, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "volume in the cache", func() bool { return e.m.volume(name) != nil })

	e.clock.Advance(9 * time.Minute)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(name) == nil {
		t.Fatal("warren deleted a volume in the delay after its start")
	}
	e.clock.Advance(2 * time.Minute)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(name) != nil {
		t.Error("warren did not delete the volume after the delay")
	}
}

const onePolicy = "policies:\n  - {id: kept, name: Kept, scope: user, storage: {retention: %s}%s}\n"

// The reaper uses the policy file of this time. A longer retention time
// protects the volumes that are stopped at that time.
func TestRetentionComesFromTheCurrentPolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	e.stop(req)
	e.m.Reap(context.Background())
	e.waitVolume(name)

	// The file had 24h. The administrator changes it to 30 days.
	e.usePolicies(fmt.Sprintf(onePolicy, "30d", ""))
	e.clock.Advance(100 * time.Hour)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(name) == nil {
		t.Fatal("the reaper used the retention time of a previous policy file")
	}
	// A shorter time also applies to the volumes that exist.
	e.usePolicies(fmt.Sprintf(onePolicy, "48h", ""))
	e.m.Reap(context.Background())
	if e.volumeFromAPI(name) != nil {
		t.Error("the reaper did not use the shorter retention time of the new policy file")
	}
}

// An object has no policy when its policy is not in the file or has a
// different scope. No request can get to it.
func TestObjectsWithNoPolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	kept, chat := templateRef("kept", "alice"), chatKeptRef("alice", "c1")
	e.acquire(kept).Release()
	e.acquire(chat).Release()
	open := e.acquire(templateRef("quick", "alice"))
	e.waitVolume(chat.Ref.Name())

	// The new file has no policy "quick", and "chat-kept" has a different
	// scope.
	e.usePolicies(`
policies:
  - {id: kept, name: Kept, scope: user, storage: {retention: 24h}}
  - {id: chat-kept, name: Was for each chat, scope: user, storage: {retention: 24h}}
`)
	// The instance list does not show the volume and the pod of the
	// previous scope. Open WebUI would send an operation for that row to a
	// different sandbox.
	if got := e.m.Instances("chat-kept"); len(got) != 0 {
		t.Errorf("instances of the policy with a new scope = %+v, want none", got)
	}
	if got := e.m.Instances("kept"); len(got) != 1 {
		t.Errorf("instances of the policy that did not change = %+v", got)
	}
	e.m.mu.Lock()
	owner, total := e.m.volumeCountLocked(ownerDigest("alice"))
	e.m.mu.Unlock()
	if owner != 0 || total != 2 {
		t.Errorf("volume count = %d for the user and %d total, want 0 and 2", owner, total)
	}

	e.m.Reap(context.Background())
	waitFor(t, "stop of the pod with the previous scope", func() bool { return e.m.pod(chat.Ref.Name()) == nil })
	if e.m.pod(kept.Ref.Name()) == nil {
		t.Error("the reaper stopped a pod that has a policy")
	}
	// A pod with an open connection also stops. An open terminal must not
	// keep the sandbox of a policy that the administrator removed.
	waitFor(t, "stop of the pod with no policy", func() bool { return e.m.pod(templateRef("quick", "alice").Ref.Name()) == nil })
	open.Release()
	// Warren keeps the volumes that have no policy.
	e.clock.Advance(10000 * time.Hour)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(chat.Ref.Name()) == nil {
		t.Error("the reaper deleted a volume that has no policy")
	}
}

// A change of the idle time in the policy file applies to the pods that
// exist.
func TestIdleTimeoutComesFromTheCurrentPolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("never-idle", "alice")
	e.acquire(req).Release()
	e.clock.Advance(100 * time.Hour)
	e.m.Reap(context.Background())
	if e.m.pod(req.Ref.Name()) == nil {
		t.Fatal("the reaper stopped a sandbox whose policy has the idle time 0")
	}
	e.usePolicies("policies:\n  - {id: never-idle, name: Now with an idle time, scope: user, idleTimeout: 10m, storage: {type: ephemeral}}\n")
	e.m.Reap(context.Background())
	waitFor(t, "idle stop with the new policy", func() bool { return e.m.pod(req.Ref.Name()) == nil })
}

// A pod whose volume is in deletion cannot keep its files. Warren gives no
// lease on it and stops it. The next start makes a new volume.
func TestPodWhoseVolumeIsInDeletion(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	e.waitVolume(name)

	// A person deletes the volume with kubectl. Kubernetes keeps it while
	// the pod uses it.
	pvcs := e.client.CoreV1().PersistentVolumeClaims(testNamespace)
	v, err := pvcs.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deleted := metav1.NewTime(e.clock.Now())
	v.DeletionTimestamp = &deleted
	v.Finalizers = []string{"kubernetes.io/pvc-protection"}
	if _, err := pvcs.Update(context.Background(), v, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "deletion in the cache", func() bool { c := e.m.volume(name); return c != nil && c.DeletionTimestamp != nil })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := e.m.Acquire(ctx, req); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("lease on a pod whose volume is in deletion: %v, want ErrNotReady", err)
	}
	e.m.Reap(context.Background())
	waitFor(t, "stop of the pod", func() bool { return e.m.pod(name) == nil })

	// A pod whose volume does not exist cannot start. The reaper waits a
	// short time, because the cache of the volumes can be late.
	other := templateRef("kept", "bob")
	e.acquire(other).Release()
	e.waitVolume(other.Ref.Name())
	if err := pvcs.Delete(context.Background(), other.Ref.Name(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "volume gone from the cache", func() bool { return e.m.volume(other.Ref.Name()) == nil })
	// The watch of the volumes can be broken. The cache then has no volume
	// for a pod whose volume exists. The reaper must not stop that pod.
	third := templateRef("kept", "carol")
	e.acquire(third).Release()
	e.waitVolume(third.Ref.Name())
	if err := e.m.volumes.GetStore().Delete(e.m.volume(third.Ref.Name())); err != nil {
		t.Fatal(err)
	}
	e.m.Reap(context.Background())
	// The delete request is a part of the reaper run. The cache is not a
	// correct source here: it shows the stop of a pod some time later.
	if n := e.podDeletes(other.Ref.Name()); n != 0 {
		t.Fatal("the reaper stopped a new pod before the cache of the volumes can show its volume")
	}
	e.clock.Advance(time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "stop of the pod with no volume", func() bool { return e.m.pod(other.Ref.Name()) == nil })
	if n := e.podDeletes(third.Ref.Name()); n != 0 {
		t.Error("the reaper stopped a pod whose volume is in the API server and not in the cache")
	}
}

// If a different process changes the volume between the read and the write
// of a start, the start must not continue on old data.
func TestStartStopsWhenTheVolumeChangesUnderIt(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	e.stop(req)
	e.m.Reap(context.Background())
	e.waitVolume(name)

	conflicts := 1
	e.client.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(corev1.Resource("persistentvolumeclaims"), name, errors.New("changed"))
		}
		return false, nil, nil
	})
	// The first attempt gets a conflict and makes no pod. The request then
	// tries again and is successful.
	e.acquire(req).Release()
	if conflicts != 0 {
		t.Error("the start did not send the conditional change")
	}
	if v := e.volumeFromAPI(name); v == nil || v.Annotations[AnnotationUnusedSince] != "" {
		t.Errorf("volume after the start = %+v", v)
	}
}

// A reset clears the files and keeps the instance: the sandbox has a new,
// empty volume and no pod, and it stays in the instance list. A delete
// removes the instance.
func TestResetKeepsTheInstance(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxVolumesPerUser = 1 })
	req := chatKeptRef("alice", "c1")
	name := req.Ref.Name()
	e.acquire(req).Release()
	old := e.volumeFromAPI(name).UID

	if err := e.m.Reset(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	if n := len(e.pods()); n != 0 {
		t.Errorf("pods after the reset = %d, want 0: a reset does not start the sandbox", n)
	}
	v := e.volumeFromAPI(name)
	if v == nil || v.UID == old {
		t.Fatalf("volume after the reset = %+v, want a new volume", v)
	}
	// The new volume stays on the account of the same user. A reset has no
	// user ID, thus the user of the caller cannot change the account.
	if v.Annotations[AnnotationCreatedBy] != "alice" || v.Labels[LabelOwner] != ownerDigest("alice") ||
		v.Annotations[AnnotationContext] != "chat:c1" || v.Labels[LabelTemplate] != "chat-kept" {
		t.Errorf("metadata of the new volume = %+v", v.ObjectMeta)
	}
	waitFor(t, "new volume in the cache", func() bool { c := e.m.volume(name); return c != nil && c.UID == v.UID })
	list := e.m.Instances("chat-kept")
	if len(list) != 1 || list[0].Status != StatusStopped || list[0].ContextID != "chat:c1" {
		t.Errorf("instances after the reset = %+v, want one stopped instance", list)
	}
	// The instance keeps its place in the limit of the user. If the limit
	// does not refuse the start, the request must not wait for a pod that
	// no test makes ready.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := e.m.Acquire(ctx, chatKeptRef("alice", "c2")); !errors.Is(err, backend.ErrVolumeLimit) {
		t.Errorf("second chat volume after a reset: %v, want ErrVolumeLimit", err)
	}
	// The next request uses the empty volume.
	e.acquire(req).Release()
	if got := e.volumeFromAPI(name); got == nil || got.UID != v.UID {
		t.Errorf("the start after the reset did not use the new volume")
	}

	// A delete removes the instance and gives the volume back.
	if err := e.m.Delete(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "volume gone from the cache", func() bool { return e.m.volume(name) == nil })
	if len(e.volumes()) != 0 || len(e.m.Instances("chat-kept")) != 0 {
		t.Errorf("after the delete: volumes = %d, instances = %d, want 0 and 0", len(e.volumes()), len(e.m.Instances("chat-kept")))
	}
	e.acquire(chatKeptRef("alice", "c2")).Release()
}

// A reset makes no volume for a sandbox that had none.
func TestResetMakesNoVolumeForASandboxThatHadNone(t *testing.T) {
	e := newTestEnv(t, nil)
	// A sandbox that does not exist.
	if err := e.m.Reset(context.Background(), templateRef("kept", "nobody").Ref); err != nil {
		t.Fatal(err)
	}
	// A sandbox whose policy keeps no files.
	plain := userRef("carol")
	e.acquire(plain).Release()
	if err := e.m.Reset(context.Background(), plain.Ref); err != nil {
		t.Fatal(err)
	}
	if len(e.volumes()) != 0 || len(e.pods()) != 0 {
		t.Errorf("after the resets: volumes = %d, pods = %d, want 0 and 0", len(e.volumes()), len(e.pods()))
	}
	// A sandbox whose policy changed to ephemeral storage: the old volume
	// goes away, and warren makes no new volume.
	req := templateRef("kept", "alice")
	e.acquire(req).Release()
	e.stop(req)
	e.usePolicies("policies:\n  - {id: kept, name: Kept, scope: user, storage: {type: ephemeral}}\n")
	if err := e.m.Reset(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	if n := len(e.volumes()); n != 0 {
		t.Errorf("volumes after the reset of a sandbox with an ephemeral policy = %d, want 0", n)
	}
	if n := e.recordCount(); n != 0 {
		t.Errorf("records = %d, want 0", n)
	}
}
