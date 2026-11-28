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
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/coderpunch/warren/internal/backend"
	"github.com/coderpunch/warren/internal/metrics"
	"github.com/coderpunch/warren/internal/policy"
	"github.com/coderpunch/warren/internal/scope"
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
	// Network has the data for the network policies of the sandboxes.
	Network NetworkConfig
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
	// Dial makes one connection attempt to the test pod, for the test of the
	// network policies. It returns nil only when the test pod answers. Tests
	// change it.
	Dial func(ctx context.Context, address string) error
	// CanaryInterval is the time between two tests of the network policies.
	// CanaryPoll and CanaryPause are the wait times in one test.
	CanaryInterval, CanaryPoll, CanaryPause time.Duration
	// CanaryRetry is the time before the next test while the recorded result
	// is not "the cluster applies network policies". A test that was not
	// possible keeps the recorded result of a previous test.
	CanaryRetry time.Duration
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
	// leases is the number of open leases that keep the sandbox: the
	// requests that are not WebSockets, and the WebSockets with the mark
	// backend.Request.Hold. While it is not zero, the reaper does not stop
	// the sandbox for its idle time. A passive request does not count, and
	// a WebSocket with no mark does not count.
	leases int
	// sockets has the open leases of the WebSocket requests. Each one is
	// for one pod, and it gets a signal when that pod stops: refer to
	// stopLeasesLocked. Only a lease with the mark Hold is also in the
	// number of leases.
	sockets map[*lease]struct{}
	// lastActivity is the time of the last use: the start of a lease, the
	// end of a lease that is in the number of leases, or a message of a
	// client on a WebSocket (lease.Active). A passive request is no use.
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

	// unreachable is the UID of a ready pod that warren could not use.
	// unreachableSince is the time of the first failure of a sequence of
	// failures, and unreachableLast is the time of the last failure.
	unreachable      types.UID
	unreachableSince time.Time
	unreachableLast  time.Time
	// nodeBlocked is the UID of the pod for which warren made sure that its
	// network policy does not permit the addresses of its node.
	nodeBlocked types.UID

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
	// waiting is the UID of the pod whose container cannot start.
	// waitingReasons has the causes for which warren wrote a log record for
	// that pod. waitingSince is the time at which warren saw a cause that
	// stops the pod first. It is zero while warren saw no such cause.
	waiting        types.UID
	waitingReasons []string
	waitingSince   time.Time
	// resetting is true while a reset operation runs. No start and no
	// lease is possible in that time.
	resetting bool
	// restarting is the number of restart operations that are in progress.
	// While it is not zero, only a restart operation starts the sandbox: a
	// request waits for that start, and it does not send its own. It is a
	// number because two operations can run at the same time.
	restarting int
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
	// networkReady is true after warren made sure one time that the sandbox
	// namespace has the policy that refuses all connections.
	networkReady atomic.Bool
	// otherPolicies is the number of network policies in the sandbox
	// namespace that warren did not make and that the configuration does
	// not accept. It is -1 until warren examined the namespace.
	otherPolicies atomic.Int32
	// policyProblem is the text of the last problem with the network policy
	// of the namespace, and testProblem is the text of the last problem with
	// the test of the network plugin. They are for the readiness report. The
	// reaper writes the first one and the test writes the second one, at the
	// same time, thus each condition has its own text.
	policyProblem atomic.Value
	testProblem   atomic.Value
	// enforcement is the result of the test that shows whether the cluster
	// applies network policies.
	enforcement atomic.Int32
	// hosts has the node addresses that are in the network policies of the
	// sandboxes. The reaper records a new address after a run that wrote all
	// policies, and it removes an address before it writes the policies
	// without that address. A policy that warren makes between two runs gets
	// the addresses of this record: refer to policyHosts. The caller must
	// hold mu.
	hosts map[string]bool
	// kick asks Run for one run of the reaper at this time.
	kick chan struct{}
}

// badKey is the record of pods of one sandbox that refused their key.
type badKey struct {
	count int
	last  time.Time
}

