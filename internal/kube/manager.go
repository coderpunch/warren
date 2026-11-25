// Package kube is the Kubernetes backend of warren. It starts one pod for
// each sandbox when a request needs it, waits until the pod is ready, and
// stops a pod that had no use for some time.
//
// All state is in Kubernetes: the labels and annotations of the pods, and
// one Secret for each pod. After a restart, warren finds its pods again.
package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/metrics"
	"github.com/h3xcat/warren/internal/scope"
)

const timeFormat = time.RFC3339

// Config configures a Manager.
type Config struct {
	Client kubernetes.Interface
	// Namespace is the namespace for all sandboxes.
	Namespace string
	// Image is the container image of a sandbox.
	Image string
	// IdleTimeout is the time after which warren stops a sandbox that had
	// no use. Zero means that warren never stops an idle sandbox.
	IdleTimeout time.Duration
	// StartTimeout is the maximum time that a pod can be not ready. After
	// that time warren deletes the pod.
	StartTimeout time.Duration
	// MaxPerUser is the maximum number of sandboxes that one user can
	// start. Zero means no limit.
	MaxPerUser int
	// MaxTotal is the maximum number of sandboxes. Zero means no limit.
	MaxTotal int
	// ReapInterval is the time between two runs of the reaper.
	ReapInterval time.Duration

	Logger  *slog.Logger
	Metrics *metrics.Metrics

	// Port is the port of Open Terminal in a pod. Tests change it.
	Port int
	// Verify makes sure that a key operates on a target. Tests change it.
	Verify func(ctx context.Context, target backend.Target) error
	// Now gives the time. Tests change it.
	Now func() time.Time
}

// errBadKey means that a sandbox refused the key that warren has for it.
var errBadKey = errors.New("the sandbox refused its key")

// state is the data that warren keeps in memory for one sandbox.
type state struct {
	// leases is the number of open requests and WebSockets.
	leases int
	// lastActivity is the time of the last start or end of a lease.
	lastActivity time.Time
	// persisted is the activity time that is in the pod annotation.
	persisted time.Time

	// key is the API key of the pod with the UID keyUID.
	key    string
	keyUID types.UID
	// verified is the UID of the pod on which the key operates.
	verified types.UID
	// restarts is the restart count of the container at the key check. A
	// container that started again can have a different key.
	restarts int32

	// creating is true from the start request until the informer shows
	// the pod.
	creating    bool
	createStart time.Time
	owner       string
	// coldStart is true until the first lease after a start.
	coldStart bool

	// starting is true while the start request runs.
	starting bool

	// deleting is the UID of the pod for which warren sent a delete
	// request. It is empty when no delete is in progress.
	deleting types.UID
}

// Manager implements backend.Backend with Kubernetes pods.
type Manager struct {
	cfg     Config
	started time.Time

	informer cache.SharedIndexInformer
	factory  informers.SharedInformerFactory

	mu        sync.Mutex
	sandboxes map[string]*state
	wake      chan struct{}
	// badKeys counts, for each sandbox name, the pods that refused their
	// key. It is not in the state, because the state goes away with a pod.
	badKeys map[string]badKey
}

// badKey is the record of pods of one sandbox that refused their key.
type badKey struct {
	count int
	last  time.Time
}

// maxBadKeys is the number of pods in sequence that can refuse their key
// before warren reports an error to the caller and waits before the next
// start. Warren replaces the first pod immediately.
const maxBadKeys = 2

// backoff returns the time that warren waits before the next start.
func (b badKey) backoff() time.Duration {
	if b.count < maxBadKeys {
		return 0
	}
	d := 5 * time.Second << min(b.count-maxBadKeys, 6)
	return min(d, 5*time.Minute)
}

// New makes a Manager. Call Run to start it.
func New(cfg Config) (*Manager, error) {
	if cfg.Client == nil || cfg.Namespace == "" || cfg.Image == "" {
		return nil, errors.New("kube: client, namespace, and image are necessary")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 10 * time.Minute
	}
	if cfg.ReapInterval <= 0 {
		cfg.ReapInterval = 30 * time.Second
	}
	if cfg.Port == 0 {
		cfg.Port = terminalPort
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	m := &Manager{
		cfg:       cfg,
		started:   cfg.Now(),
		sandboxes: map[string]*state{},
		wake:      make(chan struct{}),
		badKeys:   map[string]badKey{},
	}
	if m.cfg.Verify == nil {
		m.cfg.Verify = verifyKey
	}
	m.factory = informers.NewSharedInformerFactoryWithOptions(cfg.Client, 0,
		informers.WithNamespace(cfg.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = managedSelector }))
	m.informer = m.factory.Core().V1().Pods().Informer()
	if _, err := m.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.podChanged(obj, false) },
		UpdateFunc: func(_, obj any) { m.podChanged(obj, false) },
		DeleteFunc: func(obj any) { m.podChanged(obj, true) },
	}); err != nil {
		return nil, err
	}
	cfg.Metrics.SetSandboxCounts(m.counts)
	return m, nil
}

