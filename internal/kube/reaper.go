package kube

import (
	"context"
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// persistInterval is the minimum time between two writes of the
	// activity annotation of one pod.
	persistInterval = time.Minute
	// orphanAge is the minimum age of a Secret with no pod before the
	// reaper deletes it. A start request makes the Secret before the pod.
	orphanAge = 2 * time.Minute
)

// activity returns the time of the last use of a pod.
func (m *Manager) activity(pod *corev1.Pod, st *state) time.Time {
	// A time from before the pod is the activity of a previous pod.
	if st != nil && st.lastActivity.After(pod.CreationTimestamp.Time) {
		return st.lastActivity
	}
	last := pod.CreationTimestamp.Time
	if t, err := time.Parse(timeFormat, pod.Annotations[AnnotationActivity]); err == nil && t.After(last) {
		last = t
	}
	// Warren has no lease data from before its start. Do not stop a
	// sandbox immediately after a restart of warren.
	if m.started.After(last) {
		last = m.started
	}
	return last
}

// notReadySince returns the time from which a pod is not ready.
func notReadySince(pod *corev1.Pod) time.Time {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status != corev1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time
		}
	}
	return pod.CreationTimestamp.Time
}

// idleTimeout returns the idle time for a pod. It comes from the policy of
// this time, thus a change of the policy file applies to the pods that
// exist. If the policy is not in the file, it comes from the annotation that
// the pod got at its start.
func (m *Manager) idleTimeout(pod *corev1.Pod) time.Duration {
	if pol, ok := m.cfg.Policies.Get(pod.Labels[LabelTemplate]); ok {
		if pol.IdleTimeout != nil {
			return *pol.IdleTimeout
		}
		return m.cfg.IdleTimeout
	}
	if v, ok := pod.Annotations[AnnotationIdleTimeout]; ok {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return m.cfg.IdleTimeout
}

// volumeGoneAfter is the minimum age of a pod before the reaper deletes it
// because its volume is absent. The cache of the volumes can be later than
// the cache of the pods.
const volumeGoneAfter = 30 * time.Second

// lostVolume reports whether the volume of a pod is in deletion or absent.
// Such a pod cannot keep its files, or it cannot start.
func (m *Manager) lostVolume(pod *corev1.Pod, now time.Time) bool {
	if !usesVolume(pod) {
		return false
	}
	v := m.volume(pod.Name)
	if v != nil {
		return v.DeletionTimestamp != nil
	}
	if now.Sub(pod.CreationTimestamp.Time) <= volumeGoneAfter {
		return false
	}
	// The cache does not show the volume. Ask the API server before the
	// delete of a pod: the watch of the volumes can be broken.
	_, err := m.cfg.Client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	return apierrors.IsNotFound(err)
}

// Reap does one run of the reaper. It deletes sandboxes that are idle,
// failed, or not ready for too long. It writes the activity time to the
// pods, it deletes Secrets that have no pod, and it deletes volumes that no
// sandbox used for the retention time of their policy.
func (m *Manager) Reap(ctx context.Context) {
	now := m.now()
	// A person or a different program can delete or change the policy that
	// refuses all connections in the sandbox namespace.
	if err := m.ensureDefaultDeny(ctx); err != nil {
		text := "warren cannot examine the network policy of the sandbox namespace"
		if !m.networkReady.Load() {
			text = "the sandbox namespace has no network policy that refuses all connections, and warren cannot make it. No sandbox can start."
		}
		m.cfg.Logger.Error(text, "error", err.Error())
	}
	// live has the names of the Secrets that a pod uses. The network policy
	// of a pod has the same name.
	live := map[string]bool{}
	var pods []*corev1.Pod
	previous := 0
	defer func() {
		if previous > 0 {
			m.cfg.Logger.Warn("warren stopped sandboxes that a previous version of warren started. They had no network policy and not the security settings of this version. "+
				"Their volumes stay. Files that were not on a volume are lost.", "sandboxes", previous)
		}
	}()
	for _, obj := range m.informer.GetStore().List() {
		pod := obj.(*corev1.Pod)
		if pod.Labels[LabelManagedBy] != ManagedByValue {
			// The cache has only the pods of warren. This check is a second
			// protection: the reaper runs at the same time as the test of
			// the network plugin, and it must not stop the test pod.
			continue
		}
		pods = append(pods, pod)
		live[podSecretName(pod)] = true
		if pod.DeletionTimestamp != nil {
			m.forceDelete(ctx, pod, now)
			continue
		}
		if pod.Labels[LabelStart] == "" {
			// A previous version of warren made this pod. It has no network
			// policy of its own, thus the policy of the namespace refuses
			// all its connections, also the connections from warren.
			if m.deletePod(ctx, pod, "no_network_policy", false) == nil {
				previous++
			}
			continue
		}

		m.mu.Lock()
		st := m.sandboxes[pod.Name]
		leases := 0
		if st != nil {
			leases = st.leases
			if leases > 0 {
				// An open request or WebSocket is activity.
				st.lastActivity = now
			}
		}
		last := m.activity(pod, st)
		var persist bool
		if st != nil && last.Sub(st.persisted) >= persistInterval {
			st.persisted = last
			persist = true
		}
		m.mu.Unlock()

		m.warnUnschedulable(pod, now)
		startError := m.startError(pod, now)
		switch {
		case podTerminal(pod):
			_ = m.deletePod(ctx, pod, "failed", false)
		case m.lostVolume(pod, now):
			// A different process or a person deleted the volume. All that
			// the user writes now is lost. Stop the pod, thus the next
			// start makes a new volume.
			_ = m.deletePod(ctx, pod, "volume_gone", false)
		case m.orphan(pod.Labels):
			// The policy of the pod is not in the file, or it has a
			// different scope. No new request can get to the pod. An open
			// terminal would keep the pod and its network connections, thus
			// warren does not wait for the end of the connections: the
			// removal of a policy must stop its sandboxes.
			_ = m.deletePod(ctx, pod, "no_policy", false)
		case startError:
			_ = m.deletePod(ctx, pod, "start_error", false)
		case !podReady(pod) && now.Sub(notReadySince(pod)) > m.cfg.StartTimeout:
			_ = m.deletePod(ctx, pod, "not_ready", false)
		case podReady(pod) && leases == 0 && m.idleTimeout(pod) > 0 && now.Sub(last) > m.idleTimeout(pod):
			// deletePod looks at the leases again, together with its mark.
			_ = m.deletePod(ctx, pod, "idle", true)
		case persist:
			m.persistActivity(ctx, pod, last)
		}
	}
	m.deleteOrphanSecrets(ctx, live, now)
	m.reconcileNetworkPolicies(ctx, pods, live)
	m.reapVolumes(ctx, now)

	// Remove the records that have nothing behind them.
	m.mu.Lock()
	for name := range m.sandboxes {
		m.dropLocked(name)
	}
	for name, bad := range m.badKeys {
		if now.Sub(bad.last) > time.Hour {
			delete(m.badKeys, name)
		}
	}
	m.mu.Unlock()
}

// stuckMargin is the time after the end of the grace period at which warren
// removes a pod whose node does not confirm the deletion.
const stuckMargin = 2 * time.Minute

// forceDelete removes a pod that stays in deletion. The deletion time of a
// pod is the time at which its grace period ends.
//
// The removal does not wait for the node. It is safe only for a pod that has
// no data outside the pod. A pod with a volume can continue to operate on a
// node that does not answer. If warren removed it, the next pod would use
// the same volume at the same time, and the files could break. Warren thus
// keeps such a pod, and the sandbox is not available until Kubernetes
// removes the pod.
func (m *Manager) forceDelete(ctx context.Context, pod *corev1.Pod, now time.Time) {
	if now.Sub(pod.DeletionTimestamp.Time) <= stuckMargin {
		return
	}
	if usesVolume(pod) && pod.Spec.NodeName != "" {
		m.mu.Lock()
		st := m.stateLocked(pod.Name)
		logged := st.stuck == pod.UID
		st.stuck = pod.UID
		m.mu.Unlock()
		if !logged {
			m.cfg.Logger.Warn("a sandbox with a volume stays in deletion. Warren does not remove it, because its node can still write to the volume. "+
				"Examine the node.", "sandbox", pod.Name, "node", pod.Spec.NodeName)
		}
		return
	}
	zero := int64(0)
	err := m.cfg.Client.CoreV1().Pods(m.cfg.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
		Preconditions:      &metav1.Preconditions{UID: &pod.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		m.cfg.Logger.Warn("cannot remove a sandbox that stays in deletion", "sandbox", pod.Name, "error", err.Error())
		return
	}
	m.cfg.Metrics.Reap("stuck")
	m.cfg.Logger.Info("sandbox removed", "log", "audit", "sandbox", pod.Name, "reason", "stuck")
}

func (m *Manager) persistActivity(ctx context.Context, pod *corev1.Pod, last time.Time) {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{AnnotationActivity: last.UTC().Format(timeFormat)},
	}})
	_, err := m.cfg.Client.CoreV1().Pods(m.cfg.Namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		m.cfg.Logger.Debug("cannot write the activity time", "sandbox", pod.Name, "error", err.Error())
	}
}

