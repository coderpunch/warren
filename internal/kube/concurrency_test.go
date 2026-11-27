package kube

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/coderpunch/warren/internal/policy"
)

// A policy with a new scope and a retention time must not delete the volumes
// of the previous scope. docs/policies.md, sections 5 and 11: warren keeps
// them.
func TestRetentionKeepsVolumesOfAPreviousScope(t *testing.T) {
	e := newTestEnv(t, nil)
	chat := chatKeptRef("alice", "c1")
	name := chat.Ref.Name()
	e.acquire(chat).Release()
	e.waitVolume(name)
	e.stop(chat)
	e.m.Reap(context.Background())
	e.waitVolume(name)
	if e.volumeFromAPI(name).Annotations[AnnotationUnusedSince] == "" {
		t.Fatal("the reaper did not mark the volume")
	}

	// The policy had the scope "chat". The new file gives it the scope
	// "user", and it has a retention time.
	e.usePolicies("policies:\n  - {id: chat-kept, name: Now for each user, scope: user, storage: {type: persistent, retention: 24h}}\n")
	if got := e.m.Instances("chat-kept"); len(got) != 0 {
		t.Fatalf("instances = %+v, want none: the volume has no policy", got)
	}
	e.clock.Advance(100 * time.Hour)
	e.m.Reap(context.Background())
	if e.volumeFromAPI(name) == nil {
		t.Error("the reaper deleted a volume of the previous scope of a policy")
	}
}

// A change of the policy file in the middle of a reaper run must not delete
// a volume in the same run: the delete delay starts at the change.
func TestPolicyChangeInTheMiddleOfAReaperRun(t *testing.T) {
	// The reaper reads the volumes in the sequence of a map. Do the test
	// until the run changes a different volume before the marked volume.
	for run := 0; run < 50; run++ {
		e := newTestEnv(t, func(c *Config) { c.VolumeDeleteDelay = 10 * time.Minute })
		// The store uses the time of the machine for a change. Put the clock
		// of the test to that time. Warren then started a long time ago.
		e.clock.Advance(time.Since(e.clock.Now()))
		pvcs := e.client.CoreV1().PersistentVolumeClaims(testNamespace)
		bob := templateRef("kept", "bob")
		old := e.m.buildVolume(bob.Ref.Name(), bob, e.policy(bob))
		// 5 hours of the 24 hours of the policy.
		old.Annotations[AnnotationUnusedSince] = e.clock.Now().Add(-5 * time.Hour).UTC().Format(timeFormat)
		if _, err := pvcs.Create(context.Background(), old, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			r := templateRef("kept", fmt.Sprintf("user-%d", i))
			if _, err := pvcs.Create(context.Background(), e.m.buildVolume(r.Ref.Name(), r, e.policy(r)), metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		waitFor(t, "volumes in the cache", func() bool { return len(e.m.volumes.GetStore().List()) == 6 })

		// The administrator writes a retention time of 1 hour while the
		// reaper marks a volume.
		var once sync.Once
		e.client.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
			once.Do(func() { e.usePolicies(fmt.Sprintf(onePolicy, "1h", "")) })
			return false, nil, nil
		})
		e.m.Reap(context.Background())
		if e.volumeFromAPI(bob.Ref.Name()) == nil {
			t.Fatalf("run %d: the reaper deleted a volume immediately after a change of the policy file, with a delete delay of 10 minutes", run)
		}
	}
}

// A start that makes a volume and then passes its time limit must remove the
// volume. The test uses a real client: the simulated client ignores the
// context of a request.
func TestStartPastItsTimeLimitRemovesItsVolume(t *testing.T) {
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	var mu sync.Mutex
	var calls []string
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		reply := func(code int, body string) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		}
		notFound := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
		switch {
		case r.Method == http.MethodGet:
			reply(http.StatusNotFound, notFound)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/persistentvolumeclaims"):
			reply(http.StatusCreated, `{"kind":"PersistentVolumeClaim","apiVersion":"v1","metadata":{"name":"`+name+`","namespace":"`+testNamespace+`","uid":"volume-1"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/secrets"):
			reply(http.StatusCreated, `{"kind":"Secret","apiVersion":"v1","metadata":{"name":"s","namespace":"`+testNamespace+`","uid":"secret-1"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/networkpolicies"):
			reply(http.StatusCreated, `{"kind":"NetworkPolicy","apiVersion":"networking.k8s.io/v1","metadata":{"name":"s","namespace":"`+testNamespace+`","uid":"policy-1"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pods"):
			// An admission webhook that does not answer.
			select {
			case <-r.Context().Done():
			case <-release:
			}
		case r.Method == http.MethodDelete:
			reply(http.StatusOK, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		}
	}))
	defer srv.Close()
	defer close(release)

	client, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{
		Client:    client,
		Namespace: testNamespace,
		Image:     "example.test/terminal:1",
		Policies:  policy.NewStore(testPolicies(t)),
		Network:   NetworkConfig{WarrenNamespace: "warren-system", WarrenPodLabels: map[string]string{"app": "warren"}},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	pol, _ := m.cfg.Policies.Get("kept")
	// start gives createObjects a context with a time limit of 30 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if created, err := m.createObjects(ctx, name, req, pol); err == nil || created {
		t.Fatalf("createObjects = %v, %v, want the error of the time limit", created, err)
	}
	mu.Lock()
	defer mu.Unlock()
	made, deleted := false, false
	for _, c := range calls {
		made = made || c == "POST /api/v1/namespaces/"+testNamespace+"/persistentvolumeclaims"
		deleted = deleted || c == "DELETE /api/v1/namespaces/"+testNamespace+"/persistentvolumeclaims/"+name
	}
	if !made {
		t.Fatalf("the start did not make the volume: %v", calls)
	}
	if !deleted {
		t.Errorf("the start passed its time limit and sent no delete request for the volume that it made: %v", calls)
	}
	// The Secret and the network policy of the start must also go away.
	deletes := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "DELETE ") && (strings.Contains(c, "/secrets/") || strings.Contains(c, "/networkpolicies/")) {
			deletes++
		}
	}
	if deletes != 2 {
		t.Errorf("delete requests for the Secret and the network policy = %d, want 2: %v", deletes, calls)
	}
}

// A start operation of the management API must start the sandbox when the
// reaper works on the volume at that moment.
func TestManagerStartWaitsForTheReaper(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("kept", "alice")
	name := req.Ref.Name()
	e.acquire(req).Release()
	e.stop(req)
	e.waitVolume(name)

	// The reaper sends the mark of the volume, and the API server is slow.
	inPatch, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.client.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		once.Do(func() {
			close(inPatch)
			<-release
		})
		return false, nil, nil
	})
	reaped := make(chan struct{})
	go func() {
		e.m.Reap(context.Background())
		close(reaped)
	}()
	<-inPatch
	time.AfterFunc(100*time.Millisecond, func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.m.Start(ctx, req); err != nil {
		t.Fatal(err)
	}
	<-reaped
	e.m.mu.Lock()
	st := e.m.sandboxes[name]
	starting := st != nil && (st.starting || st.creating)
	e.m.mu.Unlock()
	if !starting && e.m.pod(name) == nil && len(e.pods()) == 0 {
		t.Error("Start returned no error, and it started nothing")
	}
}