func (m *Manager) now() time.Time { return m.cfg.Now() }

// Run starts the pod cache and the reaper. It returns when the context ends.
func (m *Manager) Run(ctx context.Context) error {
	m.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return errors.New("kube: the pod cache did not start")
	}
	m.cfg.Logger.Info("sandbox backend ready", "namespace", m.cfg.Namespace, "sandboxes", len(m.informer.GetStore().List()))
	ticker := time.NewTicker(m.cfg.ReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.Reap(ctx)
		}
	}
}

// Ready reports whether the pod cache has the current state of the cluster.
func (m *Manager) Ready() bool { return m.informer.HasSynced() }

// podChanged is the handler for each pod event.
func (m *Manager) podChanged(obj any, deleted bool) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	m.mu.Lock()
	if st := m.sandboxes[pod.Name]; st != nil {
		if deleted {
			if st.deleting == pod.UID {
				st.deleting = ""
			}
			if st.keyUID == pod.UID {
				st.key, st.keyUID = "", ""
			}
			if st.verified == pod.UID {
				st.verified = ""
			}
			m.dropLocked(pod.Name)
		} else if pod.DeletionTimestamp == nil {
			st.creating = false
		}
	}
	// Wake all callers that wait for a pod.
	close(m.wake)
	m.wake = make(chan struct{})
	m.mu.Unlock()
}

func (m *Manager) waiter() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wake
}

// stateLocked returns the state of a sandbox. The caller must hold m.mu.
func (m *Manager) stateLocked(name string) *state {
	st := m.sandboxes[name]
	if st == nil {
		st = &state{}
		m.sandboxes[name] = st
	}
	return st
}

// dropLocked removes the state of a sandbox that has no pod, no lease, and
// no start or delete in progress. The caller must hold m.mu.
func (m *Manager) dropLocked(name string) {
	st := m.sandboxes[name]
	if st != nil && st.leases == 0 && !st.starting && !st.creating && st.deleting == "" && m.pod(name) == nil {
		delete(m.sandboxes, name)
	}
}

func (m *Manager) pod(name string) *corev1.Pod {
	obj, ok, err := m.informer.GetStore().GetByKey(m.cfg.Namespace + "/" + name)
	if err != nil || !ok {
		return nil
	}
	pod, _ := obj.(*corev1.Pod)
	return pod
}

func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
}

// pollInterval is the maximum time between two looks at a sandbox while a
// caller waits. The pod events usually wake the caller before that time.
const pollInterval = time.Second