// deleteOrphanSecrets deletes the Secrets of warren that have no pod. Such
// a Secret stays when warren stops in the middle of a start request.
func (m *Manager) deleteOrphanSecrets(ctx context.Context, live map[string]bool, now time.Time) {
	secrets := m.cfg.Client.CoreV1().Secrets(m.cfg.Namespace)
	list, err := secrets.List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		m.cfg.Logger.Debug("cannot list Secrets", "error", err.Error())
		return
	}
	for i := range list.Items {
		s := &list.Items[i]
		if live[s.Name] || now.Sub(s.CreationTimestamp.Time) < orphanAge {
			continue
		}
		// A start request makes the Secret before the pod.
		m.mu.Lock()
		st := m.sandboxes[s.Labels[LabelSandbox]]
		starting := st != nil && (st.starting || st.creating)
		m.mu.Unlock()
		if starting {
			continue
		}
		if err := secrets.Delete(ctx, s.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &s.UID},
		}); err == nil {
			m.cfg.Logger.Info("deleted a Secret that had no sandbox", "secret", s.Name)
		}
	}
}

// unschedulableAfter is the time after which warren writes a log record for
// a pod that has no node.
const unschedulableAfter = time.Minute

// warnUnschedulable writes one log record for a pod for which Kubernetes
// finds no node. The usual causes are a volume that cannot bind, a volume on
// a node that the policy does not permit, and no free resources. Without
// the record, an administrator sees only sandboxes that are not ready.
func (m *Manager) warnUnschedulable(pod *corev1.Pod, now time.Time) {
	if pod.Spec.NodeName != "" {
		return
	}
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse || c.Reason != corev1.PodReasonUnschedulable ||
			now.Sub(c.LastTransitionTime.Time) < unschedulableAfter {
			continue
		}
		m.mu.Lock()
		st := m.stateLocked(pod.Name)
		logged := st.unschedulable == pod.UID
		st.unschedulable = pod.UID
		m.mu.Unlock()
		if !logged {
			m.cfg.Logger.Warn("Kubernetes finds no node for a sandbox", "sandbox", pod.Name,
				"template", pod.Labels[LabelTemplate], "message", c.Message)
		}
	}
}

