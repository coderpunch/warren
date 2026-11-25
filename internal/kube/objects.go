package kube

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/policy"
)

// Warren keeps its state in these labels and annotations. It has no other
// database.
const (
	prefix = "warren.h3xcat.github.io"

	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "warren"
	// LabelTemplate is the policy ID.
	LabelTemplate = prefix + "/template"
	// LabelScope is user, chat, model, or shared.
	LabelScope = prefix + "/scope"
	// LabelOwner is a digest of the ID of the user that started the
	// sandbox. A label cannot contain all characters of a user ID.
	LabelOwner = prefix + "/owner"
	// LabelSandbox is the name of the sandbox. A Secret and a network
	// policy have this label, because their names have a random part.
	LabelSandbox = prefix + "/sandbox"
	// LabelStart is on a pod. Its value is the name of the Secret and of
	// the network policy of that start of the sandbox. The network policy
	// selects its pod with this label.
	LabelStart = prefix + "/start"

	// AnnotationContext is the context ID, for example "user:1234".
	AnnotationContext = prefix + "/context-id"
	// AnnotationCreatedBy is the ID of the user that started the sandbox.
	AnnotationCreatedBy = prefix + "/created-by"
	// AnnotationActivity is the time of the last use, in RFC 3339 form.
	AnnotationActivity = prefix + "/last-activity"
	// AnnotationIdleTimeout is the idle time of the policy at the start of
	// the pod. A pod without it uses the idle time of warren.
	AnnotationIdleTimeout = prefix + "/idle-timeout"
	// AnnotationNetwork is on a network policy. It has the network settings
	// of the sandbox policy from which warren made the network policy.
	AnnotationNetwork = prefix + "/network"
	// AnnotationUnusedSince is on a volume. It is the time at which warren
	// found that no sandbox uses the volume, in RFC 3339 form.
	AnnotationUnusedSince = prefix + "/unused-since"

	secretKeyName  = "api-key"
	keyVariable    = "OPEN_TERMINAL_API_KEY"
	containerName  = "terminal"
	terminalPort   = 8000
	keyRandomBytes = 32

	// homePath is the home directory in all images of Open Terminal
	// (docs/open-terminal-api.md, section 8.1).
	homePath   = "/home/user"
	homeVolume = "home"
	tmpVolume  = "tmp"
	// volumeGroup is the group that owns the files of a volume. The process
	// of the sandbox is a member of this group, thus it can write to the
	// volume with each user ID.
	volumeGroup = 1001
	// defaultUser is the user ID of the processes of a sandbox, for a
	// policy that sets no user ID. It is the user of the default image. The
	// server of Open Terminal operates with each user ID.
	defaultUser = 1001

	// defaultDenyName is the name of the network policy that refuses all
	// connections of all pods in the sandbox namespace.
	defaultDenyName = "warren-default-deny"
)

// defaultCommand starts the server of the default image. It does not use
// the entry point of the image, and it does not look for a program in PATH:
// the home directory of a sandbox is in PATH, and its user can write to it.
// The option -I makes Python ignore the modules of the home directory and
// the PYTHON variables.
var defaultCommand = []string{"/usr/bin/tini", "--", "/usr/local/bin/python3", "-I", "-u", "-m", "open_terminal"}

// managedSelector selects the objects that warren made.
const managedSelector = LabelManagedBy + "=" + ManagedByValue

// ownerDigest makes a label value from a user ID.
func ownerDigest(userID string) string {
	sum := sha256.Sum256([]byte(userID))
	return hex.EncodeToString(sum[:])[:32]
}

// newKey makes the random API key of one sandbox.
func newKey() (string, error) {
	b := make([]byte, keyRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func labels(req backend.Request) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelTemplate:  req.Ref.TemplateID,
		LabelScope:     string(req.Ref.Kind),
		LabelOwner:     ownerDigest(req.UserID),
	}
}

