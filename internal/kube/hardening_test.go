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
	k8stesting "k8s.io/client-go/testing"

	"github.com/coderpunch/warren/internal/backend"
)

// These tests are for defects that an independent review of the lifecycle
// found. Each test fails on the code before the correction.

func (e *testEnv) creates() int {
	n := 0
	for _, a := range e.client.Actions() {
		if a.Matches("create", "pods") {
			n++
		}
	}
	return n
}

func (e *testEnv) recordCount() int {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	return len(e.m.sandboxes)
}

// A delete request for a pod that is gone must not block the next pod of
// the same sandbox.
func TestDeleteOfAPodThatIsGone(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	old := e.m.pod(name)

	// A different actor deletes the pod, and the cache shows that.
	if err := e.client.CoreV1().Pods(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod gone from the cache", func() bool { return e.m.pod(name) == nil })
	// The reaper has an old copy of the pod and sends its delete request.
	if err := e.m.deletePod(context.Background(), old, "idle", notIdle); err != nil {
		t.Fatalf("deletePod for a pod that is gone = %v", err)
	}

	lease := e.acquire(req)
	lease.Release()
	// The new pod can stop.
	if err := e.m.Stop(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stop of the new pod", func() bool { return e.m.pod(name) == nil })
}

// The activity of a previous pod must not make the next pod idle.
func TestActivityOfAPreviousPod(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	lease := e.acquire(req) // for example an open terminal
	if err := e.m.Stop(context.Background(), req.Ref); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stop", func() bool { return e.m.pod(name) == nil })
	lease.Release()

	e.clock.Advance(2 * time.Hour)
	if err := e.m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "new pod", func() bool { return e.m.pod(name) != nil })
	e.makeReady(name)
	e.clock.Advance(20 * time.Second)
	e.m.Reap(context.Background())
	if len(e.pods()) != 1 {
		t.Error("the reaper deleted a new pod because of the activity time of the previous pod")
	}
}

// A request that warren refuses must not leave a record in memory.
func TestRefusedRequestsLeaveNoRecords(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxPerUser = 1 })
	e.acquire(chatRef("alice", "c0")).Release()
	for i := 0; i < 200; i++ {
		// A user selects a different chat, or a different "agent:" model.
		if _, err := e.m.Acquire(context.Background(), chatRef("alice", fmt.Sprintf("chat-%d", i))); !errors.Is(err, backend.ErrLimit) {
			t.Fatalf("request %d: %v, want ErrLimit", i, err)
		}
	}
	if n := e.recordCount(); n > 1 {
		t.Errorf("records after 200 refused requests = %d, want 1", n)
	}
	// The record of a sandbox goes away with its pod.
	_ = e.m.Stop(context.Background(), chatRef("alice", "c0").Ref)
	// Wait for the cache, not only for the API server.
	waitFor(t, "stop", func() bool { return e.m.pod(chatRef("alice", "c0").Ref.Name()) == nil })
	e.m.Reap(context.Background())
	if n := e.recordCount(); n != 0 {
		t.Errorf("records with no sandbox = %d, want 0", n)
	}
}

// A sandbox that refuses its key again and again must not make warren
// start and delete pods with no end.
func TestRefusedKeyHasALimit(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.mu.Lock()
	e.badKeys = 1000
	e.mu.Unlock()

	// A kubelet that makes each new pod ready.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if p := e.m.pod(name); p != nil && !podReady(p) && p.DeletionTimestamp == nil {
				pods := e.client.CoreV1().Pods(testNamespace)
				if live, err := pods.Get(context.Background(), name, metav1.GetOptions{}); err == nil {
					readyStatus(live)
					_, _ = pods.UpdateStatus(context.Background(), live, metav1.UpdateOptions{})
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := e.m.Acquire(ctx, req)
	if err == nil || errors.Is(err, backend.ErrNotReady) {
		t.Fatalf("Acquire = %v, want an error about the key", err)
	}
	if n := e.creates(); n != maxBadKeys {
		t.Errorf("pods made = %d, want %d", n, maxBadKeys)
	}
	// A request in the wait time starts no pod.
	if _, err := e.m.Acquire(ctx, req); err == nil {
		t.Error("a request in the wait time got a lease")
	}
	if n := e.creates(); n != maxBadKeys {
		t.Errorf("pods made in the wait time = %d, want %d", n, maxBadKeys)
	}
	// After the wait time, warren tries again. A sandbox with a correct
	// key removes the record.
	e.clock.Advance(time.Minute)
	e.mu.Lock()
	e.badKeys = 0
	e.mu.Unlock()
	lease, err := e.m.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("Acquire after the wait time: %v", err)
	}
	lease.Release()
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	if len(e.m.badKeys) != 0 {
		t.Error("the record of refused keys stays after a successful key check")
	}
}