// startErrorAfter is the time after which warren stops a pod whose container
// cannot start. It counts from the time at which warren saw the condition
// first, until the pod is ready again.
const startErrorAfter = 2 * time.Minute

// waitingErrors are the causes for which a container does not start, and
// which do not go away. The usual cause is an image that does not agree with
// the start command or the user ID of the policy. The second value tells if
// warren stops the pod early: an image that Kubernetes cannot pull at this
// time can come later.
var waitingErrors = map[string]bool{
	"CreateContainerConfigError": true, "CreateContainerError": true, "RunContainerError": true,
	"CrashLoopBackOff": true, "InvalidImageName": true, "ErrImageNeverPull": true,
	"ErrImagePull": false, "ImagePullBackOff": false,
}

// startError writes one log record for a pod whose container cannot start.
// Without the record, an administrator sees only a sandbox that is not
// ready. It reports true when warren must stop the pod.
func (m *Manager) startError(pod *corev1.Pod, now time.Time) bool {
	for _, c := range pod.Status.ContainerStatuses {
		w := c.State.Waiting
		if c.Name != containerName || w == nil {
			continue
		}
		stop, known := waitingErrors[w.Reason]
		if !known {
			continue
		}
		m.mu.Lock()
		st := m.stateLocked(pod.Name)
		logged := st.waiting == pod.UID
		if !logged {
			st.waiting, st.waitingSince = pod.UID, now
		}
		since := st.waitingSince
		m.mu.Unlock()
		if !logged {
			m.cfg.Logger.Warn("the container of a sandbox cannot start. Examine the image, the command, and the user ID of the policy.",
				"sandbox", pod.Name, "template", pod.Labels[LabelTemplate], "reason", w.Reason, "message", w.Message)
		}
		// The time counts from the first sight of the condition, not from
		// the start of the pod. A sandbox that operated, and whose container
		// then stops again and again, gets the same time as a new sandbox:
		// Kubernetes starts the container again with the same home directory.
		return stop && now.Sub(since) > startErrorAfter
	}
	if podReady(pod) {
		// The container operates. A later error is a new condition.
		m.mu.Lock()
		if st := m.sandboxes[pod.Name]; st != nil && st.waiting == pod.UID {
			st.waiting = ""
		}
		m.mu.Unlock()
	}
	return false
}

