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
	"sort"
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
	"github.com/h3xcat/warren/internal/policy"
	"github.com/h3xcat/warren/internal/scope"
)

const timeFormat = time.RFC3339

// Config configures a Manager.
type Config struct {
	Client kubernetes.Interface
	// Namespace is the namespace for all sandboxes.
	Namespace string
	// Image is the container image of a sandbox, for a policy that sets no
	// image.
	Image string
	// Policies gives the settings of a sandbox for its template ID.
	Policies Policies
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
	// MaxVolumesPerUser is the maximum number of volumes of the chat scope
	// and the model scope that the starts of one user can make. Zero means
	// no limit. The other scopes have one volume for each user or policy.
	MaxVolumesPerUser int
	// MaxVolumes is the maximum number of volumes. Zero means no limit.
	MaxVolumes int
	// VolumeDeleteDelay is the time after the start of warren, and after a
	// change of the policies, in which the reaper deletes no volume. In
	// that time an administrator can correct a retention time that is
	// wrong.
	VolumeDeleteDelay time.Duration
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

// Policies gives the policy for a template ID.
type Policies interface {
	Get(id string) (policy.Policy, bool)
}

// errWait means that a sandbox cannot start at this time. The caller must
// wait and try again.
var errWait = errors.New("the sandbox cannot start at this time")

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

	// volumePending is true from a start request that makes a volume until
	// the cache shows the volume. volumeScope is the scope of that start.
	volumePending bool
	volumeScope   scope.Kind
	// unschedulable is the UID of the pod for which warren wrote a log
	// record, because Kubernetes found no node for it. stuck is the UID of
	// the pod for which warren wrote a log record, because the pod stays
	// in deletion.
	unschedulable types.UID
	stuck         types.UID
	// resetting is true while a reset operation runs. No start and no
	// lease is possible in that time.
	resetting bool
	// volumeBusy is true while the reaper changes or deletes the volume.
	// No start is possible in that time.
	volumeBusy bool
	// kept has the labels of the volume that a reset replaces. It is not nil
	// from the delete request of a reset until the reset ends. The sandbox
	// keeps its place in the volume limits in that time.
	kept map[string]string
}

// Manager implements backend.Backend with Kubernetes pods.
type Manager struct {
	cfg     Config
	started time.Time

	informer cache.SharedIndexInformer
	// volumes is the cache of the volumes of warren.
	volumes cache.SharedIndexInformer
	factory informers.SharedInformerFactory

	mu        sync.Mutex
	sandboxes map[string]*state
	wake      chan struct{}
	// badKeys counts, for each sandbox name, the pods that refused their
	// key. It is not in the state, because the state goes away with a pod.
	badKeys map[string]badKey
	// volumesReported is the policy time for which warren wrote the log
	// record about the volumes.
	volumesReported time.Time
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
	if cfg.Client == nil || cfg.Namespace == "" || cfg.Image == "" || cfg.Policies == nil {
		return nil, errors.New("kube: client, namespace, image, and policies are necessary")
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
	m.volumes = m.factory.Core().V1().PersistentVolumeClaims().Informer()
	if _, err := m.volumes.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.volumeChanged(obj) },
		UpdateFunc: func(_, obj any) { m.volumeChanged(obj) },
		DeleteFunc: func(obj any) { m.volumeChanged(obj) },
	}); err != nil {
		return nil, err
	}
	cfg.Metrics.SetSandboxCounts(m.counts)
	cfg.Metrics.SetVolumeCount(func() int { return len(m.volumes.GetStore().List()) })
	return m, nil
}

func (m *Manager) now() time.Time { return m.cfg.Now() }

// policiesSince returns the time from which the current policies apply: the
// start of warren, or the last change of the policy file.
func (m *Manager) policiesSince() time.Time {
	since := m.started
	if p, ok := m.cfg.Policies.(interface{ ChangedAt() time.Time }); ok {
		if t := p.ChangedAt(); t.After(since) {
			since = t
		}
	}
	return since
}

// Run starts the pod cache and the reaper. It returns when the context ends.
func (m *Manager) Run(ctx context.Context) error {
	m.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced, m.volumes.HasSynced) {
		return errors.New("kube: the cache of the pods and the volumes did not start")
	}
	m.cfg.Logger.Info("sandbox backend ready", "namespace", m.cfg.Namespace,
		"sandboxes", len(m.informer.GetStore().List()), "volumes", len(m.volumes.GetStore().List()))
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

