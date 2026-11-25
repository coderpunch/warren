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

// Reap does one run of the reaper. It deletes sandboxes that are idle,
// failed, or not ready for too long. It writes the activity time to the
// pods, and it deletes Secrets that have no pod.
func (m *Manager) Reap(ctx context.Context) {
	now := m.now()
	// live has the names of the Secrets that a pod uses.
	live := map[string]bool{}
	for _, obj := range m.informer.GetStore().List() {
		pod := obj.(*corev1.Pod)
		live[podSecretName(pod)] = true
		if pod.DeletionTimestamp != nil {
			m.forceDelete(ctx, pod, now)
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

		switch {
		case podTerminal(pod):
			_ = m.deletePod(ctx, pod, "failed", false)
		case !podReady(pod) && now.Sub(notReadySince(pod)) > m.cfg.StartTimeout:
			_ = m.deletePod(ctx, pod, "not_ready", false)
		case podReady(pod) && leases == 0 && m.cfg.IdleTimeout > 0 && now.Sub(last) > m.cfg.IdleTimeout:
			// deletePod looks at the leases again, together with its mark.
			_ = m.deletePod(ctx, pod, "idle", true)
		case persist:
			m.persistActivity(ctx, pod, last)
		}
	}
	m.deleteOrphanSecrets(ctx, live, now)

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
// pod is the time at which its grace period ends. A sandbox has no data
// outside its pod, thus the removal is safe.
func (m *Manager) forceDelete(ctx context.Context, pod *corev1.Pod, now time.Time) {
	if now.Sub(pod.DeletionTimestamp.Time) <= stuckMargin {
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