// reapVolumes marks the volumes that no sandbox uses, and deletes a volume
// when its mark is older than the retention time of its policy.
//
// The mark is the annotation "unused-since". The reaper sets it when a
// volume has no pod, and a start removes it before it makes the pod. Thus
// the time of a volume counts from the stop of its sandbox, and it survives
// a restart of warren.
func (m *Manager) reapVolumes(ctx context.Context, now time.Time) {
	pvcs := m.cfg.Client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace)
	// For some time after its start and after a change of the policies,
	// warren deletes no volume. A policy file with a retention time that is
	// too short would delete many volumes at the first run. The log record
	// tells the administrator the number.
	since := m.policiesSince()
	hold := m.cfg.VolumeDeleteDelay > 0 && now.Sub(since) < m.cfg.VolumeDeleteDelay
	m.reportVolumes(now, since, hold)
	for _, obj := range m.volumes.GetStore().List() {
		pvc := obj.(*corev1.PersistentVolumeClaim)
		if pvc.DeletionTimestamp != nil {
			continue
		}
		name := pvc.Name
		mark, marked := pvc.Annotations[AnnotationUnusedSince]

		// Do not change a volume while a start or a reset uses it, and let
		// no start begin while this function works on the volume.
		m.mu.Lock()
		st := m.sandboxes[name]
		// After a change of the storage type to ephemeral, the pod of the
		// sandbox does not use the volume.
		pod := m.pod(name)
		inUse := pod != nil && usesVolume(pod)
		busy := inUse || (st != nil && (st.starting || st.creating || st.resetting || st.volumeBusy))
		if !busy {
			m.stateLocked(name).volumeBusy = true
		}
		m.mu.Unlock()
		if busy {
			if marked && inUse {
				// A pod uses the volume, thus the mark is wrong.
				m.markVolume(ctx, pvc, "")
			}
			continue
		}

		unused, err := time.Parse(timeFormat, mark)
		switch {
		case !marked || err != nil || unused.After(now):
			m.markVolume(ctx, pvc, now.UTC().Format(timeFormat))
		default:
			// Use the policy of this time. Warren keeps the volumes of a
			// policy that is not in the file.
			if hold || !m.expired(pvc, unused, now) {
				break
			}
			// The policies changed after the start of this run. The delete
			// delay starts again, and the next run writes the log record.
			if !m.policiesSince().Equal(since) {
				break
			}
			// Delete only the volume that this function examined, in the
			// state that it examined. A start of a different warren process
			// removes the mark, and that changes the resource version.
			err := pvcs.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
				UID: &pvc.UID, ResourceVersion: &pvc.ResourceVersion,
			}})
			switch {
			case err == nil:
				m.cfg.Metrics.VolumeDelete("retention")
				m.cfg.Logger.Info("volume deleted", "log", "audit", "sandbox", name, "reason", "retention",
					"template", pvc.Labels[LabelTemplate], "context", pvc.Annotations[AnnotationContext],
					"unused_since", mark)
			case !apierrors.IsNotFound(err) && !apierrors.IsConflict(err):
				m.cfg.Logger.Warn("cannot delete a volume", "sandbox", name, "error", err.Error())
			}
		}

		m.mu.Lock()
		if st := m.sandboxes[name]; st != nil {
			st.volumeBusy = false
			m.dropLocked(name)
		}
		m.wakeLocked()
		m.mu.Unlock()
	}
}

