package policy

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/h3xcat/warren/internal/scope"
)

const validFile = `
policies:
  - id: default
    name: Default sandbox
    scope: user
  - id: team-shared
    name: Team workspace
    scope: shared
  - id: by-chat
    name: One for each chat
    scope: chat
`

func TestParseValid(t *testing.T) {
	set, err := Parse([]byte(validFile))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := set.Get("team-shared")
	if !ok || p.Name != "Team workspace" || p.Scope != scope.KindShared {
		t.Errorf("Get(team-shared) = %+v, %v", p, ok)
	}
	if _, ok := set.Get("absent"); ok {
		t.Error("Get returned a policy that does not exist")
	}
	// A template ID is an exact match. No prefix and no different case.
	for _, id := range []string{"Default", "default ", "defaul", "default/x", ""} {
		if _, ok := set.Get(id); ok {
			t.Errorf("Get(%q) matched a policy", id)
		}
	}
	var ids []string
	for _, p := range set.List() {
		ids = append(ids, p.ID)
	}
	if got := strings.Join(ids, ","); got != "by-chat,default,team-shared" {
		t.Errorf("List order = %s", got)
	}
}

func TestParseAcceptsQuotedValues(t *testing.T) {
	set, err := Parse([]byte("policies:\n  - {id: \"007\", name: \"No\", scope: \"user\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := set.Get("007"); !ok || p.Name != "No" {
		t.Errorf("Get(007) = %+v, %v", p, ok)
	}
	// A word such as "yes" or "No" stays the same text. It does not
	// become "true" or "false".
	set, err = Parse([]byte("policies:\n  - {id: yes, name: No, scope: user}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := set.Get("yes"); !ok || p.Name != "No" {
		t.Errorf("Get(yes) = %+v, %v", p, ok)
	}
}

func TestParseRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"no policies":    "policies: []\n",
		"empty file":     "",
		"unknown field":  "policies:\n  - {id: a, name: A, scope: user, privileged: true}\n",
		"unknown top":    "policies:\n  - {id: a, name: A, scope: user}\nextra: 1\n",
		"duplicate ID":   "policies:\n  - {id: a, name: A, scope: user}\n  - {id: a, name: B, scope: chat}\n",
		"upper-case ID":  "policies:\n  - {id: Default, name: A, scope: user}\n",
		"ID with slash":  "policies:\n  - {id: a/b, name: A, scope: user}\n",
		"ID with colon":  "policies:\n  - {id: 'a:b', name: A, scope: user}\n",
		"empty ID":       "policies:\n  - {id: '', name: A, scope: user}\n",
		"no name":        "policies:\n  - {id: a, scope: user}\n",
		"no scope":       "policies:\n  - {id: a, name: A}\n",
		"unknown scope":  "policies:\n  - {id: a, name: A, scope: team}\n",
		"long ID":        "policies:\n  - {id: " + strings.Repeat("a", 64) + ", name: A, scope: user}\n",
		"duplicate keys": "policies:\n  - {id: a, name: A, scope: user, scope: shared}\n",
		// YAML changes these values to a different string with no error.
		"number as ID":    "policies:\n  - {id: 007, name: A, scope: user}\n",
		"hex number ID":   "policies:\n  - {id: 0x1f, name: A, scope: user}\n",
		"boolean as ID":   "policies:\n  - {id: true, name: A, scope: user}\n",
		"boolean as name": "policies:\n  - {id: a, name: false, scope: user}\n",
		"float as ID":     "policies:\n  - {id: 1e3, name: A, scope: user}\n",
		"null as name":    "policies:\n  - {id: a, name: ~, scope: user}\n",
		"list as name":    "policies:\n  - {id: a, name: [A], scope: user}\n",
		"two documents":   "policies:\n  - {id: a, name: A, scope: user}\n---\npolicies:\n  - {id: b, name: B, scope: user}\n",
		"upper-case key":  "policies:\n  - {ID: a, name: A, scope: user}\n",
		"upper-case top":  "Policies:\n  - {id: a, name: A, scope: user}\n",
		"policy is text":  "policies:\n  - just-text\n",
		"null policy":     "policies:\n  - ~\n",
		"top is a list":   "- {id: a, name: A, scope: user}\n",
		"control in name": "policies:\n  - {id: a, name: \"A\\tB\", scope: user}\n",
		"space in name":   "policies:\n  - {id: a, name: \" A\", scope: user}\n",
	}
	for name, content := range cases {
		if _, err := Parse([]byte(content)); err == nil {
			t.Errorf("%s: Parse gave no error", name)
		}
	}
}