// newSecretName makes the name of the Secret for one start of a sandbox.
// Each start has a different Secret. Thus two starts at the same time never
// change the Secret of each other, and the pod that Kubernetes accepts has
// the key of its own start.
func newSecretName(sandbox string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return sandbox + "-" + hex.EncodeToString(b), nil
}

// usesVolume reports whether a pod has its home directory on a volume that
// stays after the pod.
func usesVolume(pod *corev1.Pod) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			return true
		}
	}
	return false
}

// podSecretName returns the name of the Secret that has the key of a pod.
func podSecretName(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		for _, e := range c.Env {
			if e.Name == keyVariable && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				return e.ValueFrom.SecretKeyRef.Name
			}
		}
	}
	return ""
}

// containerRestarts returns the number of restarts of the sandbox container.
func containerRestarts(pod *corev1.Pod) int32 {
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == containerName {
			return c.RestartCount
		}
	}
	return 0
}

func (m *Manager) buildSecret(sandbox, secretName string, req backend.Request, key string) *corev1.Secret {
	l := labels(req)
	l[LabelSandbox] = sandbox
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: m.cfg.Namespace,
			Labels:    l,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{secretKeyName: []byte(key)},
	}
}

// buildVolume makes the volume that keeps the home directory of a sandbox.
// The volume has the name of the sandbox, and no pod owns it. Thus it stays
// when the pod goes away.
func (m *Manager) buildVolume(name string, req backend.Request, pol policy.Policy) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.cfg.Namespace,
			Labels:    labels(req),
			Annotations: map[string]string{
				AnnotationContext:   req.Ref.ContextID(),
				AnnotationCreatedBy: req.UserID,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: pol.Storage.Size.DeepCopy()},
			},
		},
	}
	if pol.Storage.StorageClassName != "" {
		class := pol.Storage.StorageClassName
		pvc.Spec.StorageClassName = &class
	}
	return pvc
}

// reservedVariables are the variables that warren sets. The policy file
// cannot set them (internal/policy). This is a second protection.
var reservedVariables = map[string]bool{keyVariable: true, "HOME": true, "OPEN_TERMINAL_ISOLATED": true, "OPEN_TERMINAL_MULTI_USER": true}

