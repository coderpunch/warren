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

	// creating is true from the start request until the informer shows
	// the pod.
	creating    bool
	createStart time.Time
	owner       string
	// coldStart is true until the first lease after a start.
	coldStart bool

	// deleting is true after warren sent a delete request.
	deleting bool
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
			st.deleting = false
			st.creating = false
			st.key, st.keyUID, st.verified = "", "", ""
			if st.leases == 0 {
				delete(m.sandboxes, pod.Name)
			}
		} else {
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
	st := m.stateLocked(name)
	deleting := st.deleting
	m.mu.Unlock()

	switch {
	case pod == nil:
		return nil, m.start(req)
	case pod.DeletionTimestamp != nil || deleting:
		return nil, nil
	case podTerminal(pod):
		m.deletePod(ctx, pod, "failed")
		return nil, nil
	case !podReady(pod):
		return nil, nil
	}

	target, err := m.target(ctx, pod)
	if errors.Is(err, errBadKey) {
		// A different process made the pod, or the key changed. The pod
		// has no use for warren.
		m.deletePod(ctx, pod, "bad_key")
		return nil, nil
	}
	if err != nil {
		m.cfg.Logger.Debug("sandbox not usable at this time", "sandbox", name, "error", err.Error())
		return nil, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	st = m.stateLocked(name)
	if st.deleting {
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
	})
}

// createStale is the time after which warren does not wait for the pod of
// a start request to show in the cache.
const createStale = 20 * time.Second

// start sends the start request for a sandbox, if no start is in progress.
func (m *Manager) start(req backend.Request) error {
	name := req.Ref.Name()
	owner := ownerDigest(req.UserID)

	m.mu.Lock()
	st := m.stateLocked(name)
	if st.creating && m.now().Sub(st.createStart) < createStale {
		m.mu.Unlock()
		return nil
	}
	ownerCount, total := m.countLocked(owner)
	if (m.cfg.MaxPerUser > 0 && ownerCount >= m.cfg.MaxPerUser) || (m.cfg.MaxTotal > 0 && total >= m.cfg.MaxTotal) {
		m.mu.Unlock()
		m.cfg.Metrics.Create("limit")
		m.cfg.Logger.Warn("sandbox limit", "log", "audit", "user", req.UserID, "sandbox", name,
			"user_sandboxes", ownerCount, "total_sandboxes", total)
		return backend.ErrLimit
	}
	st.creating = true
	st.createStart = m.now()
	st.owner = owner
	st.coldStart = true
	m.mu.Unlock()

	// Do not use the context of the request. If the client goes away in
	// the middle of the start, the objects must be complete.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.createObjects(ctx, name, req); err != nil {
		m.mu.Lock()
		st.creating = false
		st.coldStart = false
		m.mu.Unlock()
		m.cfg.Metrics.Create("error")
		return fmt.Errorf("start sandbox: %w", err)
	}
	m.cfg.Metrics.Create("ok")
	return nil
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

func (m *Manager) createObjects(ctx context.Context, name string, req backend.Request) error {
	pods := m.cfg.Client.CoreV1().Pods(m.cfg.Namespace)
	secrets := m.cfg.Client.CoreV1().Secrets(m.cfg.Namespace)

	// The cache can be late. If the pod exists, do not change its Secret.
	if _, err := pods.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	key, err := newKey()
	if err != nil {
		return err
	}
	// Each start of a pod gets a new key.
	secret := m.buildSecret(name, req, key)
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		if err := secrets.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	pod, err := pods.Create(ctx, m.buildPod(name, req), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// A different process made the pod at the same time. The key check
		// finds if its key is different.
		return nil
	}
	if err != nil {
		_ = secrets.Delete(ctx, name, metav1.DeleteOptions{})
		return err
	}

	// The pod owns its Secret. Kubernetes deletes the Secret with the pod.
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []map[string]any{{
		"apiVersion": "v1", "kind": "Pod", "name": pod.Name, "uid": pod.UID,
	}}}})
	if _, err := secrets.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		m.cfg.Logger.Warn("cannot set the owner of a Secret. The reaper deletes it later.", "sandbox", name, "error", err.Error())
	}

	m.mu.Lock()
	st := m.stateLocked(name)
	st.key, st.keyUID = key, pod.UID
	m.mu.Unlock()

	m.cfg.Logger.Info("sandbox created", "log", "audit", "user", req.UserID, "template", req.Ref.TemplateID,
		"context", req.Ref.ContextID(), "sandbox", name)
	return nil
}

// target returns the address and the key of a ready pod. On the first use
// of a pod, it makes sure that the key operates.
func (m *Manager) target(ctx context.Context, pod *corev1.Pod) (backend.Target, error) {
	m.mu.Lock()
	st := m.stateLocked(pod.Name)
	key := ""
	if st.keyUID == pod.UID {
		key = st.key
	}
	verified := st.verified == pod.UID
	m.mu.Unlock()

	if key == "" {
		// Warren started again, and thus it must read the key.
		secret, err := m.cfg.Client.CoreV1().Secrets(m.cfg.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return backend.Target{}, errBadKey
		}
		if err != nil {
			return backend.Target{}, err
		}
		key = string(secret.Data[secretKeyName])
		if key == "" {
			return backend.Target{}, errBadKey
		}
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
	}
	m.mu.Lock()
	st = m.stateLocked(pod.Name)
	st.key, st.keyUID, st.verified = key, pod.UID, pod.UID
	m.mu.Unlock()
	return target, nil
}

var verifyClient = &http.Client{
	Timeout:   3 * time.Second,
	Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
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

// deletePod deletes a pod and records the reason.
func (m *Manager) deletePod(ctx context.Context, pod *corev1.Pod, reason string) {
	m.mu.Lock()
	st := m.stateLocked(pod.Name)
	if st.deleting {
		m.mu.Unlock()
		return
	}
	st.deleting = true
	m.mu.Unlock()

	// Delete only the pod that warren examined, and not a new pod with the
	// same name.
	err := m.cfg.Client.CoreV1().Pods(m.cfg.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &pod.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		m.mu.Lock()
		st.deleting = false
		m.mu.Unlock()
		m.cfg.Logger.Warn("cannot delete a sandbox", "sandbox", pod.Name, "reason", reason, "error", err.Error())
		return
	}
	m.cfg.Metrics.Reap(reason)
	m.cfg.Logger.Info("sandbox deleted", "log", "audit", "sandbox", pod.Name, "reason", reason,
		"template", pod.Labels[LabelTemplate], "context", pod.Annotations[AnnotationContext])
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

// Start sends the start request for a sandbox. It does not wait.
func (m *Manager) Start(req backend.Request) error {
	if pod := m.pod(req.Ref.Name()); pod != nil && pod.DeletionTimestamp == nil && !podTerminal(pod) {
		return nil
	}
	return m.start(req)
}

// Stop deletes the pod of a sandbox. The data of the sandbox stays.
func (m *Manager) Stop(ctx context.Context, ref scope.Ref) {
	if pod := m.pod(ref.Name()); pod != nil && pod.DeletionTimestamp == nil {
		m.deletePod(ctx, pod, "api")
	}
}

// Restart stops a sandbox, waits until its pod is gone, and starts it again.
func (m *Manager) Restart(ctx context.Context, req backend.Request) error {
	m.Stop(ctx, req.Ref)
	name := req.Ref.Name()
	for m.pod(name) != nil {
		wake := m.waiter()
		if m.pod(name) == nil {
			break
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
	return m.start(req)
}