// A restart by a manager must not move a sandbox to the account of the
// manager, and it must work when the owner is at the limit.
func TestRestartKeepsTheOwner(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxPerUser = 1 })
	alice := chatRef("alice", "c1")
	name := alice.Ref.Name()
	e.acquire(alice).Release()
	e.acquire(userRef("manager")).Release() // the manager is at the limit

	// Open WebUI sends the ID of the manager for the sandbox of a chat.
	byManager := backend.Request{Ref: alice.Ref, UserID: "manager"}
	done := make(chan error, 1)
	go func() { done <- e.m.Restart(context.Background(), byManager) }()
	if err := <-done; err != nil {
		t.Fatalf("Restart = %v", err)
	}
	waitFor(t, "new pod", func() bool { return e.m.pod(name) != nil })
	pod := e.m.pod(name)
	if pod.Annotations[AnnotationCreatedBy] != "alice" || pod.Labels[LabelOwner] != ownerDigest("alice") {
		t.Errorf("after the restart, the sandbox is on the account of %q", pod.Annotations[AnnotationCreatedBy])
	}
}

// The management API starts the sandbox of a chat for other users. The
// limit for each user does not apply, but the limit for all sandboxes does.
func TestStartByTheManagementAPI(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.MaxPerUser = 1; c.MaxTotal = 3 })
	e.acquire(userRef("manager")).Release()
	for _, chat := range []string{"c1", "c2"} {
		if err := e.m.Start(context.Background(), backend.Request{Ref: chatRef("x", chat).Ref, UserID: "manager"}); err != nil {
			t.Fatalf("Start %s = %v", chat, err)
		}
	}
	if err := e.m.Start(context.Background(), backend.Request{Ref: chatRef("x", "c3").Ref, UserID: "manager"}); !errors.Is(err, backend.ErrCapacity) {
		t.Errorf("Start above the total limit = %v, want ErrCapacity", err)
	}
	// Sandbox traffic of the same user has the limit.
	if _, err := e.m.Acquire(context.Background(), chatRef("manager", "c9")); !errors.Is(err, backend.ErrLimit) && !errors.Is(err, backend.ErrCapacity) {
		t.Errorf("Acquire = %v, want a limit error", err)
	}
}

// Each start has its own Secret. A stop removes it.
func TestEachStartHasItsOwnSecret(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	first := e.acquire(req)
	firstSecret := podSecretName(e.m.pod(name))
	first.Release()
	if firstSecret == "" || firstSecret == name {
		t.Fatalf("Secret name = %q", firstSecret)
	}
	_ = e.m.Stop(context.Background(), req.Ref)
	waitFor(t, "stop", func() bool { return e.m.pod(name) == nil })

	second := e.acquire(req)
	defer second.Release()
	secondSecret := podSecretName(e.m.pod(name))
	if secondSecret == firstSecret || second.Target().APIKey == first.Target().APIKey {
		t.Error("the second start has the Secret or the key of the first start")
	}
	// The Secret of the first start has no pod. The reaper deletes it.
	e.clock.Advance(3 * time.Minute)
	e.m.Reap(context.Background())
	secrets := e.secrets()
	if len(secrets) != 1 || secrets[0].Name != secondSecret {
		t.Errorf("secrets after the reaper = %d, want only %s", len(secrets), secondSecret)
	}
}

// A container that started again can have a different key.
func TestKeyCheckAfterAContainerRestart(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	// The Secret changes, and the container starts again with the new key.
	secrets := e.client.CoreV1().Secrets(testNamespace)
	s, err := secrets.Get(context.Background(), podSecretName(e.m.pod(name)), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.Data[secretKeyName] = []byte("a-new-key-from-a-different-source")
	if _, err := secrets.Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.setStatus(name, func(p *corev1.Pod) {
		readyStatus(p)
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerName, RestartCount: 1}}
	})
	waitFor(t, "restart count in the cache", func() bool { return containerRestarts(e.m.pod(name)) == 1 })

	lease, err := e.m.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Target().APIKey != "a-new-key-from-a-different-source" {
		t.Error("warren uses the old key after a restart of the container")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.verified) != 2 {
		t.Errorf("key checks = %d, want 2", len(e.verified))
	}
}

// Before the pod cache has its first list, warren cannot count sandboxes.
func TestNoOperationBeforeTheCacheIsReady(t *testing.T) {
	e := newTestEnv(t, nil)
	m, err := New(e.m.cfg) // the cache of this manager did not start
	if err != nil {
		t.Fatal(err)
	}
	req := userRef("alice")
	if _, err := m.Acquire(context.Background(), req); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Acquire = %v, want ErrNotReady", err)
	}
	if err := m.Start(context.Background(), req); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Start = %v, want ErrNotReady", err)
	}
	if err := m.Stop(context.Background(), req.Ref); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Stop = %v, want ErrNotReady", err)
	}
	if err := m.Restart(context.Background(), req); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Restart = %v, want ErrNotReady", err)
	}
	if len(e.pods()) != 0 {
		t.Error("a manager with no cache made a pod")
	}
}

// A pod that has the name of a sandbox, but that warren did not make.
func TestPodThatWarrenDidNotMake(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: req.Ref.Name(), Namespace: testNamespace}}
	if _, err := e.client.CoreV1().Pods(testNamespace).Create(context.Background(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := e.m.Acquire(ctx, req)
	if err == nil || errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Acquire = %v, want an error that names the pod", err)
	}
	if n := len(e.secrets()); n != 0 {
		t.Errorf("secrets = %d, want 0", n)
	}
}