// unreachableLimit is the time after which warren stops a ready pod that it
// cannot connect to.
const unreachableLimit = 60 * time.Second

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
	if err := cfg.Network.validate(cfg.Namespace); err != nil {
		return nil, err
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
	if cfg.Dial == nil {
		cfg.Dial = dialCanary
	}
	if cfg.CanaryInterval <= 0 {
		cfg.CanaryInterval = time.Hour
	}
	if cfg.CanaryPoll <= 0 {
		cfg.CanaryPoll = time.Second
	}
	if cfg.CanaryPause <= 0 {
		cfg.CanaryPause = canaryPause
	}
	if cfg.CanaryRetry <= 0 {
		cfg.CanaryRetry = time.Minute
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
		kick:      make(chan struct{}, 1),
	}
	if m.cfg.Verify == nil {
		m.cfg.Verify = verifyKey
	}
	m.factory = informers.NewSharedInformerFactoryWithOptions(cfg.Client, 0,
		informers.WithNamespace(cfg.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = managedSelector }))
	m.informer = m.factory.Core().V1().Pods().Informer()
	if _, err := m.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { m.podChanged(obj, false) },
		UpdateFunc: func(_, obj any) {
			m.podChanged(obj, false)
			m.nodeChanged(obj)
		},
		DeleteFunc: func(obj any) { m.podChanged(obj, true) },
	}); err != nil {
		return nil, err
	}
	m.otherPolicies.Store(-1)
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