// Acquire gives a lease on a sandbox. It starts the sandbox if it does not
// exist, and waits until it is ready or until the context ends. When the
// context ends first, the start continues, and a subsequent call can
// succeed.
func (m *Manager) Acquire(ctx context.Context, req backend.Request) (backend.Lease, error) {
	if !m.Ready() {
		// Without the pod cache, warren cannot see or count the sandboxes.
		return nil, backend.ErrNotReady
	}
	name := req.Ref.Name()
	for {
		wake := m.waiter()
		l, err := m.tryAcquire(ctx, name, req)
		if err != nil {
			return nil, err
		}
		if l != nil {
			return l, nil
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, backend.ErrNotReady
			}
			return nil, ctx.Err()
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// tryAcquire does one step. It returns a lease, or nil if the caller must
// wait.
func (m *Manager) tryAcquire(ctx context.Context, name string, req backend.Request) (backend.Lease, error) {
	pod := m.pod(name)
	m.mu.Lock()
	// Read the state, but do not make it. A request that the limit
	// refuses must not leave a record.
	st := m.sandboxes[name]
	deleting := pod != nil && st != nil && st.deleting == pod.UID
	bad := m.badKeys[name]
	m.mu.Unlock()

	switch {
	case pod == nil:
		if bad.count > 0 && m.now().Sub(bad.last) < bad.backoff() {
			// A sandbox that refuses its key again and again must not
			// make warren start and delete pods with no end.
			return nil, fmt.Errorf("the sandbox refused its key %d times; warren waits before the next start", bad.count)
		}
		return nil, m.start(ctx, req, false)
	case pod.Labels[LabelManagedBy] != ManagedByValue:
		// The cache has only the pods of warren. This check is a second
		// protection.
		return nil, fmt.Errorf("a pod with the name %s exists, and warren did not make it", name)
	case pod.DeletionTimestamp != nil || deleting:
		return nil, nil
	case podTerminal(pod):
		_ = m.deletePod(ctx, pod, "failed", false)
		return nil, nil
	case !podReady(pod):
		return nil, nil
	}

	target, err := m.target(ctx, pod)
	if errors.Is(err, errBadKey) {
		// A different process made the pod, or the key changed. The pod
		// has no use for warren.
		m.mu.Lock()
		bad := m.badKeys[name]
		bad.count++
		bad.last = m.now()
		m.badKeys[name] = bad
		m.mu.Unlock()
		_ = m.deletePod(ctx, pod, "bad_key", false)
		if bad.count >= maxBadKeys {
			m.cfg.Logger.Warn("a sandbox refuses its key again. Examine the image of the sandbox.",
				"sandbox", name, "count", bad.count)
			return nil, fmt.Errorf("the sandbox refused its key %d times", bad.count)
		}
		return nil, nil
	}
	if err != nil {
		m.cfg.Logger.Debug("sandbox not usable at this time", "sandbox", name, "error", err.Error())
		return nil, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	st = m.stateLocked(name)
	// The pod can be in deletion, or a different pod, since the first look.
	if cur := m.pod(name); cur == nil || cur.UID != pod.UID || cur.DeletionTimestamp != nil || st.deleting == pod.UID {
		return nil, nil
	}
	st.leases++
	st.lastActivity = m.now()
	if st.coldStart {
		st.coldStart = false
		m.cfg.Metrics.ColdStart(m.now().Sub(st.createStart))
	}
	return &lease{m: m, name: name, target: target}, nil
}

type lease struct {
	m      *Manager
	name   string
	target backend.Target
	once   sync.Once
}

func (l *lease) Target() backend.Target { return l.target }

func (l *lease) Release() {
	l.once.Do(func() {
		l.m.mu.Lock()
		defer l.m.mu.Unlock()
		st := l.m.sandboxes[l.name]
		if st == nil {
			return
		}
		st.leases--
		st.lastActivity = l.m.now()
		l.m.dropLocked(l.name)
	})
}

// createStale is the time after which warren does not wait for the pod of
// a start request to show in the cache.
const createStale = 20 * time.Second

// start sends the start request for a sandbox, if no start is in progress.
// It waits for the result until ctx ends. The start request continues after
// that time.
//
// With exempt, the limit for each user does not apply. A manager who starts
// the sandbox of a chat or a model uses it for other users.
func (m *Manager) start(ctx context.Context, req backend.Request, exempt bool) error {
	name := req.Ref.Name()
	owner := ownerDigest(req.UserID)

	m.mu.Lock()
	st := m.stateLocked(name)
	if st.starting || (st.creating && m.now().Sub(st.createStart) < createStale) {
		m.mu.Unlock()
		return nil
	}
	// The caller read the cache before this lock. If the pod came into the
	// cache after that, the caller must wait for it. Without this check,
	// the pod of this sandbox counts for the limit of its own start.
	if pod := m.pod(name); pod != nil && pod.DeletionTimestamp == nil && !podTerminal(pod) {
		m.dropLocked(name)
		m.mu.Unlock()
		return nil
	}
	ownerCount, total := m.countLocked(owner)
	userLimit := !exempt && m.cfg.MaxPerUser > 0 && ownerCount >= m.cfg.MaxPerUser
	if userLimit || (m.cfg.MaxTotal > 0 && total >= m.cfg.MaxTotal) {
		m.dropLocked(name)
		m.mu.Unlock()
		m.cfg.Metrics.Create("limit")
		m.cfg.Logger.Warn("sandbox limit", "log", "audit", "user", req.UserID, "sandbox", name,
			"user_sandboxes", ownerCount, "total_sandboxes", total)
		if userLimit {
			return backend.ErrLimit
		}
		return backend.ErrCapacity
	}
	st.starting = true
	st.creating = true
	st.createStart = m.now()
	st.owner = owner
	st.coldStart = true
	m.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		// Do not use the context of the request. If the client goes away
		// in the middle of the start, the objects must be complete.
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		created, err := m.createObjects(c, name, req)
		m.mu.Lock()
		st.starting = false
		if err != nil || !created {
			st.creating = false
			st.coldStart = false
			m.dropLocked(name)
		}
		m.mu.Unlock()
		switch {
		case err != nil:
			m.cfg.Metrics.Create("error")
			m.cfg.Logger.Warn("cannot start a sandbox", "sandbox", name, "error", err.Error())
			done <- fmt.Errorf("start sandbox: %w", err)
			return
		case created:
			m.cfg.Metrics.Create("ok")
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return nil
	}
}

// countLocked counts the sandboxes of one owner and all sandboxes. It
// includes the start requests that the cache does not show at this time.
func (m *Manager) countLocked(owner string) (ownerCount, total int) {
	seen := map[string]bool{}
	for _, obj := range m.informer.GetStore().List() {
		pod := obj.(*corev1.Pod)
		if pod.DeletionTimestamp != nil || podTerminal(pod) {
			continue
		}
		seen[pod.Name] = true
		total++
		if pod.Labels[LabelOwner] == owner {
			ownerCount++
		}
	}
	for name, st := range m.sandboxes {
		if st.creating && !seen[name] {
			total++
			if st.owner == owner {
				ownerCount++
			}
		}
	}
	return ownerCount, total
}

// createObjects makes the Secret and the pod. It reports whether it made
// the pod.
func (m *Manager) createObjects(ctx context.Context, name string, req backend.Request) (bool, error) {
	pods := m.cfg.Client.CoreV1().Pods(m.cfg.Namespace)
	secrets := m.cfg.Client.CoreV1().Secrets(m.cfg.Namespace)

	// The cache can be late. If the pod exists, there is nothing to make.
	if p, err := pods.Get(ctx, name, metav1.GetOptions{}); err == nil {
		if p.Labels[LabelManagedBy] != ManagedByValue {
			return false, fmt.Errorf("a pod with the name %s exists, and warren did not make it", name)
		}
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}

	// Each start has a new key in a new Secret.
	key, err := newKey()
	if err != nil {
		return false, err
	}
	secretName, err := newSecretName(name)
	if err != nil {
		return false, err
	}
	made, err := secrets.Create(ctx, m.buildSecret(name, secretName, req, key), metav1.CreateOptions{})
	if err != nil {
		return false, err
	}

	pod, err := pods.Create(ctx, m.buildPod(name, secretName, req), metav1.CreateOptions{})
	if err != nil {
		// Delete only the Secret of this start request.
		_ = secrets.Delete(ctx, secretName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &made.UID}})
		if apierrors.IsAlreadyExists(err) {
			// A different process made the pod at the same time. That
			// pod has the Secret of its own start.
			return false, nil
		}
		return false, err
	}

	// The pod owns its Secret. Kubernetes deletes the Secret with the pod.
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []map[string]any{{
		"apiVersion": "v1", "kind": "Pod", "name": pod.Name, "uid": pod.UID,
	}}}})
	if _, err := secrets.Patch(ctx, secretName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		m.cfg.Logger.Warn("cannot set the owner of a Secret. The reaper deletes it later.", "sandbox", name, "error", err.Error())
	}

	m.mu.Lock()
	st := m.stateLocked(name)
	st.key, st.keyUID = key, pod.UID
	m.mu.Unlock()

	m.cfg.Logger.Info("sandbox created", "log", "audit", "user", req.UserID, "template", req.Ref.TemplateID,
		"context", req.Ref.ContextID(), "sandbox", name)
	return true, nil
}

