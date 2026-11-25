package kube

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/h3xcat/warren/internal/backend"
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

	// AnnotationContext is the context ID, for example "user:1234".
	AnnotationContext = prefix + "/context-id"
	// AnnotationCreatedBy is the ID of the user that started the sandbox.
	AnnotationCreatedBy = prefix + "/created-by"
	// AnnotationActivity is the time of the last use, in RFC 3339 form.
	AnnotationActivity = prefix + "/last-activity"

	secretKeyName  = "api-key"
	containerName  = "terminal"
	terminalPort   = 8000
	keyRandomBytes = 32
)

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

func (m *Manager) buildSecret(name string, req backend.Request, key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.cfg.Namespace,
			Labels:    labels(req),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{secretKeyName: []byte(key)},
	}
}

func (m *Manager) buildPod(name string, req backend.Request) *corev1.Pod {
	no := false
	grace := int64(10)
	port := intstr.FromInt32(terminalPort)
	probe := func() *corev1.ProbeHandler {
		// The health route of Open Terminal accepts only GET and needs no
		// key (docs/open-terminal-api.md, section 6).
		return &corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: port}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.cfg.Namespace,
			Labels:    labels(req),
			Annotations: map[string]string{
				AnnotationContext:   req.Ref.ContextID(),
				AnnotationCreatedBy: req.UserID,
				AnnotationActivity:  m.now().UTC().Format(timeFormat),
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			TerminationGracePeriodSeconds: &grace,
			// A sandbox gets no credential for the Kubernetes API and no
			// data about other services.
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			Containers: []corev1.Container{{
				Name:  containerName,
				Image: m.cfg.Image,
				// The address and the port are on the command line. If
				// not, a configuration file in the home directory can
				// change them (docs/open-terminal-api.md, section 2.1).
				Args: []string{"run", "--host", "0.0.0.0", "--port", strconv.Itoa(terminalPort)},
				Ports: []corev1.ContainerPort{{
					Name: "http", ContainerPort: terminalPort, Protocol: corev1.ProtocolTCP,
				}},
				Env: []corev1.EnvVar{
					{Name: "OPEN_TERMINAL_API_KEY", ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: name},
							Key:                  secretKeyName,
						},
					}},
					// Ignore the configuration files, which the user can write.
					{Name: "OPEN_TERMINAL_ISOLATED", Value: "true"},
					// One pod for each scope key gives the isolation.
					{Name: "OPEN_TERMINAL_MULTI_USER", Value: "false"},
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
}