// Run starts the pod cache, the reaper, and the test of the network plugin.
// It returns when the context ends.
//
// No sandbox starts before the namespace refuses all connections that the
// policy of a sandbox does not permit, before the reaper examined the
// network policies of the namespace, and before the test showed that the
// cluster applies the policies: refer to NotReadyReason. Run does not wait
// for these conditions. The reaper must operate in all conditions: the
// sandboxes from before a restart of warren need their idle stop and their
// network policies also while warren can start no sandbox, and only the
// reaper can make a resource quota free that stops the test pod.
func (m *Manager) Run(ctx context.Context) error {
	m.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced, m.volumes.HasSynced) {
		return errors.New("kube: the cache of the pods and the volumes did not start")
	}
	// One run of the reaper before the first test: it makes the network
	// policy of the namespace, and it examines the network policies of the
	// namespace and of the pods that exist.
	m.Reap(ctx)
	// The test has its own goroutine. One test can wait some minutes for its
	// pod, and the reaper must not wait for it.
	tests := make(chan struct{})
	go func() {
		defer close(tests)
		m.runEnforcementTests(ctx)
	}()
	defer func() { <-tests }()

	ticker := time.NewTicker(m.cfg.ReapInterval)
	defer ticker.Stop()
	ready := false
	for {
		if !ready && m.Ready() {
			ready = true
			m.cfg.Logger.Info("sandbox backend ready", "namespace", m.cfg.Namespace,
				"sandboxes", len(m.informer.GetStore().List()), "volumes", len(m.volumes.GetStore().List()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.Reap(ctx)
		case <-m.kick:
			// A pod got a node that is not in the network policies.
			m.Reap(ctx)
		}
	}
}

// runEnforcementTests does the test of the network plugin again and again,
// until the context ends. A network plugin can stop or change while warren
// operates, and an administrator can correct a network plugin.
func (m *Manager) runEnforcementTests(ctx context.Context) {
	for {
		wait := m.cfg.ReapInterval
		// The test needs the policy of the namespace. With no such policy,
		// the test is not possible. The reaper makes the policy.
		if m.networkReady.Load() {
			m.examineEnforcement(ctx)
			wait = m.cfg.CanaryRetry
			if m.enforcement.Load() == enforcementProven {
				wait = m.cfg.CanaryInterval
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// nodeChanged asks for a run of the reaper when a pod has a node address
// that is not in the network policies of the sandboxes. Warren knows the
// node of a pod only after the start request, thus the policy of a new pod
// on a new node does not have the address of that node.
func (m *Manager) nodeChanged(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.DeletionTimestamp != nil {
		return
	}
	m.mu.Lock()
	known := true
	for _, cidr := range m.podHosts(pod) {
		known = known && m.hosts[cidr]
	}
	m.mu.Unlock()
	if !known {
		select {
		case m.kick <- struct{}{}:
		default:
		}
	}
}

// Synced reports whether the cache has the current state of the cluster.
// Without it, warren cannot see the sandboxes.
func (m *Manager) Synced() bool { return m.informer.HasSynced() && m.volumes.HasSynced() }

// Ready reports whether a sandbox can start: the cache has the current state
// of the cluster, and the sandbox namespace refuses the connections that no
// policy permits. The operations that only stop or delete need Synced only.
func (m *Manager) Ready() bool { return m.NotReadyReason() == "" }

// NotReadyReason returns the cause for which no sandbox can start, or an
// empty text.
func (m *Manager) NotReadyReason() string {
	switch {
	case !m.Synced():
		return "the cache of the pods and the volumes is not ready"
	case !m.networkReady.Load():
		return withCause("the sandbox namespace has no network policy that refuses all connections", &m.policyProblem)
	case m.otherPolicies.Load() < 0:
		return "warren did not examine the network policies of the sandbox namespace"
	case m.enforcement.Load() == enforcementAbsent:
		return "the cluster does not apply network policies"
	case m.enforcement.Load() != enforcementProven:
		return withCause("the test of the network policies is not complete", &m.testProblem)
	}
	return ""
}

// withCause adds the text of a problem to a condition, if there is one.
func withCause(condition string, problem *atomic.Value) string {
	if text, _ := problem.Load().(string); text != "" {
		return condition + ": " + text
	}
	return condition
}

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
		if deleted || pod.DeletionTimestamp != nil {
			// The pod stops. If warren sent the delete request, the leases
			// have their signal and its reason. If not, a different actor
			// deletes the pod.
			m.stopLeasesLocked(st, pod.UID, backend.StopExternal)
		}
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
	if st != nil && st.leases == 0 && len(st.sockets) == 0 && !st.starting && !st.creating && st.deleting == "" &&
		!st.resetting && st.restarting == 0 && !st.volumeBusy && m.pod(name) == nil {
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

// podAddresses returns all addresses of a pod.
func podAddresses(pod *corev1.Pod) []string {
	ips := []string{pod.Status.PodIP}
	for _, p := range pod.Status.PodIPs {
		if p.IP != pod.Status.PodIP {
			ips = append(ips, p.IP)
		}
	}
	return ips
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
//
// Acquire returns ErrNotReady while warren is not ready, also to a caller
// that waits at that time. A lease that a caller has stays.
//
// A passive request (backend.Request.Passive) never starts the sandbox. It
// waits only while a start that a different caller began is in progress,
// while a restart operation is in progress, or while a pod is not ready or
// not usable. In all other conditions with no usable pod it gets ErrStopped
// immediately, also in the middle of the wait. Its lease is not a use of the
// sandbox: refer to tryAcquire.
//
// While a restart operation is in progress, no request starts the sandbox.
// Each request waits for the start of that operation: refer to Restart.
//
// A request that does not begin a start (backend.Request.NoStart) sends no
// start request and no delete request for a pod in a final phase. While the
// sandbox has no usable pod, it waits for the start of a different request or
// operation, until its context ends. In that wait it has no lease, it makes
// no record of the sandbox, and it is not a use of the sandbox. The limits on
// the number of sandboxes and the wait time after refused keys do not apply
// to it, because it starts nothing. On a pod that is ready, it gets the lease
// of a usual request, after the same checks.
//
// The lease of a WebSocket request (backend.Request.WebSocket) is for one
// pod. Its Stopped channel closes when that pod stops. The start of the
// lease is a use of the sandbox. After that, the open WebSocket is not a
// use, and it does not keep the sandbox: only a call of Active is a use.
// Thus the reaper can stop the sandbox for its idle time while the WebSocket
// is open. A WebSocket request with the mark backend.Request.Hold is
// different: its lease keeps the sandbox while it is open, as the lease of a
// request that is not a WebSocket.
func (m *Manager) Acquire(ctx context.Context, req backend.Request) (backend.Lease, error) {
	name := req.Ref.Name()
	for {
		wake := m.waiter()
		// Without the pod cache, warren cannot see or count the sandboxes.
		// Warren can also become not ready while the caller waits. Thus each
		// step examines the readiness, not only the first one.
		if !m.Ready() {
			return nil, backend.ErrNotReady
		}
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
	m.mu.Lock()
	// Read the state, but do not make it. A request that the limit
	// refuses must not leave a record.
	st := m.sandboxes[name]
	// Read the pod after the state. The mark of a start in progress goes
	// away only after the cache has the pod. In the opposite sequence, a
	// passive request could see no pod and no start while a pod starts.
	pod := m.pod(name)
	deleting := pod != nil && st != nil && st.deleting == pod.UID
	resetting := st != nil && st.resetting
	restarting := st != nil && st.restarting > 0
	starting := st != nil && st.startInProgress(m.now())
	bad := m.badKeys[name]
	m.mu.Unlock()

	if req.Passive {
		// A passive request does not start a sandbox, does not delete a pod,
		// and does not wait for the end of a stop or a reset. It waits only
		// for a start that is in progress, for the start of a restart
		// operation, and for a pod that is not ready or not usable: the
		// switch and the checks below are the same for each request in these
		// conditions.
		switch {
		case resetting:
			return nil, backend.ErrStopped
		case pod == nil && (starting || restarting):
			// A different caller began the start, or a restart operation
			// starts the sandbox after the end of the previous pod. Wait for
			// the pod.
			return nil, nil
		case pod == nil:
			return nil, backend.ErrStopped
		case pod.Labels[LabelManagedBy] != ManagedByValue:
			// The switch below gives the error.
		case restarting && (pod.DeletionTimestamp != nil || deleting || podTerminal(pod)):
			// A restart operation removes this pod and starts the sandbox
			// again. Wait for the new pod. If the operation ends with no
			// start, the next step gives the reply of a stopped sandbox.
			return nil, nil
		case pod.DeletionTimestamp != nil || deleting || podTerminal(pod):
			// The reaper deletes a pod in a final phase.
			return nil, backend.ErrStopped
		}
	}

	switch {
	case resetting:
		// A reset deletes the pod and the volume. Wait for its end.
		return nil, nil
	case restarting && pod == nil:
		// A restart operation starts the sandbox, on the account of the user
		// that started it first, and with no limit for that user. The start
		// of a request uses the user of the request and the limit of that
		// user. Thus the request does not start the sandbox at this time: it
		// waits for the start of the operation.
		return nil, nil
	case pod == nil:
		if req.NoStart {
			// The request does not begin a start. It waits for the pod of a
			// start that a different request or operation begins. This step
			// made no record of the sandbox, and it makes none.
			return nil, nil
		}
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
		// The delete of this pod is the first step of the next start. A
		// request that does not begin a start leaves the pod to the reaper.
		if !req.NoStart {
			_ = m.deletePod(ctx, pod, "failed", notIdle)
		}
		return nil, nil
	case !podReady(pod):
		return nil, nil
	case m.volumeGone(pod):
		// The files of this pod go away with its volume. The reaper deletes
		// the pod, and the next start makes a new volume.
		return nil, nil
	}
	// The "internet" mode permits public addresses. If the pods of the
	// cluster have public addresses, a sandbox could connect to them. On a
	// cluster with IPv4 and IPv6, a pod has one address of each family, and
	// only the second one can be public.
	for _, ip := range podAddresses(pod) {
		addr, err := netip.ParseAddr(ip)
		if err != nil || m.cfg.Network.Blocked(addr) {
			continue
		}
		_ = m.deletePod(ctx, pod, "public_address", notIdle)
		m.cfg.Logger.Error("a sandbox has an address that the internet mode does not refuse. Warren stopped it. "+
			"Add the pod network of the cluster to the blocked networks of the configuration (--egress-blocked-cidrs).",
			"sandbox", name, "address", addr.String())
		return nil, fmt.Errorf("the pod network of the cluster has the public address %s", addr)
	}
	// The start request made the network policy before Kubernetes selected
	// a node for the pod.
	if err := m.blockNode(ctx, pod); err != nil {
		m.cfg.Logger.Debug("sandbox not usable at this time: its network policy does not have the address of its node",
			"sandbox", name, "error", err.Error())
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
		_ = m.deletePod(ctx, pod, "bad_key", notIdle)
		if bad.count >= maxBadKeys {
			m.cfg.Logger.Warn("a sandbox refuses its key again. Examine the image of the sandbox.",
				"sandbox", name, "count", bad.count)
			return nil, fmt.Errorf("the sandbox refused its key %d times", bad.count)
		}
		return nil, nil
	}
	if err != nil {
		m.cfg.Logger.Debug("sandbox not usable at this time", "sandbox", name, "error", err.Error())
		if ctx.Err() != nil {
			// The request ended in the middle of the check. That shows
			// nothing about the sandbox.
			return nil, nil
		}
		// The pod is ready for Kubernetes, but warren cannot use it. The
		// usual cause is a network policy that does not permit warren. Such
		// a pod would stay with no end.
		m.mu.Lock()
		st := m.stateLocked(name)
		now := m.now()
		// The time counts for failures in sequence only. After a long time
		// with no attempt, warren does not know that the pod was not usable
		// in that time: one failure then starts a new sequence.
		if st.unreachable != pod.UID || now.Sub(st.unreachableLast) > unreachableLimit {
			st.unreachable, st.unreachableSince = pod.UID, now
		}
		st.unreachableLast = now
		tooLong := now.Sub(st.unreachableSince) > unreachableLimit
		m.mu.Unlock()
		if tooLong {
			m.cfg.Logger.Error("warren cannot connect to a sandbox that is ready. Warren stops it. Examine the network policies: "+
				"the pod labels in the configuration of warren must be the labels of the warren pods.",
				"sandbox", name, "error", err.Error())
			_ = m.deletePod(ctx, pod, "unreachable", notIdle)
			return nil, fmt.Errorf("warren cannot connect to the sandbox: %w", err)
		}
		return nil, nil
	}

	// The key check and the write of the node address took time. Warren can
	// be not ready now. A caller that waited must get no lease then, as a new
	// caller gets none.
	if !m.Ready() {
		return nil, backend.ErrNotReady
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st = m.stateLocked(name)
	// The pod can be in deletion, or a different pod, since the first look.
	if cur := m.pod(name); cur == nil || cur.UID != pod.UID || cur.DeletionTimestamp != nil || st.deleting == pod.UID || st.resetting {
		return nil, nil
	}
	if req.Passive {
		// This lease is not a use of the sandbox. It is not in the lease
		// count, it does not change the activity time, and it is not the
		// first lease of a cold start. Thus the idle stop does not wait for
		// it.
		return &lease{m: m, name: name, target: target, passive: true}, nil
	}
	// The start of each lease is a use of the sandbox.
	now := m.now()
	st.lastActivity = now
	if st.coldStart {
		st.coldStart = false
		m.cfg.Metrics.ColdStart(now.Sub(st.createStart))
	}
	l := &lease{m: m, name: name, target: target}
	if req.WebSocket {
		// The lease is for this pod. The checks above made sure, with the
		// lock, that the pod is not in deletion. Thus each stop of the pod
		// comes after this point and finds the lease.
		l.uid, l.stopped = pod.UID, make(chan struct{})
		l.hold = req.Hold
		l.marked.Store(int64(now.Sub(m.started)))
		if st.sockets == nil {
			st.sockets = map[*lease]struct{}{}
		}
		st.sockets[l] = struct{}{}
	}
	if l.keeps() {
		st.leases++
	}
	return l, nil
}

type lease struct {
	m      *Manager
	name   string
	target backend.Target
	// passive is true for the lease of a passive request. Such a lease has
	// only the target.
	passive bool
	once    sync.Once

	// The fields below are only for the lease of a WebSocket request. uid is
	// the pod of the lease. stopped closes when warren stops that pod, or
	// sees that a different actor deletes it.
	uid     types.UID
	stopped chan struct{}
	// over is true after stopped closed, and reason is the cause. The lock
	// of the Manager guards the two fields.
	over   bool
	reason string
	// hold is true for a WebSocket whose open connection keeps the sandbox
	// (backend.Request.Hold).
	hold bool
	// marked is the time at which this lease wrote the activity time of its
	// sandbox last: refer to Active. It is not a clock time. It is the time
	// after the start of warren, in nanoseconds, and thus a difference of two
	// readings of the monotonic clock. A step of the system clock does not
	// change it.
	marked atomic.Int64
}

// keeps reports whether the lease is in the number of leases of its sandbox.
// While that number is not zero, the reaper does not stop the sandbox for its
// idle time. The lease of a request that is not a WebSocket keeps the
// sandbox, and the lease of a WebSocket with the mark Hold. The lease of each
// other WebSocket, and the lease of a passive request, do not.
func (l *lease) keeps() bool {
	return !l.passive && (l.stopped == nil || l.hold)
}

// activityInterval is the minimum time between two writes of the activity
// time by one WebSocket lease. A stream of messages thus takes the lock of
// the Manager one time in this interval, and not one time for each message.
// The recorded time of the last use can be early by this interval. An idle
// time is in minutes.
const activityInterval = 10 * time.Second

// Active records a use of the sandbox through the WebSocket of this lease.
// The proxy calls it for each message of the client that is a use. Most calls
// read one atomic value and return. Active never calls Kubernetes: the reaper
// writes the activity time to the pod.
//
// A lease is for one pod. A call after the stop signal, or after Release,
// writes nothing. Thus a relay to a previous pod cannot keep the next pod of
// the same sandbox. A lease that keeps the sandbox is a use from its start to
// its end, and Active does nothing for it: the lease of a request that is not
// a WebSocket, and the lease of a WebSocket with the mark Hold.
//
// Active takes only the lock of the Manager, for a short time, and the
// Manager calls no function of a lease while it has that lock.
func (l *lease) Active() {
	if l.stopped == nil || l.hold {
		return
	}
	// The interval uses the monotonic clock, as the idle time of the reaper
	// does. With the system clock, a step to an earlier time would make this
	// function ignore each message for the time of the step, and the reaper
	// would stop a sandbox that a person uses.
	now := l.m.now()
	since := int64(now.Sub(l.m.started))
	if since-l.marked.Load() < int64(activityInterval) {
		return
	}
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	st := l.m.sandboxes[l.name]
	if st == nil || l.over {
		return
	}
	if _, open := st.sockets[l]; !open {
		return
	}
	l.marked.Store(since)
	// A different lease can have a later time: this call read the time
	// before it had the lock.
	if now.After(st.lastActivity) {
		st.lastActivity = now
	}
}

func (l *lease) Target() backend.Target { return l.target }

// Stopped returns the channel of the stop signal. For a lease that is not
// the lease of a WebSocket request, the channel is nil: it never closes.
func (l *lease) Stopped() <-chan struct{} { return l.stopped }

func (l *lease) StopReason() string {
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	return l.reason
}

func (l *lease) Release() {
	if l.passive {
		return
	}
	l.once.Do(func() {
		l.m.mu.Lock()
		defer l.m.mu.Unlock()
		st := l.m.sandboxes[l.name]
		if st == nil {
			return
		}
		delete(st.sockets, l)
		if l.keeps() {
			// The lease was a use of the sandbox until this time.
			st.leases--
			st.lastActivity = l.m.now()
		}
		// The end of a WebSocket that does not keep the sandbox is not a
		// use. If it was, a client that connects and goes away again and
		// again could hold a sandbox with the two times of each connection.
		l.m.dropLocked(l.name)
	})
}

// stopLeasesLocked gives the stop signal to each WebSocket lease on the pod
// with this UID. A lease that has the signal keeps its first reason. A lease
// on a different pod of the same sandbox gets no signal. The caller must
// hold m.mu.
func (m *Manager) stopLeasesLocked(st *state, uid types.UID, reason string) {
	for l := range st.sockets {
		if l.uid != uid || l.over {
			continue
		}
		l.over, l.reason = true, reason
		close(l.stopped)
	}
}

// createStale is the time after which warren does not wait for the pod of
// a start request to show in the cache.
const createStale = 20 * time.Second

// startInProgress reports whether a start request for the sandbox runs, or
// whether warren waits for the pod of a start request to show in the cache.
// The caller must hold the lock of the Manager.
func (st *state) startInProgress(now time.Time) bool {
	return st.starting || (st.creating && now.Sub(st.createStart) < createStale)
}

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
	// A caller can wait before this call, for example for the end of the
	// previous pod. Warren can become not ready in that time.
	if !m.Ready() {
		return backend.ErrNotReady
	}

	m.mu.Lock()
	st := m.stateLocked(name)
	if st.startInProgress(m.now()) {
		m.mu.Unlock()
		return nil
	}
	// A network policy of a different owner can permit what the policies of
	// warren refuse.
	if n := m.otherPolicies.Load(); n != 0 {
		m.dropLocked(name)
		m.mu.Unlock()
		if n < 0 {
			return errors.New("warren cannot examine the network policies of the sandbox namespace")
		}
		return fmt.Errorf("the sandbox namespace has %d network policies that warren did not make", n)
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
	// removeStartObjects deletes the Secret and the network policy of this
	// start request only.
	networkPolicies := m.cfg.Client.NetworkingV1().NetworkPolicies(m.cfg.Namespace)
	var policyUID types.UID
	removeStartObjects := func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = secrets.Delete(c, secretName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &made.UID}})
		if policyUID != "" {
			_ = networkPolicies.Delete(c, secretName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &policyUID}})
		}
	}

	// The network policy comes before the pod. The pod must not operate at
	// a time when only the policy of the namespace applies to it: it would
	// not be wrong, but warren could not connect to the sandbox.
	podSpec := m.buildPod(name, secretName, req, pol)
	madePolicy, err := networkPolicies.Create(ctx, m.buildNetworkPolicy(secretName, podSpec.Labels, pol, m.policyHosts()), metav1.CreateOptions{})
	if err != nil {
		removeStartObjects()
		removeNewVolume()
		return false, fmt.Errorf("make the network policy: %w", err)
	}
	policyUID = madePolicy.UID

	pod, err := pods.Create(ctx, podSpec, metav1.CreateOptions{})
	if err != nil {
		removeStartObjects()
		if apierrors.IsAlreadyExists(err) {
			// A different process made the pod at the same time. That
			// pod has the Secret of its own start, and it uses the volume.
			return false, nil
		}
		removeNewVolume()
		return false, err
	}
	if pol.UserNamespace != nil && *pol.UserNamespace && (pod.Spec.HostUsers == nil || *pod.Spec.HostUsers) {
		// An API server that does not have the feature UserNamespacesSupport
		// removes the field and gives no error. The pod would operate with
		// the user IDs of the node.
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		zero := int64(0)
		err := pods.Delete(c, name, metav1.DeleteOptions{GracePeriodSeconds: &zero, Preconditions: &metav1.Preconditions{UID: &pod.UID}})
		if err != nil && !apierrors.IsNotFound(err) {
			// The reaper deletes the Secret and the network policy after the pod.
			return false, fmt.Errorf("the cluster ignores the user namespace of the policy (userNamespace: true), and warren cannot delete the pod: %w", err)
		}
		removeStartObjects()
		removeNewVolume()
		return false, errors.New("the cluster ignores the user namespace of the policy (userNamespace: true). " +
			"The API server must have the feature UserNamespacesSupport")
	}

	// The pod owns its Secret. Kubernetes deletes the Secret with the pod.
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []map[string]any{{
		"apiVersion": "v1", "kind": "Pod", "name": pod.Name, "uid": pod.UID,
	}}}})
	if _, err := secrets.Patch(ctx, secretName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		m.cfg.Logger.Warn("cannot set the owner of a Secret. The reaper deletes it later.", "sandbox", name, "error", err.Error())
	}
	if _, err := networkPolicies.Patch(ctx, secretName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		m.cfg.Logger.Warn("cannot set the owner of a network policy. The reaper sets it later.", "sandbox", name, "error", err.Error())
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
	st.unreachable = ""
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

// notIdle is the idleSince value of deletePod for each stop that is not the
// idle stop.
var notIdle time.Time

// deletePod deletes a pod and records the reason.
//
// idleSince is not zero only for the idle stop. It is the time of the last
// use that the reaper read when it made its decision. The function then makes
// the decision again, together with its mark: it does nothing if the sandbox
// has a lease that keeps it, or if the sandbox had a use after that time. For
// each other reason, idleSince is notIdle, and the function has no such
// condition.
//
// The WebSocket leases on the pod get the stop signal with the reason before
// the delete request. The pod gets its stop signal from Kubernetes only after
// that request. Thus the proxy closes each WebSocket with the code of warren,
// and not with a code that the sandbox sends when it stops. If the delete
// request fails, the signal stays: the WebSockets close, and the pod
// continues.
func (m *Manager) deletePod(ctx context.Context, pod *corev1.Pod, reason string, idleSince time.Time) error {
	m.mu.Lock()
	st := m.stateLocked(pod.Name)
	used := !idleSince.IsZero() && (st.leases > 0 || st.lastActivity.After(idleSince))
	if st.deleting == pod.UID || used {
		m.mu.Unlock()
		return nil
	}
	st.deleting = pod.UID
	m.stopLeasesLocked(st, pod.UID, reason)
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

// deleteSent reports whether warren sent the delete request for a pod. The
// cache gets the deletion time of a pod only with an event that comes after
// the reply to that request. In the time between, the pod in the cache has no
// sign of the stop, and only the record of the sandbox shows it. sent is the
// delete mark of that record. A caller must read the mark before it reads the
// pod from the cache: the mark goes away when the pod is not in the cache.
func deleteSent(pod *corev1.Pod, sent types.UID) bool {
	return sent != "" && pod.UID == sent
}

// deleteMark returns the UID of the pod of a sandbox for which warren sent
// the delete request, or an empty value: refer to deleteSent.
func (m *Manager) deleteMark(name string) types.UID {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.sandboxes[name]; st != nil {
		return st.deleting
	}
	return ""
}

// InstanceStatus returns the status of one sandbox. A pod for which warren
// sent the delete request is a stopped sandbox, as a pod in deletion is. The
// management API reads the status immediately after a stop operation.
func (m *Manager) InstanceStatus(ref scope.Ref) string {
	name := ref.Name()
	sent := m.deleteMark(name)
	pod := m.pod(name)
	if pod != nil && deleteSent(pod, sent) {
		return StatusStopped
	}
	return podStatus(pod)
}

// Instances returns the sandboxes of one template. A sandbox that has a
// volume and no pod is a stopped sandbox.
//
// The list has no entry for a pod in deletion, and none for a pod for which
// warren sent the delete request (deleteSent). The sandbox of such a pod is in
// the list only if it has a volume, as a stopped sandbox.
func (m *Manager) Instances(templateID string) []Instance {
	var out []Instance
	seen := map[string]bool{}
	// Read the delete marks before the pods.
	sent := map[string]types.UID{}
	m.mu.Lock()
	for name, st := range m.sandboxes {
		if st.deleting != "" {
			sent[name] = st.deleting
		}
	}
	m.mu.Unlock()
	for _, obj := range m.informer.GetStore().List() {
		pod := obj.(*corev1.Pod)
		if pod.Labels[LabelTemplate] != templateID || pod.DeletionTimestamp != nil || deleteSent(pod, sent[pod.Name]) || m.orphan(pod.Labels) {
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
// or in deletion, or if warren sent the delete request for it, Start waits
// until that pod is gone.
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
	sent := m.deleteMark(name)
	pod := m.pod(name)
	// A pod for which warren sent the delete request stops, also while the
	// cache does not have its deletion time: refer to deleteSent. The steps
	// below wait for its end, as for a pod in deletion.
	if pod != nil && pod.DeletionTimestamp == nil && !deleteSent(pod, sent) && !podTerminal(pod) {
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
		if pod.DeletionTimestamp == nil && !deleteSent(pod, sent) {
			// The pod is in a final phase, and nothing deletes it.
			if err := m.deletePod(ctx, pod, "failed", notIdle); err != nil {
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
	// A stop needs the cache only. When the cluster applies no network
	// policies, an administrator must be able to stop the sandboxes.
	if !m.Synced() {
		return backend.ErrNotReady
	}
	if pod := m.pod(ref.Name()); pod != nil && pod.DeletionTimestamp == nil {
		return m.deletePod(ctx, pod, backend.StopAPI, notIdle)
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
//
// While the operation is in progress, only the operation starts the sandbox.
// The clients of the WebSockets that the stop closed connect again in a short
// time, and they wait in Acquire when the pod goes away. Without the mark,
// one of them can send the start request first, with its own user and with
// the limit of that user. If the operation ends with no start, for example
// when its context ends or when warren has no capacity, the requests that
// wait start the sandbox as usual.
func (m *Manager) Restart(ctx context.Context, req backend.Request) error {
	if !m.Ready() {
		return backend.ErrNotReady
	}
	name := req.Ref.Name()
	m.mu.Lock()
	m.stateLocked(name).restarting++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		// The state stays while the mark is set: refer to dropLocked.
		if st := m.sandboxes[name]; st != nil && st.restarting > 0 {
			st.restarting--
		}
		m.dropLocked(name)
		m.wakeLocked()
		m.mu.Unlock()
	}()
	by := ""
	if pod := m.pod(name); pod != nil {
		// For the sandbox of a chat, a model, or a template, Open WebUI
		// sends the ID of the manager (docs/protocol.md, section 4.6).
		by = pod.Annotations[AnnotationCreatedBy]
		if pod.DeletionTimestamp == nil {
			if err := m.deletePod(ctx, pod, backend.StopAPI, notIdle); err != nil {
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
	if !m.Synced() {
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
		starting := st != nil && st.startInProgress(m.now())
		m.mu.Unlock()
		if pod == nil && !starting {
			break
		}
		if pod != nil && pod.DeletionTimestamp == nil {
			if err := m.deletePod(ctx, pod, backend.StopAPI, notIdle); err != nil {
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
					if err := m.deletePod(ctx, pod, backend.StopAPI, notIdle); err != nil {
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
