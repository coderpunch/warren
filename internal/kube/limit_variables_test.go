package kube

import (
	"math"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// limitNames are the four limit variables in the sequence of the pod.
var limitNames = []string{"OPEN_TERMINAL_CPU_LIMIT", "OPEN_TERMINAL_CPU_COUNT", "OPEN_TERMINAL_MEMORY_LIMIT", "OPEN_TERMINAL_MEMORY_BYTES"}

// variables returns the variables of a container by name. A name that
// occurs two times is an error.
func variables(t *testing.T, c corev1.Container) map[string]corev1.EnvVar {
	t.Helper()
	out := map[string]corev1.EnvVar{}
	for _, v := range c.Env {
		if _, dup := out[v.Name]; dup {
			t.Errorf("the variable %s occurs two times", v.Name)
		}
		out[v.Name] = v
	}
	return out
}

// The sandbox gets its cpu limit and its memory limit in four variables
// (docs/policies.md, section 6.1). Each case goes from the text of a policy
// file to the pod that the API server has.
func TestLimitVariablesFollowThePolicy(t *testing.T) {
	cases := []struct {
		id, resources string
		// want has the values in the sequence of limitNames.
		want [4]string
	}{
		// The limits of warren apply to a policy that sets none.
		{"no-limits", "", [4]string{"2", "2", "2Gi", "2147483648"}},
		{"part", "resources: {limits: {cpu: 500m, memory: 512Mi}}", [4]string{"500m", "1", "512Mi", "536870912"}},
		// A decimal number and the milli form are the same limit.
		{"decimal", "resources: {limits: {cpu: 0.5}}", [4]string{"500m", "1", "2Gi", "2147483648"}},
		// The count is the limit rounded up. A decimal unit stays decimal.
		{"rounded-up", "resources: {limits: {cpu: 1500m, memory: 1G}}", [4]string{"1500m", "2", "1G", "1000000000"}},
		// The value has the largest unit that loses no precision.
		{"largest-unit", "resources: {limits: {cpu: 1000m, memory: 1024Mi}}", [4]string{"1", "1", "1Gi", "1073741824"}},
		{"minimum", "resources: {requests: {cpu: 1m, memory: 16Mi}, limits: {cpu: 1m, memory: 16Mi}}", [4]string{"1m", "1", "16Mi", "16777216"}},
		{"no-unit", "resources: {limits: {cpu: 3, memory: 1500000000}}", [4]string{"3", "3", "1500M", "1500000000"}},
		// Kubernetes keeps the exponent form of a cpu value.
		{"exponent", "resources: {limits: {cpu: \"1e3\"}}", [4]string{"1e3", "1000", "2Gi", "2147483648"}},
	}
	file := "policies:\n"
	for _, c := range cases {
		file += "  - {id: " + c.id + ", name: Limits, scope: user, storage: {type: ephemeral}"
		if c.resources != "" {
			file += ", " + c.resources
		}
		file += "}\n"
	}
	e := newTestEnv(t, nil)
	e.usePolicies(file)

	for i, c := range cases {
		// Each case has a different user. Thus the sandbox limit of a user
		// has no effect.
		req := templateRef(c.id, "user-"+strconv.Itoa(i))
		e.acquire(req).Release()
		var container corev1.Container
		found := false
		for _, pod := range e.pods() {
			if pod.Name == req.Ref.Name() {
				container, found = pod.Spec.Containers[0], true
			}
		}
		if !found {
			t.Fatalf("%s: the API server has no pod", c.id)
		}
		env := variables(t, container)
		for j, name := range limitNames {
			v := env[name]
			if v.Value != c.want[j] || v.ValueFrom != nil {
				t.Errorf("%s: %s = %q, want %q", c.id, name, v.Value, c.want[j])
			}
		}
		// The two LIMIT variables are the limits of the container.
		for name, res := range map[string]corev1.ResourceName{limitNames[0]: corev1.ResourceCPU, limitNames[2]: corev1.ResourceMemory} {
			got, err := resource.ParseQuantity(env[name].Value)
			if err != nil {
				t.Errorf("%s: %s = %q is not a Kubernetes quantity: %v", c.id, name, env[name].Value, err)
				continue
			}
			limit, ok := container.Resources.Limits[res]
			if !ok || got.Cmp(limit) != 0 {
				t.Errorf("%s: %s = %q, and the limit of the container is %s", c.id, name, env[name].Value, limit.String())
			}
		}
		// MEMORY_BYTES is the same limit as a number.
		bytes, err := strconv.ParseInt(env[limitNames[3]].Value, 10, 64)
		if limit := container.Resources.Limits[corev1.ResourceMemory]; err != nil || limit.CmpInt64(bytes) != 0 {
			t.Errorf("%s: %s = %q, and the limit of the container is %s", c.id, limitNames[3], env[limitNames[3]].Value, limit.String())
		}
	}
}

// The start code must not let a policy replace the limit variables. The
// policy file cannot set them (internal/policy). This policy value does not
// come from the file.
func TestPodBuilderIgnoresLimitVariablesOfAPolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("tuned", "alice")
	pol := e.policy(req)
	pol.Env = map[string]string{"KEPT": "yes"}
	for _, name := range limitNames {
		pol.Env[name] = "64"
	}
	env := variables(t, e.m.buildPod(req.Ref.Name(), "secret-1", req, pol).Spec.Containers[0])
	// The policy "tuned" has the limits 1500m and 3Gi.
	want := map[string]string{limitNames[0]: "1500m", limitNames[1]: "2", limitNames[2]: "3Gi", limitNames[3]: "3221225472", "KEPT": "yes"}
	for name, w := range want {
		if env[name].Value != w {
			t.Errorf("%s = %q, want %q", name, env[name].Value, w)
		}
	}
}

// A limit that the list does not have gets no variables. A cpu limit that is
// too large for the number type must not give a wrong count.
func TestLimitVariablesOfAnIncompleteList(t *testing.T) {
	text := func(env []corev1.EnvVar) string {
		var parts []string
		for _, v := range env {
			parts = append(parts, v.Name+"="+v.Value)
		}
		return strings.Join(parts, ",")
	}
	if got := limitVariables(nil); len(got) != 0 {
		t.Errorf("no limits: %s", text(got))
	}
	storage := corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}
	if got := limitVariables(storage); len(got) != 0 {
		t.Errorf("only a storage limit: %s", text(got))
	}
	cpu := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}
	if got := text(limitVariables(cpu)); got != "OPEN_TERMINAL_CPU_LIMIT=250m,OPEN_TERMINAL_CPU_COUNT=1" {
		t.Errorf("only a cpu limit: %s", got)
	}
	memory := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}
	if got := text(limitVariables(memory)); got != "OPEN_TERMINAL_MEMORY_LIMIT=64Mi,OPEN_TERMINAL_MEMORY_BYTES=67108864" {
		t.Errorf("only a memory limit: %s", got)
	}
	// The policy file has no maximum for cpu.
	maximum := strconv.FormatInt(math.MaxInt64, 10)
	for _, value := range []string{"9223372036854775807", "9223372036854775808", "100000000000000000000"} {
		huge := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(value)}
		if got := limitVariables(huge); len(got) != 2 || got[1].Value != maximum {
			t.Errorf("cpu %s: %s, want the count %s", value, text(got), maximum)
		}
	}
}
