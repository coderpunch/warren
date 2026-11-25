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
	if st != nil && !st.lastActivity.IsZero() {
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
	live := map[string]bool{}
	for _, obj := range m.informer.GetStore().List() {
		pod := obj.(*corev1.Pod)
		live[pod.Name] = true
		if pod.DeletionTimestamp != nil {
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
			m.deletePod(ctx, pod, "failed")
		case !podReady(pod) && now.Sub(notReadySince(pod)) > m.cfg.StartTimeout:
			m.deletePod(ctx, pod, "not_ready")
		case podReady(pod) && leases == 0 && m.cfg.IdleTimeout > 0 && now.Sub(last) > m.cfg.IdleTimeout:
			m.deleteIdle(ctx, pod)
		case persist:
			m.persistActivity(ctx, pod, last)
		}
	}
	m.deleteOrphanSecrets(ctx, live, now)
}

// deleteIdle deletes an idle pod, if no lease started after the check.
func (m *Manager) deleteIdle(ctx context.Context, pod *corev1.Pod) {
	m.mu.Lock()
	st := m.sandboxes[pod.Name]
	busy := st != nil && st.leases > 0
	m.mu.Unlock()
	if busy {
		return
	}
	m.deletePod(ctx, pod, "idle")
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
		m.mu.Lock()
		st := m.sandboxes[s.Name]
		creating := st != nil && st.creating
		m.mu.Unlock()
		if creating {
			continue
		}
		if err := secrets.Delete(ctx, s.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &s.UID},
		}); err == nil {
			m.cfg.Logger.Info("deleted a Secret that had no sandbox", "secret", s.Name)
		}
	}
}