// Ready reports whether the cache has the current state of the cluster.
func (m *Manager) Ready() bool { return m.informer.HasSynced() && m.volumes.HasSynced() }

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
	m.wakeLocked()
	m.mu.Unlock()
}

// wakeLocked wakes all callers that wait for a pod or a volume. The caller
// must hold m.mu.
func (m *Manager) wakeLocked() {
	close(m.wake)
	m.wake = make(chan struct{})
}

// volumeChanged is the handler for each volume event.
func (m *Manager) volumeChanged(obj any) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok {
		return
	}
	m.mu.Lock()
	if st := m.sandboxes[pvc.Name]; st != nil {
		// The cache now counts the volume.
		st.volumePending = false
	}
	m.wakeLocked()
	m.mu.Unlock()
}

// volumeGone reports whether the volume of a pod is in deletion.
func (m *Manager) volumeGone(pod *corev1.Pod) bool {
	if !usesVolume(pod) {
		return false
	}
	v := m.volume(pod.Name)
	return v != nil && v.DeletionTimestamp != nil
}

// volume returns the volume of a sandbox from the cache.
func (m *Manager) volume(name string) *corev1.PersistentVolumeClaim {
	obj, ok, err := m.volumes.GetStore().GetByKey(m.cfg.Namespace + "/" + name)
	if err != nil || !ok {
		return nil
	}
	pvc, _ := obj.(*corev1.PersistentVolumeClaim)
	return pvc
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
// no operation in progress. The caller must hold m.mu.
func (m *Manager) dropLocked(name string) {
	st := m.sandboxes[name]
	if st != nil && st.leases == 0 && !st.starting && !st.creating && st.deleting == "" &&
		!st.resetting && !st.volumeBusy && m.pod(name) == nil {
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
	resetting := st != nil && st.resetting
	bad := m.badKeys[name]
	m.mu.Unlock()

	switch {
	case resetting:
		// A reset deletes the pod and the volume. Wait for its end.
		return nil, nil
	case pod == nil:
		if bad.count > 0 && m.now().Sub(bad.last) < bad.backoff() {
			// A sandbox that refuses its key again and again must not
			// make warren start and delete pods with no end.
			return nil, fmt.Errorf("the sandbox refused its key %d times; warren waits before the next start", bad.count)
		}
		if err := m.start(ctx, req, false); !errors.Is(err, errWait) {
			return nil, err
		}
		return nil, nil
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
	case m.volumeGone(pod):
		// The files of this pod go away with its volume. The reaper deletes
		// the pod, and the next start makes a new volume.
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
	if cur := m.pod(name); cur == nil || cur.UID != pod.UID || cur.DeletionTimestamp != nil || st.deleting == pod.UID || st.resetting {
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
	pol, ok := m.cfg.Policies.Get(req.Ref.TemplateID)
	if !ok {
		return fmt.Errorf("no policy has the ID %q", req.Ref.TemplateID)
	}

	m.mu.Lock()
	st := m.stateLocked(name)
	if st.starting || (st.creating && m.now().Sub(st.createStart) < createStale) {
		m.mu.Unlock()
		return nil
	}
	// A reset or the reaper works on the volume. The caller must wait.
	if st.resetting || st.volumeBusy {
		m.mu.Unlock()
		return errWait
	}
	// The volume is in deletion. The event of its end wakes the caller.
	old := m.volume(name)
	if pol.Persistent() && old != nil && old.DeletionTimestamp != nil {
		m.dropLocked(name)
		m.mu.Unlock()
		return errWait
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
	newVolume := pol.Persistent() && old == nil
	if newVolume {
		ownerVolumes, volumes := m.volumeCountLocked(owner)
		// The limit for each user is for the scopes in which one user can
		// have very many sandboxes. A person who manages the template is
		// exempt, as for the pod limit. The total limit has no exemption.
		userLimit := !exempt && unbounded(req.Ref.Kind) && m.cfg.MaxVolumesPerUser > 0 && ownerVolumes >= m.cfg.MaxVolumesPerUser
		if userLimit || (m.cfg.MaxVolumes > 0 && volumes >= m.cfg.MaxVolumes) {
			m.dropLocked(name)
			m.mu.Unlock()
			m.cfg.Metrics.Create("volume_limit")
			m.cfg.Logger.Warn("volume limit", "log", "audit", "user", req.UserID, "sandbox", name,
				"user_volumes", ownerVolumes, "total_volumes", volumes)
			if userLimit {
				return backend.ErrVolumeLimit
			}
			return backend.ErrCapacity
		}
	}
	st.volumePending = newVolume
	st.volumeScope = req.Ref.Kind
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
		created, err := m.createObjects(c, name, req, pol)
		m.mu.Lock()
		st.starting = false
		if err != nil || !created {
			st.creating = false
			st.coldStart = false
			// The start can have made the volume. The cache then counts it
			// in a short time.
			st.volumePending = false
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

// unbounded reports whether one user can have very many sandboxes of a
// scope: a user makes a new chat at each time, and Open WebUI does not
// examine each model ID (docs/protocol.md, section 6).
func unbounded(kind scope.Kind) bool {
	return kind == scope.KindChat || kind == scope.KindModel
}

// orphan reports whether an object has no policy at this time: its policy
// is not in the file, or the policy has a different scope. A sandbox name
// includes the scope, thus no request can get to such an object.
func (m *Manager) orphan(labels map[string]string) bool {
	pol, ok := m.cfg.Policies.Get(labels[LabelTemplate])
	return !ok || string(pol.Scope) != labels[LabelScope]
}

// volumeCountLocked counts the volumes of the chat scope and the model
// scope of one owner, and all volumes. A volume with no policy counts only
// for the total: its user cannot use it and cannot give it back. It includes the start requests whose
// volume the cache does not show at this time.
func (m *Manager) volumeCountLocked(owner string) (ownerCount, total int) {
	seen, live := map[string]bool{}, map[string]bool{}
	for _, obj := range m.volumes.GetStore().List() {
		pvc := obj.(*corev1.PersistentVolumeClaim)
		seen[pvc.Name] = true
		if pvc.DeletionTimestamp != nil {
			continue
		}
		live[pvc.Name] = true
		total++
		if pvc.Labels[LabelOwner] == owner && unbounded(scope.Kind(pvc.Labels[LabelScope])) && !m.orphan(pvc.Labels) {
			ownerCount++
		}
	}
	for name, st := range m.sandboxes {
		// A reset replaces the volume of this sandbox. The old volume is in
		// deletion or gone, and the cache does not show the new volume.
		if st.kept != nil && !live[name] {
			total++
			if st.kept[LabelOwner] == owner && unbounded(scope.Kind(st.kept[LabelScope])) && !m.orphan(st.kept) {
				ownerCount++
			}
		}
		if st.volumePending && !seen[name] {
			total++
			if st.owner == owner && unbounded(st.volumeScope) {
				ownerCount++
			}
		}
	}
	return ownerCount, total
}

// ensureVolume makes the volume of a sandbox, if it does not exist. It
// reports false if the volume is in deletion. The caller must then try
// again later. If it made the volume, it returns the UID of the volume.
func (m *Manager) ensureVolume(ctx context.Context, name string, req backend.Request, pol policy.Policy) (bool, types.UID, error) {
	pvcs := m.cfg.Client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace)
	// Read the volume from the API server. The cache can be late, and a
	// sandbox must not start on a volume that the reaper marked as unused.
	cur, err := pvcs.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		var made *corev1.PersistentVolumeClaim
		made, err = pvcs.Create(ctx, m.buildVolume(name, req, pol), metav1.CreateOptions{})
		if err == nil {
			// All requests share this quantity, and String writes to it.
			size := pol.Storage.Size.DeepCopy()
			m.cfg.Logger.Info("volume created", "log", "audit", "user", req.UserID, "template", req.Ref.TemplateID,
				"context", req.Ref.ContextID(), "sandbox", name, "size", size.String())
			return true, made.UID, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return false, "", fmt.Errorf("make the volume: %w", err)
		}
		cur, err = pvcs.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return false, "", fmt.Errorf("read the volume: %w", err)
	}
	if cur.Labels[LabelManagedBy] != ManagedByValue {
		return false, "", fmt.Errorf("a volume with the name %s exists, and warren did not make it", name)
	}
	if cur.DeletionTimestamp != nil {
		return false, "", nil
	}
	if cur.Spec.VolumeName == "" && !volumeMatches(cur, pol) {
		// The volume never got storage, thus it has no data, and Kubernetes
		// does not let a claim change its class. Without this delete, a
		// corrected policy does not repair a sandbox whose first volume
		// could not bind. The conditions keep a volume that got storage, or
		// that a different process replaced, after the read.
		err := pvcs.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
			UID: &cur.UID, ResourceVersion: &cur.ResourceVersion,
		}})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return false, "", fmt.Errorf("delete the volume that never got storage: %w", err)
		}
		if err == nil {
			m.cfg.Metrics.VolumeDelete("unbound")
			m.cfg.Logger.Info("volume deleted", "log", "audit", "sandbox", name, "reason", "unbound",
				"template", cur.Labels[LabelTemplate], "context", cur.Annotations[AnnotationContext])
			return m.replaceVolume(ctx, name, req, pol, cur)
		}
		// The caller tries again and makes the volume of the current policy.
		return false, "", nil
	}
	if _, marked := cur.Annotations[AnnotationUnusedSince]; marked {
		// The volume is in use again. The resource version makes the change
		// conditional: it fails if a different process changed or deleted
		// the volume after the read. The caller then tries again.
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
			"resourceVersion": cur.ResourceVersion,
			"annotations":     map[string]any{AnnotationUnusedSince: nil},
		}})
		patched, err := pvcs.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return false, "", nil
		}
		if err != nil {
			return false, "", fmt.Errorf("mark the volume as used: %w", err)
		}
		if patched.DeletionTimestamp != nil {
			return false, "", nil
		}
	}
	return true, "", nil
}