const fullFile = `
defaults:
  image: registry.example/terminal:1
  resources:
    limits: {memory: 1Gi}
  env:
    A: from-defaults
    B: from-defaults
  nodeSelector: {pool: sandboxes}
  storage: {size: 5Gi, retention: 30d}
  idleTimeout: 10m
policies:
  - id: personal
    name: Personal
    scope: user
    users: ["u-1", "u-2"]
    image: registry.example/other@sha256:0123
    imagePullPolicy: Always
    imagePullSecrets: [pull-secret]
    runtimeClassName: gvisor
    resources:
      requests: {cpu: 250m, memory: "512Mi"}
      limits: {cpu: 0.5, ephemeral-storage: 4Gi}
    storage: {storageClassName: fast, retention: 0}
    env: {B: from-policy, OPEN_TERMINAL_EXECUTE_TIMEOUT: "120"}
    idleTimeout: 0
    tolerations:
      - {key: dedicated, operator: Equal, value: sandboxes, effect: NoExecute, tolerationSeconds: 60}
      - {operator: Exists}
  - id: by-chat
    name: Chat
    scope: chat
  - id: chat-kept
    name: Chat with a volume
    scope: chat
    storage: {type: persistent}
  - id: by-model
    name: Model
    scope: model
  - id: shared
    name: Shared
    scope: shared
    storage: {type: ephemeral}
`

func mustGet(t *testing.T, set *Set, id string) Policy {
	t.Helper()
	p, ok := set.Get(id)
	if !ok {
		t.Fatalf("no policy %q", id)
	}
	return p
}

func quantityOf(t *testing.T, list corev1.ResourceList, name corev1.ResourceName) string {
	t.Helper()
	q, ok := list[name]
	if !ok {
		return "<absent>"
	}
	return q.String()
}

func TestParseFullPolicy(t *testing.T) {
	set, err := Parse([]byte(fullFile))
	if err != nil {
		t.Fatal(err)
	}
	p := mustGet(t, set, "personal")
	if p.Image != "registry.example/other@sha256:0123" || p.ImagePullPolicy != corev1.PullAlways ||
		len(p.ImagePullSecrets) != 1 || p.ImagePullSecrets[0] != "pull-secret" || p.RuntimeClassName != "gvisor" {
		t.Errorf("image settings = %+v", p.Spec)
	}
	// The file gives the text 0.5 for the CPU limit. Kubernetes shows it as 500m.
	want := map[string]string{
		"requests.cpu": "250m", "requests.memory": "512Mi", "requests.ephemeral-storage": "100Mi",
		"limits.cpu": "500m", "limits.memory": "1Gi", "limits.ephemeral-storage": "4Gi",
	}
	for key, w := range want {
		kind, name, _ := strings.Cut(key, ".")
		list := p.Resources.Requests
		if kind == "limits" {
			list = p.Resources.Limits
		}
		if got := quantityOf(t, list, corev1.ResourceName(name)); got != w {
			t.Errorf("%s = %s, want %s", key, got, w)
		}
	}
	if !p.Persistent() || p.Storage.Size.String() != "5Gi" || p.Storage.StorageClassName != "fast" {
		t.Errorf("storage = %+v", p.Storage)
	}
	// The policy changes the retention of the defaults back to "keep".
	if p.Storage.Retention == nil || *p.Storage.Retention != 0 {
		t.Errorf("retention = %v, want 0", p.Storage.Retention)
	}
	if p.IdleTimeout == nil || *p.IdleTimeout != 0 {
		t.Errorf("idleTimeout = %v, want 0", p.IdleTimeout)
	}
	if p.Env["A"] != "from-defaults" || p.Env["B"] != "from-policy" || p.Env["OPEN_TERMINAL_EXECUTE_TIMEOUT"] != "120" {
		t.Errorf("env = %v", p.Env)
	}
	if p.NodeSelector["pool"] != "sandboxes" {
		t.Errorf("nodeSelector = %v", p.NodeSelector)
	}
	if len(p.Tolerations) != 2 || p.Tolerations[0].TolerationSeconds == nil || *p.Tolerations[0].TolerationSeconds != 60 ||
		p.Tolerations[1].Operator != corev1.TolerationOpExists {
		t.Errorf("tolerations = %+v", p.Tolerations)
	}
	if !p.Allows("u-1") || !p.Allows("u-2") || p.Allows("u-3") || p.Allows("") || p.Allows("U-1") || p.Allows("u-1 ") {
		t.Error("the user list does not select the correct users")
	}

	chat := mustGet(t, set, "by-chat")
	if chat.Persistent() || chat.Storage.Size.String() != "5Gi" {
		t.Errorf("chat storage = %+v, want ephemeral with the size of the defaults", chat.Storage)
	}
	if chat.Image != "registry.example/terminal:1" || chat.Env["B"] != "from-defaults" {
		t.Errorf("chat policy did not get the defaults: %+v", chat.Spec)
	}
	if chat.Storage.Retention == nil || *chat.Storage.Retention != 720*time.Hour || *chat.IdleTimeout != 10*time.Minute {
		t.Errorf("chat times = %v, %v", chat.Storage.Retention, chat.IdleTimeout)
	}
	if !chat.Allows("each-user") {
		t.Error("a policy with no user list must permit all users")
	}
	if !mustGet(t, set, "chat-kept").Persistent() {
		t.Error("a chat policy with storage.type persistent is not persistent")
	}
	if mustGet(t, set, "by-model").Persistent() {
		t.Error("a model policy is persistent by default")
	}
	if mustGet(t, set, "shared").Persistent() {
		t.Error("a shared policy with storage.type ephemeral is persistent")
	}
}