// expired reports whether a volume with no pod is older than the retention
// time of its policy. Warren keeps the volumes that have no policy: the
// policy is not in the file, or it has a different scope. The retention
// time of a policy is not for the volumes of its previous scope.
func (m *Manager) expired(pvc *corev1.PersistentVolumeClaim, since, now time.Time) bool {
	pol, ok := m.cfg.Policies.Get(pvc.Labels[LabelTemplate])
	if !ok || string(pol.Scope) != pvc.Labels[LabelScope] {
		return false
	}
	return pol.Storage.Retention != nil && *pol.Storage.Retention > 0 && now.Sub(since) > *pol.Storage.Retention
}

// volumeInUse reports whether a pod uses the volume of a sandbox.
func (m *Manager) volumeInUse(name string) bool {
	pod := m.pod(name)
	return pod != nil && usesVolume(pod)
}

// reportVolumes writes one log record after the start of warren and after
// each change of the policies: the number of volumes that are past their
// retention time, and the number of volumes that have no policy.
func (m *Manager) reportVolumes(now, since time.Time, hold bool) {
	m.mu.Lock()
	done := m.volumesReported.Equal(since)
	m.volumesReported = since
	m.mu.Unlock()
	if done {
		return
	}
	expired, orphans := 0, 0
	for _, obj := range m.volumes.GetStore().List() {
		pvc := obj.(*corev1.PersistentVolumeClaim)
		if m.orphan(pvc.Labels) {
			orphans++
		}
		if since, err := time.Parse(timeFormat, pvc.Annotations[AnnotationUnusedSince]); err == nil && !m.volumeInUse(pvc.Name) && m.expired(pvc, since, now) {
			expired++
		}
	}
	if expired > 0 {
		text := "volumes are past the retention time of their policy. Warren deletes them now."
		if hold {
			text = "volumes are past the retention time of their policy. Warren deletes them after the delay. " +
				"If this is not correct, change the policy file in that time."
		}
		m.cfg.Logger.Warn(text, "volumes", expired, "delay", m.cfg.VolumeDeleteDelay.String())
	}
	if orphans > 0 {
		m.cfg.Logger.Warn("volumes have no policy: their policy is not in the file, or it has a different scope. "+
			"Warren keeps them. Delete them with kubectl if they are not necessary.", "volumes", orphans)
	}
}

// markVolume sets or removes the "unused-since" annotation of a volume. An
// empty value removes it.
func (m *Manager) markVolume(ctx context.Context, pvc *corev1.PersistentVolumeClaim, value string) {
	var v any
	if value != "" {
		v = value
	}
	// The resource version makes the change conditional: it fails if the
	// volume changed after the cache read it.
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": pvc.ResourceVersion,
		"annotations":     map[string]any{AnnotationUnusedSince: v},
	}})
	_, err := m.cfg.Client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Patch(ctx, pvc.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		m.cfg.Logger.Debug("cannot write the mark of a volume", "sandbox", pvc.Name, "error", err.Error())
	}
}