// target returns the address and the key of a ready pod. On the first use
// of a pod, it makes sure that the key operates.
func (m *Manager) target(ctx context.Context, pod *corev1.Pod) (backend.Target, error) {
	restarts := containerRestarts(pod)
	m.mu.Lock()
	st := m.stateLocked(pod.Name)
	key := ""
	if st.keyUID == pod.UID {
		key = st.key
	}
	verified := st.verified == pod.UID && st.restarts == restarts
	m.mu.Unlock()

	if key == "" || !verified {
		// Warren started again, or the container started again. Read the
		// key from the Secret of the pod.
		secretName := podSecretName(pod)
		if secretName == "" {
			return backend.Target{}, errBadKey
		}
		secret, err := m.cfg.Client.CoreV1().Secrets(m.cfg.Namespace).Get(ctx, secretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return backend.Target{}, errBadKey
		}
		if err != nil {
			return backend.Target{}, err
		}
		for _, o := range secret.OwnerReferences {
			if o.Kind == "Pod" && o.UID != pod.UID {
				return backend.Target{}, errBadKey
			}
		}
		key = string(secret.Data[secretKeyName])
		if key == "" {
			return backend.Target{}, errBadKey
		}
		// Keep the key now. If the key check fails for a short time, the
		// subsequent attempts must not read the Secret again.
		m.mu.Lock()
		st = m.stateLocked(pod.Name)
		st.key, st.keyUID = key, pod.UID
		m.mu.Unlock()
	}
	target := backend.Target{
		URL:    &url.URL{Scheme: "http", Host: net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(m.cfg.Port))},
		APIKey: key,
	}
	if !verified {
		// The health route needs no key. A pod with a wrong key is thus
		// "ready" for Kubernetes (docs/open-terminal-api.md, section 4.1).
		if err := m.cfg.Verify(ctx, target); err != nil {
			return backend.Target{}, err
		}
		// The check took time. Make sure that the pod is the same pod.
		if cur := m.pod(pod.Name); cur == nil || cur.UID != pod.UID || cur.DeletionTimestamp != nil ||
			cur.Status.PodIP != pod.Status.PodIP {
			return backend.Target{}, errors.New("the pod changed during the key check")
		}
	}
	m.mu.Lock()
	st = m.stateLocked(pod.Name)
	st.key, st.keyUID, st.verified, st.restarts = key, pod.UID, pod.UID, restarts
	delete(m.badKeys, pod.Name)
	m.mu.Unlock()
	return target, nil
}