func TestBuiltInDefaults(t *testing.T) {
	set, err := Parse([]byte(validFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range set.List() {
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
			if _, ok := p.Resources.Limits[name]; !ok {
				t.Errorf("policy %s has no limit for %s", p.ID, name)
			}
			if _, ok := p.Resources.Requests[name]; !ok {
				t.Errorf("policy %s has no request for %s", p.ID, name)
			}
		}
		if p.Storage.Size == nil || p.Storage.Size.Sign() <= 0 {
			t.Errorf("policy %s has no storage size", p.ID)
		}
		if p.Image != "" || p.IdleTimeout != nil {
			t.Errorf("policy %s: image and idle time must stay empty for the defaults of warren", p.ID)
		}
		if want := p.Scope != scope.KindChat; p.Persistent() != want {
			t.Errorf("policy %s (scope %s): persistent = %v", p.ID, p.Scope, p.Persistent())
		}
	}
}

// A user can make very many chats, and Open WebUI does not examine each
// model ID. A volume for each of them is not the default, and it must have
// an end.
func TestVolumesOfChatsAndModelsNeedAnEnd(t *testing.T) {
	file := func(scope, storage string) string {
		return "policies:\n  - id: a\n    name: A\n    scope: " + scope + "\n" + storage
	}
	for _, kind := range []string{"chat", "model"} {
		set, err := Parse([]byte(file(kind, "")))
		if err != nil {
			t.Fatal(err)
		}
		if mustGet(t, set, "a").Persistent() {
			t.Errorf("scope %s: persistent storage is the default", kind)
		}
		if _, err := Parse([]byte(file(kind, "    storage: {type: persistent}\n"))); err == nil {
			t.Errorf("scope %s: persistent storage with no retention time gave no error", kind)
		}
		if _, err := Parse([]byte(file(kind, "    storage: {type: persistent, retention: 0}\n"))); err == nil {
			t.Errorf("scope %s: persistent storage with the retention time 0 gave no error", kind)
		}
		set, err = Parse([]byte(file(kind, "    storage: {type: persistent, retention: 168h}\n")))
		if err != nil || !mustGet(t, set, "a").Persistent() {
			t.Errorf("scope %s: persistent storage with a retention time: %v", kind, err)
		}
	}
	for _, kind := range []string{"user", "shared"} {
		set, err := Parse([]byte(file(kind, "")))
		if err != nil || !mustGet(t, set, "a").Persistent() {
			t.Errorf("scope %s: persistent storage must be the default (%v)", kind, err)
		}
	}
}

func TestDefaultRequestIsNotMoreThanTheLimit(t *testing.T) {
	set, err := Parse([]byte("policies:\n  - id: small\n    name: Small\n    scope: user\n    resources:\n      limits: {cpu: 50m, memory: 64Mi}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := mustGet(t, set, "small")
	if got := quantityOf(t, p.Resources.Requests, corev1.ResourceCPU); got != "50m" {
		t.Errorf("cpu request = %s, want 50m", got)
	}
	if got := quantityOf(t, p.Resources.Requests, corev1.ResourceMemory); got != "64Mi" {
		t.Errorf("memory request = %s, want 64Mi", got)
	}
}

func TestPoliciesDoNotShareData(t *testing.T) {
	set, err := Parse([]byte("defaults:\n  env: {A: x}\n  nodeSelector: {k: v}\npolicies:\n  - {id: a, name: A, scope: user}\n  - {id: b, name: B, scope: user}\n  - {id: c, name: C, scope: user, nodeSelector: {gpu: \"true\"}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	a := mustGet(t, set, "a")
	a.Env["A"] = "changed"
	a.NodeSelector["k"] = "changed"
	a.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("64")
	b := mustGet(t, set, "b")
	if b.Env["A"] != "x" || b.NodeSelector["k"] != "v" || quantityOf(t, b.Resources.Limits, corev1.ResourceCPU) != "2" {
		t.Errorf("a change of policy a changed policy b: %+v", b.Spec)
	}
	// The node selector of a policy replaces the selector of the defaults.
	// With the two keys together, a pod could find no node.
	if c := mustGet(t, set, "c"); len(c.NodeSelector) != 1 || c.NodeSelector["gpu"] != "true" {
		t.Errorf("node selector of policy c = %v, want only the key of the policy", c.NodeSelector)
	}
}

func TestParseRejectsBadSettings(t *testing.T) {
	policy := func(extra string) string {
		return "policies:\n  - id: a\n    name: A\n    scope: user\n" + extra
	}
	cases := map[string]string{
		"unknown default key":      "defaults: {privileged: \"true\"}\n" + policy(""),
		"id in defaults":           "defaults: {id: x}\n" + policy(""),
		"users in defaults":        "defaults: {users: [u]}\n" + policy(""),
		"defaults is a list":       "defaults: []\n" + policy(""),
		"two defaults":             "defaults: {image: a}\ndefaults: {image: b}\n" + policy(""),
		"empty users":              policy("    users: []\n"),
		"users is text":            policy("    users: u-1\n"),
		"empty user ID":            policy("    users: [\"\"]\n"),
		"number as user ID":        policy("    users: [42]\n"),
		"duplicate user":           policy("    users: [u, u]\n"),
		"user with control":        policy("    users: [\"a\\nb\"]\n"),
		"empty image":              policy("    image: \"\"\n"),
		"image with space":         policy("    image: \"a b\"\n"),
		"image with newline":       policy("    image: \"a\\nb\"\n"),
		"bad pull policy":          policy("    imagePullPolicy: always\n"),
		"bad pull secret":          policy("    imagePullSecrets: [Not_A_Name]\n"),
		"bad runtime class":        policy("    runtimeClassName: Not_A_Name\n"),
		"unknown resource":         policy("    resources:\n      limits: {nvidia.com/gpu: 1}\n"),
		"unknown resource kind":    policy("    resources:\n      maximum: {cpu: 1}\n"),
		"bad quantity":             policy("    resources:\n      limits: {memory: lots}\n"),
		"zero quantity":            policy("    resources:\n      limits: {memory: 0}\n"),
		"negative quantity":        policy("    resources:\n      limits: {cpu: -1}\n"),
		"octal quantity":           policy("    resources:\n      limits: {cpu: 010}\n"),
		"hex quantity":             policy("    resources:\n      limits: {cpu: 0x2}\n"),
		"boolean quantity":         policy("    resources:\n      limits: {cpu: true}\n"),
		"null quantity":            policy("    resources:\n      limits: {cpu: ~}\n"),
		"request above limit":      policy("    resources:\n      requests: {cpu: 2}\n      limits: {cpu: 1}\n"),
		"request above built-in":   policy("    resources:\n      requests: {memory: 64Gi}\n"),
		"request above defaults":   "defaults:\n  resources:\n    limits: {cpu: 1}\n" + policy("    resources:\n      requests: {cpu: 2}\n"),
		"bad storage type":         policy("    storage: {type: shared}\n"),
		"zero storage size":        policy("    storage: {size: 0}\n"),
		"bad storage size":         policy("    storage: {size: big}\n"),
		"bad storage class":        policy("    storage: {storageClassName: Not_A_Name}\n"),
		"empty storage class":      policy("    storage: {storageClassName: \"\"}\n"),
		"negative retention":       policy("    storage: {retention: -1h}\n"),
		"storage type in defaults": "defaults:\n  storage: {type: persistent}\n" + policy(""),
		"size with an exponent":    policy("    storage: {size: 1e9}\n"),
		"memory with an exponent":  policy("    resources:\n      limits: {memory: \"2e9\"}\n"),
		"zero days":                policy("    storage: {retention: 0d}\n"),
		"fraction of a day":        policy("    storage: {retention: 1.5d}\n"),
		"very short retention":     policy("    storage: {retention: 30s}\n"),
		"memory in milli-bytes":    policy("    resources:\n      limits: {memory: 512m}\n"),
		"very small memory":        policy("    resources:\n      limits: {memory: 1Mi}\n      requests: {memory: 1Mi}\n"),
		"very small CPU":           policy("    resources:\n      requests: {cpu: 0.0001}\n"),
		"storage in milli-bytes":   policy("    storage: {size: 500m}\n"),
		"very small storage":       policy("    storage: {size: 100}\n"),
		"fraction of a byte":       policy("    resources:\n      limits: {ephemeral-storage: 1500000001m}\n"),
		"env keep-alive":           policy("    env: {OPEN_TERMINAL_UVICORN_TIMEOUT_KEEP_ALIVE: \"1\"}\n"),
		"env PATH":                 policy("    env: {PATH: /home/user/bin}\n"),
		"env LD_PRELOAD":           policy("    env: {LD_PRELOAD: /home/user/x.so}\n"),
		"env PYTHONPATH":           policy("    env: {PYTHONPATH: /home/user}\n"),
		"env BASH_ENV":             policy("    env: {BASH_ENV: /home/user/.x}\n"),
		"retention with no unit":   policy("    storage: {retention: 30}\n"),
		"unknown storage key":      policy("    storage: {accessMode: ReadWriteMany}\n"),
		"storage is text":          policy("    storage: persistent\n"),
		"negative idle time":       policy("    idleTimeout: -5m\n"),
		"idle time with no unit":   policy("    idleTimeout: 30\n"),
		"env is a list":            policy("    env: [A=b]\n"),
		"env number value":         policy("    env: {A: 1}\n"),
		"env boolean value":        policy("    env: {A: true}\n"),
		"env null value":           policy("    env: {A: ~}\n"),
		"env bad name":             policy("    env: {\"A-B\": x}\n"),
		"env name with equals":     policy("    env: {\"A=B\": x}\n"),
		"env empty name":           policy("    env: {\"\": x}\n"),
		"env duplicate name":       policy("    env: {A: x, A: y}\n"),
		"env API key":              policy("    env: {OPEN_TERMINAL_API_KEY: x}\n"),
		"env API key file":         policy("    env: {OPEN_TERMINAL_API_KEY_FILE: /tmp/k}\n"),
		"env multi-user":           policy("    env: {OPEN_TERMINAL_MULTI_USER: \"true\"}\n"),
		"env isolated":             policy("    env: {OPEN_TERMINAL_ISOLATED: \"false\"}\n"),
		"env packages":             policy("    env: {OPEN_TERMINAL_PACKAGES: curl}\n"),
		"env pip packages":         policy("    env: {OPEN_TERMINAL_PIP_PACKAGES: requests}\n"),
		"env allowed domains":      policy("    env: {OPEN_TERMINAL_ALLOWED_DOMAINS: example.com}\n"),
		"env HOME":                 policy("    env: {HOME: /tmp}\n"),
		"env in defaults":          "defaults:\n  env: {OPEN_TERMINAL_API_KEY: x}\n" + policy(""),
		"env long timeout":         policy("    env: {OPEN_TERMINAL_EXECUTE_TIMEOUT: \"301\"}\n"),
		"env timeout not number":   policy("    env: {OPEN_TERMINAL_EXECUTE_TIMEOUT: \"5m\"}\n"),
		"env timeout zero":         policy("    env: {OPEN_TERMINAL_EXECUTE_TIMEOUT: \"0\"}\n"),
		"bad node selector key":    policy("    nodeSelector: {\"a b\": x}\n"),
		"bad node selector value":  policy("    nodeSelector: {a: \"x y\"}\n"),
		"toleration bad operator":  policy("    tolerations:\n      - {key: a, operator: In}\n"),
		"toleration bad effect":    policy("    tolerations:\n      - {key: a, effect: Evict}\n"),
		"toleration no key":        policy("    tolerations:\n      - {operator: Equal, value: x}\n"),
		"toleration unknown key":   policy("    tolerations:\n      - {key: a, seconds: 5}\n"),
		"toleration seconds":       policy("    tolerations:\n      - {key: a, effect: NoSchedule, tolerationSeconds: 5}\n"),
		"toleration is text":       policy("    tolerations: [a]\n"),
		"anchor":                   "defaults: &d {image: a}\n" + policy(""),
		"alias":                    "defaults: {env: &e {A: x}}\n" + policy("    env: *e\n"),
		"merge key":                "defaults: {env: &e {A: x}}\n" + policy("    <<: *e\n"),
		"tagged value":             policy("    image: !!binary aGk=\n"),
	}
	for name, content := range cases {
		if _, err := Parse([]byte(content)); err == nil {
			t.Errorf("%s: Parse gave no error", name)
		}
	}
	// The same file with no bad setting must be valid. If not, the cases
	// above fail for a different cause.
	if _, err := Parse([]byte(policy("    env: {A: x}\n"))); err != nil {
		t.Fatalf("the base file is not valid: %v", err)
	}
}

func TestErrorsHaveLineNumbers(t *testing.T) {
	_, err := Parse([]byte("policies:\n  - id: a\n    name: A\n    scope: user\n    resources:\n      limits:\n        memory: lots\n"))
	if err == nil || !strings.Contains(err.Error(), "line 7") {
		t.Errorf("error = %v, want the line 7", err)
	}
	_, err = Parse([]byte("policies:\n  - id: a\n    name: A\n    scope: user\n    env:\n      HOME: /x\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "HOME") {
		t.Errorf("error = %v, want the line of the policy and the name of the variable", err)
	}
}

func TestNewSetCompletesPoliciesFromCode(t *testing.T) {
	set, err := NewSet([]Policy{{ID: "a", Name: "A", Scope: scope.KindUser}})
	if err != nil {
		t.Fatal(err)
	}
	p := mustGet(t, set, "a")
	if !p.Persistent() || p.Storage.Size == nil || len(p.Resources.Limits) != 3 {
		t.Errorf("policy = %+v", p)
	}
}

func TestStoreReplacesTheSet(t *testing.T) {
	first, err := Parse([]byte("policies:\n  - {id: a, name: A, scope: user}\n"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(first)
	if _, ok := store.Get("a"); !ok || len(store.List()) != 1 {
		t.Fatal("the store does not have the first set")
	}
	second, err := Parse([]byte("policies:\n  - {id: b, name: B, scope: chat}\n  - {id: c, name: C, scope: user}\n"))
	if err != nil {
		t.Fatal(err)
	}
	store.Replace(second)
	if _, ok := store.Get("a"); ok {
		t.Error("the store has a policy of the previous set")
	}
	if _, ok := store.Get("b"); !ok || len(store.List()) != 2 {
		t.Error("the store does not have the new set")
	}
}

func TestDaysInATime(t *testing.T) {
	set, err := Parse([]byte("policies:\n  - {id: a, name: A, scope: user, storage: {retention: 30d}, idleTimeout: 1d}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := mustGet(t, set, "a")
	if *p.Storage.Retention != 720*time.Hour || *p.IdleTimeout != 24*time.Hour {
		t.Errorf("retention = %v, idle time = %v", *p.Storage.Retention, *p.IdleTimeout)
	}
}
