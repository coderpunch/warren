package kube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/h3xcat/warren/internal/policy"
)

// A network policy is only a request to the network plugin of the cluster.
// Some plugins ignore it, and Kubernetes gives no error. All sandboxes could
// then connect to all addresses. Warren thus makes a test with one small pod
// (the "canary") in the sandbox namespace:
//
//  1. The pod has a network policy as a sandbox has it. Warren must get a
//     connection to the pod. This shows that the pod labels and the
//     namespace in the configuration of warren are correct. If they are
//     not, no request could get to a sandbox.
//  2. Warren deletes that policy. Only the policy of the namespace applies
//     to the pod now, and it must refuse the connection. If the connection
//     is successful, the cluster does not apply network policies, and warren
//     starts no sandbox.

const (
	// LabelCanary is on the test pod. The pod does not have the label of
	// the sandboxes, thus the cache and the reaper ignore it.
	LabelCanary = prefix + "/canary"

	// Results of the test.
	enforcementUnknown int32 = iota
	enforcementProven
	enforcementAbsent

	canaryStartTimeout = 4 * time.Minute
	// canaryAttempts and canaryPause give a network plugin some time to
	// apply the policy of the namespace to a new pod.
	canaryAttempts = 6
	canaryPause    = 4 * time.Second
	// canarySlow is the time after which warren writes a warning: for that
	// time, a new pod had no network policy.
	canarySlow = 3 * time.Second
)

// errNotEnforced means that the cluster does not apply network policies.
var errNotEnforced = errors.New("the cluster does not apply network policies")

// dialTCP opens and closes one TCP connection.
func dialTCP(ctx context.Context, address string) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}

// buildCanary makes the test pod. It has the security settings of a sandbox,
// and it listens on the port of a sandbox.
func (m *Manager) buildCanary(name string) *corev1.Pod {
	no, yes := false, true
	grace, deadline := int64(1), int64(900)
	user, group := int64(defaultUser), int64(volumeGroup)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.cfg.Namespace,
			// The network policy of the test selects the pod with the start
			// label, as the policy of a sandbox does.
			Labels: map[string]string{LabelCanary: "true", LabelStart: name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &grace,
			// Kubernetes stops the pod after this time if warren does not
			// delete it.
			ActiveDeadlineSeconds:        &deadline,
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &yes,
				RunAsUser:      &user,
				RunAsGroup:     &group,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Volumes: []corev1.Volume{
				{Name: homeVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			Containers: []corev1.Container{{
				Name:  containerName,
				Image: m.cfg.Image,
				// The test needs only a program that listens on the port
				// of a sandbox. Python is at this path in all images of Open
				// Terminal, thus the test does not depend on the start
				// command of a policy.
				Command:    []string{"/usr/local/bin/python3", "-I", "-m", "http.server", strconv.Itoa(terminalPort)},
				WorkingDir: "/tmp",
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					Privileged:               &no,
					ReadOnlyRootFilesystem:   &yes,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("128Mi"),
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: homeVolume, MountPath: homePath},
					{Name: tmpVolume, MountPath: "/tmp"},
				},
				// The kubelet connects from the node. The policy of the
				// namespace does not refuse that connection. Thus "ready"
				// shows that the server listens.
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(terminalPort)}},
					PeriodSeconds: 1, TimeoutSeconds: 2, FailureThreshold: 3,
				},
			}},
		},
	}
}

// errUnreachable means that warren cannot connect to a pod whose network
// policy permits warren.
var errUnreachable = errors.New("warren cannot connect to a pod that its network policy permits")