// A pod that stays in deletion must not block its sandbox with no end.
func TestPodThatStaysInDeletion(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	deleted := metav1.NewTime(e.clock.Now())
	stuck := e.m.buildPod(name, name+"-00000000", req, e.policy(req))
	stuck.DeletionTimestamp = &deleted
	stuck.Finalizers = []string{"example.test/never"}
	if _, err := e.client.CoreV1().Pods(testNamespace).Create(context.Background(), stuck, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod in the cache", func() bool { return e.m.pod(name) != nil })

	e.clock.Advance(time.Minute)
	e.m.Reap(context.Background())
	if len(e.pods()) != 1 {
		t.Fatal("the reaper removed a pod before the margin")
	}
	e.clock.Advance(2 * time.Minute)
	e.m.Reap(context.Background())
	waitFor(t, "removal of the pod that stays in deletion", func() bool { return e.m.pod(name) == nil })
	e.acquire(req).Release()
}

// The start operation must replace a pod that failed.
func TestStartReplacesAFailedPod(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	firstUID := e.m.pod(name).UID
	e.setStatus(name, func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
	waitFor(t, "failed phase in the cache", func() bool { return podTerminal(e.m.pod(name)) })
	if s := e.m.InstanceStatus(req.Ref); s != StatusStopped {
		t.Errorf("status of a failed pod = %s", s)
	}
	if err := e.m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "new pod", func() bool { p := e.m.pod(name); return p != nil && p.UID != firstUID })
}

func TestWaitReady(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	// No sandbox and no start: no wait.
	start := time.Now()
	e.m.WaitReady(context.Background(), req.Ref)
	if time.Since(start) > time.Second {
		t.Error("WaitReady waited for a sandbox that does not start")
	}
	if err := e.m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { e.m.WaitReady(context.Background(), req.Ref); close(done) }()
	select {
	case <-done:
		t.Fatal("WaitReady returned before the sandbox was ready")
	case <-time.After(100 * time.Millisecond):
	}
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	e.makeReady(name)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("WaitReady did not return after the sandbox was ready")
	}
}

// A stop that Kubernetes refuses is an error for the caller.
func TestStopReportsAnError(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	e.acquire(req).Release()
	e.client.PrependReactor("delete", "pods", forbidden)
	if err := e.m.Stop(context.Background(), req.Ref); err == nil {
		t.Error("Stop = nil, want the error of the API")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := e.m.Restart(ctx, req); err == nil || time.Since(start) > time.Second {
		t.Errorf("Restart = %v after %v, want an immediate error", err, time.Since(start))
	}
}

func forbidden(k8stesting.Action) (bool, runtime.Object, error) {
	return true, nil, apierrors.NewForbidden(corev1.Resource("pods"), "x", errors.New("not permitted"))
}

// An API server that does not support user namespaces removes the setting
// hostUsers and accepts the pod. The sandbox would then use the user IDs of
// the node, with no message. Warren must not keep such a pod.
func TestClusterThatIgnoresTheUserNamespace(t *testing.T) {
	e := newTestEnv(t, nil)
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Spec.HostUsers = nil
		return false, nil, nil
	})
	// The policy "other-image" has "userNamespace: true".
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := e.m.Acquire(ctx, templateRef("other-image", "alice"))
	if err == nil || errors.Is(err, backend.ErrNotReady) || !strings.Contains(err.Error(), "user namespace") {
		t.Fatalf("Acquire = %v, want an error that names the user namespace", err)
	}
	waitFor(t, "removal of the pod", func() bool { return len(e.pods()) == 0 })
	if n := len(e.secrets()); n != 0 {
		t.Errorf("secrets = %d, want 0", n)
	}
	if got := e.networkPolicies(); len(got) != 1 {
		t.Errorf("network policies = %d, want only the policy of the namespace", len(got))
	}
	// A policy with no user namespace is not changed by this check.
	e.acquire(userRef("alice")).Release()
}

// Kubernetes replaces "$(NAME)" and "$$" in the items of a command, as in
// the value of a variable. The program must get each item as the policy file
// has it, and the key of the pod must not go onto the command line.
func TestCommandOfAPolicyIsLiteral(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("other-image", "alice")
	pol := e.policy(req)
	pol.Command = []string{"/bin/sh", "-c", `echo $$ > /tmp/pid; echo $(OPEN_TERMINAL_API_KEY) $HOME; exec "$@"`, "sh"}
	got := e.m.buildPod(req.Ref.Name(), "secret", req, pol).Spec.Containers[0].Command
	want := `echo $$$$ > /tmp/pid; echo $$(OPEN_TERMINAL_API_KEY) $$HOME; exec "$$@"`
	if len(got) != 4 || got[0] != "/bin/sh" || got[2] != want {
		t.Errorf("command = %q, want the item %q", got, want)
	}
	if pol.Command[2] == want {
		t.Error("buildPod changed the command of the policy")
	}
}
