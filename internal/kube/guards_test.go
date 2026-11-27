package kube

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// holdClient makes one request wait before it goes to the simulated client.
// A reactor cannot do that: the simulated client holds one lock for all
// requests, thus a reactor that waits stops all other requests.
type holdClient struct {
	kubernetes.Interface
	h *hold
}

type hold struct {
	verb, resource   string
	once             sync.Once
	waiting, release chan struct{}
}

// newHoldEnv makes a test environment in which the first request of one
// type waits until the test calls release.
func newHoldEnv(t *testing.T, verb, resource string) (e *testEnv, waiting <-chan struct{}, release func()) {
	h := &hold{verb: verb, resource: resource, waiting: make(chan struct{}), release: make(chan struct{})}
	e = newTestEnv(t, func(c *Config) { c.Client = holdClient{c.Client, h} })
	var done sync.Once
	release = func() { done.Do(func() { close(h.release) }) }
	t.Cleanup(release)
	return e, h.waiting, release
}

func (h *hold) wait(verb, resource string) {
	if verb == h.verb && resource == h.resource {
		h.once.Do(func() {
			close(h.waiting)
			<-h.release
		})
	}
}

func (c holdClient) CoreV1() typedcorev1.CoreV1Interface {
	return holdCore{c.Interface.CoreV1(), c.h}
}

type holdCore struct {
	typedcorev1.CoreV1Interface
	h *hold
}

func (c holdCore) Pods(namespace string) typedcorev1.PodInterface {
	return holdPods{c.CoreV1Interface.Pods(namespace), c.h}
}

func (c holdCore) PersistentVolumeClaims(namespace string) typedcorev1.PersistentVolumeClaimInterface {
	return holdVolumes{c.CoreV1Interface.PersistentVolumeClaims(namespace), c.h}
}

type holdPods struct {
	typedcorev1.PodInterface
	h *hold
}

func (p holdPods) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Pod, error) {
	p.h.wait("get", "pods")
	return p.PodInterface.Get(ctx, name, opts)
}

func (p holdPods) Create(ctx context.Context, pod *corev1.Pod, opts metav1.CreateOptions) (*corev1.Pod, error) {
	p.h.wait("create", "pods")
	return p.PodInterface.Create(ctx, pod, opts)
}

type holdVolumes struct {
	typedcorev1.PersistentVolumeClaimInterface
	h *hold
}

func (v holdVolumes) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, sub ...string) (*corev1.PersistentVolumeClaim, error) {
	v.h.wait("patch", "persistentvolumeclaims")
	return v.PersistentVolumeClaimInterface.Patch(ctx, name, pt, data, opts, sub...)
}

// The reaper must not delete or mark the volume of a start in progress.
func TestReaperKeepsTheVolumeOfAStartInProgress(t *testing.T) {
	e, waiting, release := newHoldEnv(t, "get", "pods")
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	// A volume that is past its retention time.
	old := e.m.buildVolume(name, req, e.policy(req))
	old.Annotations[AnnotationUnusedSince] = e.clock.Now().Add(-100 * time.Hour).UTC().Format(timeFormat)
	if _, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "volume in the cache", func() bool { return e.m.volume(name) != nil })
	uid := e.volumeFromAPI(name).UID

	// The start waits in its first request, before it removes the mark.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, req)
	<-waiting

	e.m.Reap(context.Background())
	if v := e.volumeFromAPI(name); v == nil || v.UID != uid {
		t.Fatal("the reaper deleted the volume of a start in progress")
	}
	release()
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	e.makeReady(name)
	if r := <-res; r.err != nil {
		t.Fatal(r.err)
	}
	if v := e.volumeFromAPI(name); v == nil || v.UID != uid || v.Annotations[AnnotationUnusedSince] != "" {
		t.Errorf("volume after the start = %+v, want the same volume with no mark", v)
	}
}

// A start must not begin while the reaper works on the volume.
func TestStartWaitsForTheReaper(t *testing.T) {
	e, waiting, release := newHoldEnv(t, "patch", "persistentvolumeclaims")
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	// A volume with no pod and no mark.
	if _, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), e.m.buildVolume(name, req, e.policy(req)), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "volume in the cache", func() bool { return e.m.volume(name) != nil })

	// The reaper waits in the request that marks the volume.
	reaped := make(chan struct{})
	go func() {
		e.m.Reap(context.Background())
		close(reaped)
	}()
	<-waiting

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := e.m.Acquire(ctx, req); err == nil {
		t.Error("a request got a lease while the reaper worked on the volume")
	}
	if n := len(e.pods()); n != 0 {
		t.Errorf("pods while the reaper works on the volume = %d, want 0", n)
	}
	release()
	<-reaped
	// The request after the reaper starts the sandbox and removes the mark.
	e.waitVolume(name)
	e.acquire(req).Release()
	if v := e.volumeFromAPI(name); v == nil || v.Annotations[AnnotationUnusedSince] != "" {
		t.Errorf("volume after the start = %+v, want no mark", v)
	}
}

// A reset waits for a start in progress, then removes the pod and the volume
// of that start. A second reset at the same time is an error.
func TestDeleteWaitsForAStartInProgress(t *testing.T) {
	e, waiting, release := newHoldEnv(t, "create", "pods")
	req := templateRef("kept", "alice")
	name := req.Ref.Name()

	// The start made the volume and the Secret. It waits in the request
	// that makes the pod. The client of the request goes away.
	ctx, cancel := context.WithCancel(context.Background())
	res := e.acquireAsync(ctx, e.m, req)
	<-waiting
	cancel()
	if r := <-res; r.err == nil {
		t.Fatal("a lease on a sandbox that has no pod")
	}

	reset := make(chan error, 1)
	go func() { reset <- e.m.Delete(context.Background(), req.Ref) }()
	waitFor(t, "start of the reset", func() bool {
		select {
		case err := <-reset:
			t.Fatalf("the reset did not wait for the start in progress: %v", err)
		default:
		}
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		st := e.m.sandboxes[name]
		return st != nil && st.resetting
	})
	if err := e.m.Delete(context.Background(), req.Ref); err == nil {
		t.Error("no error for a second reset at the same time")
	}
	select {
	case err := <-reset:
		t.Fatalf("the reset did not wait for the start in progress: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if e.volumeFromAPI(name) == nil {
		t.Error("the reset deleted the volume before the end of the start")
	}

	release()
	select {
	case err := <-reset:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reset did not end")
	}
	if len(e.pods()) != 0 || len(e.volumes()) != 0 {
		t.Errorf("after the reset: pods = %d, volumes = %d, want 0 and 0", len(e.pods()), len(e.volumes()))
	}
}