// checkEnforcement does the test one time. It returns nil if the cluster
// lets warren connect to a sandbox and refuses a connection with no
// permission. It returns errUnreachable or errNotEnforced if one of the two
// steps gives the wrong result, and a different error if the test was not
// possible.
func (m *Manager) checkEnforcement(ctx context.Context) error {
	pods := m.cfg.Client.CoreV1().Pods(m.cfg.Namespace)
	policies := m.cfg.Client.NetworkingV1().NetworkPolicies(m.cfg.Namespace)
	zero := int64(0)
	now := metav1.DeleteOptions{GracePeriodSeconds: &zero}
	// Remove the test pods of previous runs.
	if old, err := pods.List(ctx, metav1.ListOptions{LabelSelector: LabelCanary + "=true"}); err == nil {
		for i := range old.Items {
			// A list of the simulated cluster of the tests ignores the
			// selector, thus examine the label again.
			if p := &old.Items[i]; p.Labels[LabelCanary] == "true" && p.Labels[LabelManagedBy] == "" {
				_ = pods.Delete(ctx, p.Name, now)
				_ = policies.Delete(ctx, p.Name, metav1.DeleteOptions{})
			}
		}
	}

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	name := "warren-canary-" + hex.EncodeToString(suffix)
	cleanup := func(delete func(c context.Context)) {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		delete(c)
	}

	// The policy of the test pod is the policy of a sandbox that can open no
	// connection: the same code makes it.
	var closed policy.Policy
	closed.Network.Egress = policy.EgressNone
	madePolicy, err := policies.Create(ctx, m.buildNetworkPolicy(name, map[string]string{LabelSandbox: name}, closed, nil), metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("make the network policy of the test pod: %w", err)
	}
	removePolicy := func(c context.Context) error {
		err := policies.Delete(c, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &madePolicy.UID}})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	defer cleanup(func(c context.Context) { _ = removePolicy(c) })

	made, err := pods.Create(ctx, m.buildCanary(name), metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("make the test pod: %w", err)
	}
	defer cleanup(func(c context.Context) {
		_ = pods.Delete(c, name, metav1.DeleteOptions{GracePeriodSeconds: &zero, Preconditions: &metav1.Preconditions{UID: &made.UID}})
	})

	// Wait until the server of the test pod listens.
	waitCtx, cancel := context.WithTimeout(ctx, canaryStartTimeout)
	defer cancel()
	var address string
	for address == "" {
		pod, err := pods.Get(waitCtx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read the test pod: %w", err)
		}
		switch {
		case podTerminal(pod):
			return fmt.Errorf("the test pod stopped (%s)", pod.Status.Reason)
		case podReady(pod):
			address = net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(m.cfg.Port))
		default:
			select {
			case <-waitCtx.Done():
				return errors.New("the test pod did not become ready. Examine the image of the sandboxes (--sandbox-image): the test pod uses it with the default command")
			case <-time.After(m.cfg.CanaryPoll):
			}
		}
	}
	pause := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.cfg.CanaryPause):
			return nil
		}
	}

	// Step 1: the policy of the test pod permits warren. A network plugin
	// can need some seconds to apply the policy to a new pod.
	for attempt := 1; ; attempt++ {
		err := m.cfg.Dial(ctx, address)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt >= canaryAttempts {
			return fmt.Errorf("%w: %v", errUnreachable, err)
		}
		if err := pause(); err != nil {
			return err
		}
	}

	// Step 2: with no policy of its own, the test pod must refuse warren.
	if err := removePolicy(ctx); err != nil {
		return fmt.Errorf("delete the network policy of the test pod: %w", err)
	}
	start := m.now()
	for attempt := 1; ; attempt++ {
		if err := m.cfg.Dial(ctx, address); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if late := m.now().Sub(start); attempt > 1 && late >= canarySlow {
				m.cfg.Logger.Warn("the network plugin of the cluster needed some seconds to apply a change of the network policies. "+
					"If it is also slow for a new pod, a sandbox has no network isolation in its first seconds. Examine the configuration of the network plugin.",
					"seconds", int(late.Seconds()))
			}
			return nil
		}
		if attempt >= canaryAttempts {
			return errNotEnforced
		}
		if err := pause(); err != nil {
			return err
		}
	}
}

// examineEnforcement does the test and records the result.
func (m *Manager) examineEnforcement(ctx context.Context) {
	if m.cfg.Network.SkipEnforcementCheck {
		m.enforcement.Store(enforcementProven)
		return
	}
	err := m.checkEnforcement(ctx)
	switch {
	case err == nil:
		if m.enforcement.Swap(enforcementProven) != enforcementProven {
			m.cfg.Logger.Info("the cluster applies network policies", "log", "audit")
		}
	case errors.Is(err, errUnreachable):
		// The result of a previous test stays.
		m.networkProblem.Store(err.Error())
		m.cfg.Logger.Error("warren cannot connect to a test pod whose network policy permits warren. No request could get to a sandbox. "+
			"Examine the configuration: the namespace and the pod labels of warren (--warren-namespace, --warren-pod-labels) must be those of the warren pods, "+
			"and the pod of warren must not use the network of its node.",
			"namespace", m.cfg.Network.WarrenNamespace, "labels", m.cfg.Network.WarrenPodLabels, "error", err.Error())
	case errors.Is(err, errNotEnforced):
		m.enforcement.Store(enforcementAbsent)
		m.cfg.Logger.Error("THE CLUSTER DOES NOT APPLY NETWORK POLICIES. A pod with no permission could connect to a pod in the sandbox namespace. "+
			"Warren starts no sandbox. Install a network plugin that applies NetworkPolicy objects, or turn that function on.", "log", "audit")
	case ctx.Err() != nil:
	default:
		// The result of a previous test stays. With no previous test, warren
		// starts no sandbox.
		m.networkProblem.Store(err.Error())
		m.cfg.Logger.Warn("warren cannot test if the cluster applies network policies", "error", err.Error())
	}
}