var verifyClient = &http.Client{
	Timeout:   3 * time.Second,
	Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: 64 << 10},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// verifyKey sends one request that needs the key.
func verifyKey(ctx context.Context, target backend.Target) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL.String()+"/files/cwd", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+target.APIKey)
	resp, err := verifyClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return errBadKey
	}
	return fmt.Errorf("key check: status %d", resp.StatusCode)
}

// deletePod deletes a pod and records the reason. With idleOnly, it does
// nothing if the sandbox has a lease.
func (m *Manager) deletePod(ctx context.Context, pod *corev1.Pod, reason string, idleOnly bool) error {
	m.mu.Lock()
	st := m.stateLocked(pod.Name)
	if st.deleting == pod.UID || (idleOnly && st.leases > 0) {
		m.mu.Unlock()
		return nil
	}
	st.deleting = pod.UID
	m.mu.Unlock()

	// Delete only the pod that warren examined, and not a new pod with the
	// same name.
	err := m.cfg.Client.CoreV1().Pods(m.cfg.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &pod.UID},
	})
	if err != nil {
		// No delete event comes for this request.
		m.mu.Lock()
		if st := m.sandboxes[pod.Name]; st != nil && st.deleting == pod.UID {
			st.deleting = ""
			m.dropLocked(pod.Name)
		}
		m.mu.Unlock()
		if apierrors.IsNotFound(err) {
			return nil
		}
		m.cfg.Logger.Warn("cannot delete a sandbox", "sandbox", pod.Name, "reason", reason, "error", err.Error())
		return err
	}
	m.cfg.Metrics.Reap(reason)
	m.cfg.Logger.Info("sandbox deleted", "log", "audit", "sandbox", pod.Name, "reason", reason,
		"template", pod.Labels[LabelTemplate], "context", pod.Annotations[AnnotationContext])
	return nil
}

// counts returns the number of ready sandboxes and of sandboxes that start.
func (m *Manager) counts() (ready, starting int) {
	for _, obj := range m.informer.GetStore().List() {
		pod := obj.(*corev1.Pod)
		switch {
		case pod.DeletionTimestamp != nil || podTerminal(pod):
		case podReady(pod):
			ready++
		default:
			starting++
		}
	}
	return ready, starting
}

// Instance is the data of one sandbox for the management API.
type Instance struct {
	ID        string
	ContextID string
	Status    string
	CreatedBy string
}