// replaceVolume makes the volume of the current policy in place of a volume
// that never got storage, after the delete request for that volume. The new
// volume stays on the account of the same user: the old volume can be the
// record of a sandbox after a reset. The function returns no UID, thus a
// start that fails does not remove the new volume.
func (m *Manager) replaceVolume(ctx context.Context, name string, req backend.Request, pol policy.Policy, old *corev1.PersistentVolumeClaim) (bool, types.UID, error) {
	pvcs := m.cfg.Client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace)
	req.UserID = old.Annotations[AnnotationCreatedBy]
	for {
		wake := m.waiter()
		_, err := pvcs.Create(ctx, m.buildVolume(name, req, pol), metav1.CreateOptions{})
		if err == nil {
			size := pol.Storage.Size.DeepCopy()
			m.cfg.Logger.Info("volume created", "log", "audit", "user", req.UserID, "template", req.Ref.TemplateID,
				"context", req.Ref.ContextID(), "sandbox", name, "size", size.String(), "reason", "unbound")
			return true, "", nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return false, "", fmt.Errorf("make the volume: %w", err)
		}
		// The old volume has the name until Kubernetes removes it.
		if cur, err := pvcs.Get(ctx, name, metav1.GetOptions{}); err == nil && cur.UID != old.UID {
			// A different process made a volume. The caller tries again.
			return false, "", nil
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, "", nil
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// volumeMatches reports whether a volume has the storage class and the size
// of a policy. A policy with no class uses the default class of the cluster,
// and Kubernetes writes that class to the volume. Thus a policy with no class
// matches each class.
func volumeMatches(pvc *corev1.PersistentVolumeClaim, pol policy.Policy) bool {
	if want := pol.Storage.StorageClassName; want != "" && (pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != want) {
		return false
	}
	have, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	return pol.Storage.Size == nil || (ok && have.Cmp(*pol.Storage.Size) == 0)
}

// createObjects makes the volume, the Secret, and the pod. It reports
// whether it made the pod.
func (m *Manager) createObjects(ctx context.Context, name string, req backend.Request, pol policy.Policy) (bool, error) {
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

	// newVolume is the UID of a volume that this start made.
	var newVolume types.UID
	if pol.Persistent() {
		ok, uid, err := m.ensureVolume(ctx, name, req, pol)
		if err != nil || !ok {
			return false, err
		}
		newVolume = uid
	}
	// removeNewVolume deletes the volume of this start when the start does
	// not make the pod. The volume is empty. If it stayed, it would count
	// for the limits, and it would show as a stopped sandbox.
	removeNewVolume := func() {
		if newVolume == "" {
			return
		}
		// The context of the start can be at its end. That is a usual cause
		// of the failure.
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		pvcs := m.cfg.Client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace)
		err := pvcs.Delete(c, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &newVolume}})
		switch {
		case err == nil:
			m.cfg.Metrics.VolumeDelete("start_failed")
			m.cfg.Logger.Info("volume deleted", "log", "audit", "sandbox", name, "reason", "start_failed",
				"user", req.UserID, "template", req.Ref.TemplateID, "context", req.Ref.ContextID())
		case !apierrors.IsNotFound(err):
			m.cfg.Logger.Warn("cannot delete the empty volume of a start that failed", "sandbox", name, "error", err.Error())
		}
	}

	// Each start has a new key in a new Secret.
	key, err := newKey()
	if err != nil {
		removeNewVolume()
		return false, err
	}
	secretName, err := newSecretName(name)
	if err != nil {
		removeNewVolume()
		return false, err
	}
	made, err := secrets.Create(ctx, m.buildSecret(name, secretName, req, key), metav1.CreateOptions{})
	if err != nil {
		removeNewVolume()
		return false, err
	}

	pod, err := pods.Create(ctx, m.buildPod(name, secretName, req, pol), metav1.CreateOptions{})
	if err != nil {
		// Delete only the Secret of this start request.
		_ = secrets.Delete(ctx, secretName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &made.UID}})
		if apierrors.IsAlreadyExists(err) {
			// A different process made the pod at the same time. That
			// pod has the Secret of its own start, and it uses the volume.
			return false, nil
		}
		removeNewVolume()
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