func (m *Manager) buildPod(name, secretName string, req backend.Request, pol policy.Policy) *corev1.Pod {
	no, yes := false, true
	grace := int64(10)
	group := int64(volumeGroup)
	user := int64(defaultUser)
	if pol.RunAsUser != nil {
		user = *pol.RunAsUser
	}
	onMismatch := corev1.FSGroupChangeOnRootMismatch
	port := intstr.FromInt32(terminalPort)
	probe := func() *corev1.ProbeHandler {
		// The health route of Open Terminal accepts only GET and needs no
		// key (docs/open-terminal-api.md, section 6).
		return &corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: port}}
	}

	image := pol.Image
	if image == "" {
		image = m.cfg.Image
	}
	annotations := map[string]string{
		AnnotationContext:   req.Ref.ContextID(),
		AnnotationCreatedBy: req.UserID,
		AnnotationActivity:  m.now().UTC().Format(timeFormat),
	}
	if pol.IdleTimeout != nil {
		annotations[AnnotationIdleTimeout] = pol.IdleTimeout.String()
	}

	env := []corev1.EnvVar{
		{Name: keyVariable, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  secretKeyName,
			},
		}},
		// The home directory does not depend on the image and on the user
		// ID. A user ID with no entry in the user file of the image has no
		// home directory.
		{Name: "HOME", Value: homePath},
		// Ignore the configuration files, which the user can write.
		{Name: "OPEN_TERMINAL_ISOLATED", Value: "true"},
		// One pod for each scope key gives the isolation.
		{Name: "OPEN_TERMINAL_MULTI_USER", Value: "false"},
	}
	names := make([]string, 0, len(pol.Env))
	for n := range pol.Env {
		if !reservedVariables[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		// Kubernetes replaces "$(NAME)" in a value with a variable that is
		// earlier in the list, also with the key of the sandbox. "$$" is
		// the escape for one "$", thus the value stays as the file has it.
		env = append(env, corev1.EnvVar{Name: n, Value: strings.ReplaceAll(pol.Env[n], "$", "$$")})
	}

	resources := *pol.Resources.DeepCopy()
	home := corev1.Volume{Name: homeVolume}
	if pol.Persistent() {
		home.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}
	} else {
		size := pol.Storage.Size.DeepCopy()
		home.EmptyDir = &corev1.EmptyDirVolumeSource{SizeLimit: &size}
		// The limit for ephemeral storage of a pod includes its emptyDir
		// volumes. The home directory gets its size in addition to the
		// limit of the policy. If not, the smaller value of the two is
		// the real limit.
		if limit, ok := resources.Limits[corev1.ResourceEphemeralStorage]; ok {
			limit.Add(size)
			resources.Limits[corev1.ResourceEphemeralStorage] = limit
		}
	}

	podLabels := labels(req)
	podLabels[LabelSandbox] = name
	podLabels[LabelStart] = secretName
	command := defaultCommand
	if pol.Command != nil {
		command = pol.Command
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   m.cfg.Namespace,
			Labels:      podLabels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			TerminationGracePeriodSeconds: &grace,
			// A sandbox gets no credential for the Kubernetes API and no
			// data about other services.
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			NodeSelector:                 pol.NodeSelector,
			Tolerations:                  pol.Tolerations,
			// These settings are the "restricted" Pod Security Standard.
			// They are the same for all sandboxes. A policy cannot make
			// them weaker.
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &yes,
				// The user and the group are numbers, thus they do not
				// depend on the image. The group is not the group 0 of the
				// node. A container runtime does not accept a group with no
				// user.
				RunAsUser:           &user,
				RunAsGroup:          &group,
				FSGroup:             &group,
				FSGroupChangePolicy: &onMismatch,
				SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Volumes: []corev1.Volume{
				home,
				// The server writes response bodies and other temporary
				// files (docs/open-terminal-api.md, section 8.4).
				{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			Containers: []corev1.Container{{
				Name:            containerName,
				Image:           image,
				ImagePullPolicy: pol.ImagePullPolicy,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					Privileged:               &no,
					// The sandbox can write only to its home directory and
					// to the temporary directory.
					ReadOnlyRootFilesystem: &yes,
					Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Command: command,
				// The address and the port are on the command line. If
				// not, a configuration file in the home directory can
				// change them (docs/open-terminal-api.md, section 2.1).
				Args: []string{"run", "--host", "0.0.0.0", "--port", strconv.Itoa(terminalPort)},
				// The file routes use the working directory as the home
				// directory (docs/open-terminal-api.md, section 8.4).
				WorkingDir: homePath,
				Ports: []corev1.ContainerPort{{
					Name: "http", ContainerPort: terminalPort, Protocol: corev1.ProtocolTCP,
				}},
				Env:       env,
				Resources: resources,
				VolumeMounts: []corev1.VolumeMount{
					{Name: homeVolume, MountPath: homePath},
					{Name: tmpVolume, MountPath: "/tmp"},
				},
				// The server does not listen before its setup is complete.
				StartupProbe: &corev1.Probe{
					ProbeHandler:     *probe(),
					PeriodSeconds:    1,
					FailureThreshold: 300,
					TimeoutSeconds:   2,
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:     *probe(),
					PeriodSeconds:    5,
					FailureThreshold: 3,
					TimeoutSeconds:   2,
				},
			}},
		},
	}
	for _, s := range pol.ImagePullSecrets {
		pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: s})
	}
	if pol.RuntimeClassName != "" {
		class := pol.RuntimeClassName
		pod.Spec.RuntimeClassName = &class
	}
	if pol.UserNamespace != nil && *pol.UserNamespace {
		// The pod gets its own range of user IDs on the node.
		pod.Spec.HostUsers = &no
	}
	return pod
}