// Status values of an instance. Open WebUI shows "running" as in operation
// and all other values as stopped (docs/protocol.md, section 4.6).
const (
	StatusRunning  = "running"
	StatusStarting = "starting"
	StatusStopped  = "stopped"
)

func podStatus(pod *corev1.Pod) string {
	switch {
	case pod == nil || pod.DeletionTimestamp != nil || podTerminal(pod):
		return StatusStopped
	case podReady(pod):
		return StatusRunning
	}
	return StatusStarting
}

// InstanceStatus returns the status of one sandbox.
func (m *Manager) InstanceStatus(ref scope.Ref) string {
	return podStatus(m.pod(ref.Name()))
}

// Instances returns the sandboxes of one template.
func (m *Manager) Instances(templateID string) []Instance {
	var out []Instance
	for _, obj := range m.informer.GetStore().List() {
		pod := obj.(*corev1.Pod)
		if pod.Labels[LabelTemplate] != templateID || pod.DeletionTimestamp != nil {
			continue
		}
		out = append(out, Instance{
			ID:        pod.Name,
			ContextID: pod.Annotations[AnnotationContext],
			Status:    podStatus(pod),
			CreatedBy: pod.Annotations[AnnotationCreatedBy],
		})
	}
	return out
}

// Start sends the start request for a sandbox, for the management API. It
// does not wait until the sandbox is ready. If the previous pod is failed
// or in deletion, it waits until that pod is gone.
func (m *Manager) Start(ctx context.Context, req backend.Request) error {
	if !m.Ready() {
		return backend.ErrNotReady
	}
	name := req.Ref.Name()
	pod := m.pod(name)
	if pod != nil && pod.DeletionTimestamp == nil && !podTerminal(pod) {
		return nil
	}
	if pod != nil {
		if pod.DeletionTimestamp == nil {
			if err := m.deletePod(ctx, pod, "failed", false); err != nil {
				return fmt.Errorf("stop sandbox: %w", err)
			}
		}
		if err := m.waitGone(ctx, name, pod.UID); err != nil {
			return err
		}
		if m.pod(name) != nil {
			return nil
		}
	}
	// A person who can call the management API manages the template. The
	// sandbox of a chat or a model is for other users, thus the limit for
	// each user does not apply to this start.
	return m.start(ctx, req, true)
}

// Stop deletes the pod of a sandbox. The data of the sandbox stays.
func (m *Manager) Stop(ctx context.Context, ref scope.Ref) error {
	if !m.Ready() {
		return backend.ErrNotReady
	}
	if pod := m.pod(ref.Name()); pod != nil && pod.DeletionTimestamp == nil {
		return m.deletePod(ctx, pod, "api", false)
	}
	return nil
}

// waitGone waits until the pod with this UID is not in the cache.
func (m *Manager) waitGone(ctx context.Context, name string, uid types.UID) error {
	for {
		wake := m.waiter()
		if p := m.pod(name); p == nil || p.UID != uid {
			return nil
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Restart stops a sandbox, waits until its pod is gone, and starts it again.
// The sandbox stays on the account of the user that started it.
func (m *Manager) Restart(ctx context.Context, req backend.Request) error {
	if !m.Ready() {
		return backend.ErrNotReady
	}
	name := req.Ref.Name()
	if pod := m.pod(name); pod != nil {
		// For the sandbox of a chat, a model, or a template, Open WebUI
		// sends the ID of the manager (docs/protocol.md, section 4.6).
		if by := pod.Annotations[AnnotationCreatedBy]; by != "" {
			req.UserID = by
		}
		if pod.DeletionTimestamp == nil {
			if err := m.deletePod(ctx, pod, "api", false); err != nil {
				return fmt.Errorf("stop sandbox: %w", err)
			}
		}
		if err := m.waitGone(ctx, name, pod.UID); err != nil {
			return err
		}
	}
	return m.Start(ctx, req)
}

// WaitReady waits until a sandbox is ready. It returns earlier when the
// sandbox has no pod and no start request, when its pod failed, or when the
// context ends.
func (m *Manager) WaitReady(ctx context.Context, ref scope.Ref) {
	name := ref.Name()
	for {
		wake := m.waiter()
		pod := m.pod(name)
		m.mu.Lock()
		st := m.sandboxes[name]
		starting := st != nil && (st.starting || st.creating)
		m.mu.Unlock()
		if (pod == nil && !starting) || (pod != nil && (podReady(pod) || podTerminal(pod))) {
			return
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}