// Instances returns the sandboxes of one template. A sandbox that has a
// volume and no pod is a stopped sandbox.
func (m *Manager) Instances(templateID string) []Instance {
	var out []Instance
	seen := map[string]bool{}
	for _, obj := range m.informer.GetStore().List() {
		pod := obj.(*corev1.Pod)
		if pod.Labels[LabelTemplate] != templateID || pod.DeletionTimestamp != nil || m.orphan(pod.Labels) {
			continue
		}
		seen[pod.Name] = true
		out = append(out, Instance{
			ID:        pod.Name,
			ContextID: pod.Annotations[AnnotationContext],
			Status:    podStatus(pod),
			CreatedBy: pod.Annotations[AnnotationCreatedBy],
		})
	}
	for _, obj := range m.volumes.GetStore().List() {
		pvc := obj.(*corev1.PersistentVolumeClaim)
		// An object of a previous scope of the policy is not in the list.
		// Open WebUI would send an operation for it to a different sandbox.
		if pvc.Labels[LabelTemplate] != templateID || pvc.DeletionTimestamp != nil || seen[pvc.Name] || m.orphan(pvc.Labels) {
			continue
		}
		out = append(out, Instance{
			ID:        pvc.Name,
			ContextID: pvc.Annotations[AnnotationContext],
			Status:    StatusStopped,
			CreatedBy: pvc.Annotations[AnnotationCreatedBy],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Start sends the start request for a sandbox, for the management API. It
// does not wait until the sandbox is ready. If the previous pod is failed
// or in deletion, it waits until that pod is gone.
func (m *Manager) Start(ctx context.Context, req backend.Request) error {
	return m.startBy(ctx, req, "")
}

// startBy is Start for a caller that knows the user that started the
// sandbox before. An empty value means that the caller does not know it.
func (m *Manager) startBy(ctx context.Context, req backend.Request, by string) error {
	if !m.Ready() {
		return backend.ErrNotReady
	}
	name := req.Ref.Name()
	pod := m.pod(name)
	if pod != nil && pod.DeletionTimestamp == nil && !podTerminal(pod) {
		return nil
	}
	// The sandbox stays on the account of the user that started it first.
	// The previous pod has that user. If there is no pod, the volume has it.
	if pod != nil {
		if v := pod.Annotations[AnnotationCreatedBy]; v != "" {
			by = v
		}
	} else if v := m.volume(name); v != nil {
		if v := v.Annotations[AnnotationCreatedBy]; v != "" {
			by = v
		}
	}
	switch {
	case by != "":
		req.UserID = by
	case req.Ref.Kind != scope.KindUser:
		// No user started this sandbox. For a chat, a model, or a shared
		// sandbox, Open WebUI does not examine the user ID of the body: a
		// manager can send the ID of each user (docs/protocol.md, section
		// 4.6). The sandbox goes on the account of no user. Without this,
		// the pods and the volumes that a manager makes use up the limits
		// of that user. A request of a user always has a user ID, thus the
		// empty ID is the account of no user.
		req.UserID = ""
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
	for {
		wake := m.waiter()
		err := m.start(ctx, req, true)
		if !errors.Is(err, errWait) {
			return err
		}
		// The reaper or a reset works on the volume, or the volume is in
		// deletion. Wait for the end of that work.
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
	by := ""
	if pod := m.pod(name); pod != nil {
		// For the sandbox of a chat, a model, or a template, Open WebUI
		// sends the ID of the manager (docs/protocol.md, section 4.6).
		by = pod.Annotations[AnnotationCreatedBy]
		if pod.DeletionTimestamp == nil {
			if err := m.deletePod(ctx, pod, "api", false); err != nil {
				return fmt.Errorf("stop sandbox: %w", err)
			}
		}
		if err := m.waitGone(ctx, name, pod.UID); err != nil {
			return err
		}
	}
	return m.startBy(ctx, req, by)
}

// Delete stops a sandbox and deletes its volume. The sandbox is then not in
// the instance list. The next request starts it again with an empty home
// directory. Delete is the operation that gives a volume back.
func (m *Manager) Delete(ctx context.Context, ref scope.Ref) error {
	return m.clear(ctx, ref, false)
}

// Reset stops a sandbox and clears its files. It does not start the sandbox
// again. The sandbox stays in the instance list as a stopped sandbox: warren
// gives it a new, empty volume. Warren has no database, thus the volume is
// the record of a sandbox that has no pod.
func (m *Manager) Reset(ctx context.Context, ref scope.Ref) error {
	return m.clear(ctx, ref, true)
}

// clear removes the pod and the volume of a sandbox. With keep, it then
// makes a new volume for a sandbox that had one.
func (m *Manager) clear(ctx context.Context, ref scope.Ref, keep bool) error {
	if !m.Ready() {
		return backend.ErrNotReady
	}
	name := ref.Name()
	m.mu.Lock()
	st := m.stateLocked(name)
	if st.resetting {
		m.mu.Unlock()
		return errors.New("a reset of this sandbox is in progress")
	}
	// From here, no request can start the sandbox or get a lease on it, and
	// the reaper does not change its volume.
	st.resetting = true
	m.mu.Unlock()

	old, err := m.remove(ctx, name, keep)
	if err == nil && keep && old != nil {
		err = m.emptyVolume(ctx, ref, old)
	}

	m.mu.Lock()
	st.resetting = false
	st.kept = nil
	m.dropLocked(name)
	m.wakeLocked()
	m.mu.Unlock()
	return err
}

// resetWait is the time for which a reset continues to wait for the end of
// the old volume after the end of its request. It is also the maximum time
// in which the reset makes the new volume.
const resetWait = 30 * time.Second

// emptyVolume makes a new volume in place of the volume that a reset
// deleted, and waits until the cache shows it. The new volume stays on the
// account of the same user. If the policy of the sandbox keeps no files at
// this time, there is nothing to make.
func (m *Manager) emptyVolume(ctx context.Context, ref scope.Ref, old *corev1.PersistentVolumeClaim) error {
	pol, ok := m.cfg.Policies.Get(ref.TemplateID)
	if !ok || !pol.Persistent() || pol.Scope != ref.Kind {
		return nil
	}
	// Do not use the context of the request. The old volume is gone. If the
	// client goes away now, the sandbox must not stay with no volume: it
	// would not be in the instance list.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resetWait)
	defer cancel()
	wait := func(wake <-chan struct{}) bool {
		timer := time.NewTimer(pollInterval)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-wake:
		case <-timer.C:
		}
		return true
	}
	name := ref.Name()
	req := backend.Request{Ref: ref, UserID: old.Annotations[AnnotationCreatedBy]}
	pvcs := m.cfg.Client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace)
	for {
		_, err := pvcs.Create(ctx, m.buildVolume(name, req, pol), metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			// A different process made the volume. It is empty.
			break
		}
		if err == nil {
			size := pol.Storage.Size.DeepCopy()
			m.cfg.Logger.Info("volume created", "log", "audit", "user", req.UserID, "template", ref.TemplateID,
				"context", ref.ContextID(), "sandbox", name, "size", size.String(), "reason", "reset")
			break
		}
		// A ResourceQuota can count the deleted volume for a short time.
		if !wait(nil) {
			return fmt.Errorf("the reset deleted the files, but warren cannot make the empty volume. "+
				"The sandbox is not in the instance list: %w", err)
		}
	}
	// A start reads the account of the sandbox and the volume limits from
	// the cache. Keep the mark of the reset until the cache has the volume.
	for {
		wake := m.waiter()
		if v := m.volume(name); (v != nil && v.UID != old.UID && v.DeletionTimestamp == nil) || !wait(wake) {
			return nil
		}
	}
}

// remove deletes the pod and the volume of a sandbox. It returns the volume
// that it deleted or found in deletion, or nil if the sandbox had none. The
// caller must set the resetting mark first. With keep, the caller makes a
// new volume: the function then does not stop at the end of ctx after the
// delete request of the volume, and the sandbox keeps its place in the
// volume limits.
func (m *Manager) remove(ctx context.Context, name string, keep bool) (*corev1.PersistentVolumeClaim, error) {
	wait := func(wake <-chan struct{}) error {
		timer := time.NewTimer(pollInterval)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		case <-timer.C:
		}
		return nil
	}
	// A start request from before the mark can still make a pod. Wait for
	// its result, then remove the pod.
	for {
		wake := m.waiter()
		pod := m.pod(name)
		m.mu.Lock()
		st := m.sandboxes[name]
		starting := st != nil && (st.starting || (st.creating && m.now().Sub(st.createStart) < createStale))
		m.mu.Unlock()
		if pod == nil && !starting {
			break
		}
		if pod != nil && pod.DeletionTimestamp == nil {
			if err := m.deletePod(ctx, pod, "api", false); err != nil {
				return nil, fmt.Errorf("stop sandbox: %w", err)
			}
		}
		if err := wait(wake); err != nil {
			return nil, err
		}
	}

	pvcs := m.cfg.Client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace)
	// gone is the UID of the volume for which this function sent the delete
	// request, or which it found in deletion.
	var gone types.UID
	var old *corev1.PersistentVolumeClaim
	for {
		wake := m.waiter()
		if gone != "" {
			// After the delete request, wait for the cache. Read the API
			// server only when the cache does not show the volume.
			if v := m.volume(name); v != nil {
				if v.UID != gone {
					// A different process made a new volume. It is empty.
					return nil, nil
				}
				// A different warren process can make a pod in this time.
				// The volume cannot go away while a pod uses it.
				if pod := m.pod(name); pod != nil && pod.DeletionTimestamp == nil {
					if err := m.deletePod(ctx, pod, "api", false); err != nil {
						return nil, fmt.Errorf("stop sandbox: %w", err)
					}
				}
				if err := wait(wake); err != nil {
					return nil, err
				}
				continue
			}
		}
		cur, err := pvcs.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return old, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read the volume: %w", err)
		}
		if cur.Labels[LabelManagedBy] != ManagedByValue {
			return nil, fmt.Errorf("a volume with the name %s exists, and warren did not make it", name)
		}
		if gone != "" && cur.UID != gone {
			return nil, nil
		}
		if keep && old == nil {
			m.mu.Lock()
			m.stateLocked(name).kept = cur.Labels
			m.mu.Unlock()
			// From here the files go away. If the client goes away, the reset
			// must still make the new volume. If not, the result is a delete.
			request := ctx
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(context.WithoutCancel(request))
			defer cancel()
			defer context.AfterFunc(request, func() { time.AfterFunc(resetWait, cancel) })()
		}
		if cur.DeletionTimestamp == nil {
			err := pvcs.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &cur.UID}})
			if err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("delete the volume: %w", err)
			}
			if err == nil {
				m.cfg.Metrics.VolumeDelete("api")
				m.cfg.Logger.Info("volume deleted", "log", "audit", "sandbox", name, "reason", "api",
					"template", cur.Labels[LabelTemplate], "context", cur.Annotations[AnnotationContext])
			}
		}
		gone, old = cur.UID, cur
		// Kubernetes removes the volume when no pod uses it.
		if err := wait(wake); err != nil {
			return nil, err
		}
	}
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
