package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	networkingclient "k8s.io/client-go/kubernetes/typed/networking/v1"
	k8stesting "k8s.io/client-go/testing"

	"github.com/coderpunch/warren/internal/backend"
	"github.com/coderpunch/warren/internal/policy"
)

func (e *testEnv) networkPolicies() map[string]networkingv1.NetworkPolicy {
	e.t.Helper()
	list, err := e.client.NetworkingV1().NetworkPolicies(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]networkingv1.NetworkPolicy{}
	for _, np := range list.Items {
		out[np.Name] = np
	}
	return out
}

// policyOf returns the network policy of the pod of a sandbox.
func (e *testEnv) policyOf(req backend.Request) networkingv1.NetworkPolicy {
	e.t.Helper()
	pod := e.m.pod(req.Ref.Name())
	if pod == nil {
		e.t.Fatalf("no pod for %s", req.Ref.ContextID())
	}
	np, ok := e.networkPolicies()[pod.Labels[LabelStart]]
	if !ok {
		e.t.Fatalf("the pod of %s has no network policy with the name %q", req.Ref.ContextID(), pod.Labels[LabelStart])
	}
	return np
}

// The settings of the "restricted" Pod Security Standard are in each pod. A
// policy cannot change them.
func TestPodHasTheRestrictedSecuritySettings(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, req := range []backend.Request{userRef("alice"), templateRef("tuned", "alice"), templateRef("kept", "alice"), templateRef("other-image", "alice")} {
		e.acquire(req).Release()
		pod := e.m.pod(req.Ref.Name())
		id := req.Ref.TemplateID
		sc := pod.Spec.SecurityContext
		if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.SeccompProfile == nil ||
			sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s: pod security context = %+v", id, sc)
		}
		if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC || pod.Spec.ShareProcessNamespace != nil {
			t.Errorf("%s: the pod uses a namespace of the node", id)
		}
		if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			t.Errorf("%s: the pod gets a service account token", id)
		}
		if len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 {
			t.Fatalf("%s: containers = %d", id, len(pod.Spec.Containers))
		}
		c := pod.Spec.Containers[0]
		cs := c.SecurityContext
		if cs == nil || cs.AllowPrivilegeEscalation == nil || *cs.AllowPrivilegeEscalation ||
			cs.ReadOnlyRootFilesystem == nil || !*cs.ReadOnlyRootFilesystem ||
			cs.Privileged == nil || *cs.Privileged ||
			cs.Capabilities == nil || len(cs.Capabilities.Drop) != 1 || cs.Capabilities.Drop[0] != "ALL" || len(cs.Capabilities.Add) != 0 {
			t.Errorf("%s: container security context = %+v", id, cs)
		}
		for _, v := range pod.Spec.Volumes {
			if v.HostPath != nil || v.Secret != nil || v.Projected != nil || v.ConfigMap != nil {
				t.Errorf("%s: the volume %s is not a home or temporary directory", id, v.Name)
			}
		}
		// The start command does not use the entry point of the image, and
		// it has an absolute path. A file in the home directory cannot
		// replace the server.
		if len(c.Command) == 0 || !strings.HasPrefix(c.Command[0], "/") {
			t.Errorf("%s: command = %v", id, c.Command)
		}
		if pod.Labels[LabelSandbox] != pod.Name || pod.Labels[LabelStart] != podSecretName(pod) {
			t.Errorf("%s: labels = %v", id, pod.Labels)
		}
	}
	usual := e.m.pod(userRef("alice").Ref.Name())
	if got := strings.Join(usual.Spec.Containers[0].Command, " "); got != "/usr/bin/tini -- /usr/local/bin/python3 -I -u -m open_terminal" {
		t.Errorf("default command = %q", got)
	}
	// The user and the group are numbers, and the group is not 0.
	if sc := usual.Spec.SecurityContext; sc.RunAsUser == nil || *sc.RunAsUser != 1001 || sc.RunAsGroup == nil || *sc.RunAsGroup != 1001 {
		t.Errorf("user and group of a policy with no user ID = %v, %v", sc.RunAsUser, sc.RunAsGroup)
	}
	if usual.Spec.HostUsers != nil {
		t.Errorf("a policy with no userNamespace key must not ask for a user namespace")
	}
	other := e.m.pod(templateRef("other-image", "alice").Ref.Name())
	if got := strings.Join(other.Spec.Containers[0].Command, " "); got != "/sbin/tini -- /usr/local/bin/python3 -m open_terminal" {
		t.Errorf("command of the policy = %q", got)
	}
	if u := other.Spec.SecurityContext.RunAsUser; u == nil || *u != 1000 {
		t.Errorf("user ID of the policy = %v", u)
	}
	if other.Spec.HostUsers == nil || *other.Spec.HostUsers {
		t.Errorf("a policy with userNamespace true must have hostUsers false: %v", other.Spec.HostUsers)
	}
	// The arguments stay the same for each command.
	if got := strings.Join(other.Spec.Containers[0].Args, " "); got != "run --host 0.0.0.0 --port 8000" {
		t.Errorf("arguments = %q", got)
	}
}

func TestSandboxHasItsOwnNetworkPolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	alice, bob := userRef("alice"), userRef("bob")
	e.acquire(alice).Release()
	e.acquire(bob).Release()

	np := e.policyOf(alice)
	pod := e.m.pod(alice.Ref.Name())
	// The policy has the name of the Secret of the same start, and it
	// selects the pod of that start only.
	if np.Name != podSecretName(pod) || len(np.Spec.PodSelector.MatchLabels) != 1 || np.Spec.PodSelector.MatchLabels[LabelStart] != np.Name ||
		len(np.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("selector = %+v, name = %s", np.Spec.PodSelector, np.Name)
	}
	if other := e.policyOf(bob); other.Name == np.Name {
		t.Error("two sandboxes have the same network policy")
	}
	if len(np.OwnerReferences) != 1 || np.OwnerReferences[0].UID != pod.UID || np.OwnerReferences[0].Kind != "Pod" {
		t.Errorf("owner = %+v, want the pod", np.OwnerReferences)
	}
	if !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) || !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		t.Errorf("policy types = %v", np.Spec.PolicyTypes)
	}

	// Ingress: only the pods of warren, only the port of the server.
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 1 || len(np.Spec.Ingress[0].Ports) != 1 {
		t.Fatalf("ingress = %+v", np.Spec.Ingress)
	}
	from := np.Spec.Ingress[0].From[0]
	if from.IPBlock != nil || from.NamespaceSelector == nil || from.PodSelector == nil ||
		from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "warren-system" || len(from.NamespaceSelector.MatchLabels) != 1 ||
		from.PodSelector.MatchLabels["app.kubernetes.io/name"] != "warren" || len(from.PodSelector.MatchLabels) != 1 {
		t.Errorf("ingress source = %+v", from)
	}
	port := np.Spec.Ingress[0].Ports[0]
	if port.Port == nil || port.Port.IntValue() != 8000 || port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP || port.EndPort != nil {
		t.Errorf("ingress port = %+v", port)
	}

	// Egress with the "internet" mode: DNS of the cluster, and all
	// addresses except the private and special networks.
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want 2: %+v", len(np.Spec.Egress), np.Spec.Egress)
	}
	dns := np.Spec.Egress[0]
	if len(dns.To) != 1 || dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" ||
		dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" || len(dns.Ports) != 2 {
		t.Errorf("DNS rule = %+v", dns)
	}
	for _, p := range dns.Ports {
		if p.Port.IntValue() != 53 {
			t.Errorf("DNS port = %v", p.Port)
		}
	}
	internet := np.Spec.Egress[1]
	if len(internet.To) != 2 || len(internet.Ports) != 0 || internet.To[0].IPBlock == nil || internet.To[1].IPBlock == nil {
		t.Fatalf("internet rule = %+v", internet)
	}
	v4, v6 := internet.To[0].IPBlock, internet.To[1].IPBlock
	if v4.CIDR != "0.0.0.0/0" || v6.CIDR != "::/0" {
		t.Errorf("networks = %s and %s", v4.CIDR, v6.CIDR)
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10", "127.0.0.0/8"} {
		if !slices.Contains(v4.Except, cidr) {
			t.Errorf("the internet rule permits the private network %s", cidr)
		}
	}
	// 64:ff9b::a00:0/104 is the network 10.0.0.0/8 behind a NAT64 gateway.
	for _, cidr := range []string{"fc00::/7", "fe80::/10", "::1/128", "64:ff9b::a00:0/104", "64:ff9b::c0a8:0/112", "64:ff9b::a9fe:0/112", "2002::/16"} {
		if !slices.Contains(v6.Except, cidr) {
			t.Errorf("the internet rule permits the network %s", cidr)
		}
	}
	// A public IPv4 address behind a NAT64 gateway stays possible.
	if slices.Contains(v6.Except, "64:ff9b::/96") {
		t.Error("the internet rule refuses all addresses behind a NAT64 gateway")
	}
	// Kubernetes accepts an exception only when it is a part of the network
	// of its rule, and it refuses IPv4 addresses in IPv6 form.
	for _, b := range []*networkingv1.IPBlock{v4, v6} {
		whole := netip.MustParsePrefix(b.CIDR)
		for _, cidr := range b.Except {
			p, err := netip.ParsePrefix(cidr)
			if err != nil || p.Masked() != p || p.Addr().Is4In6() || p.Addr().Is4() != whole.Addr().Is4() || p.Bits() <= whole.Bits() {
				t.Errorf("Kubernetes does not accept the exception %q of the network %s", cidr, b.CIDR)
			}
		}
		if !slices.IsSorted(b.Except) {
			t.Errorf("the exceptions of %s are not in a fixed sequence: a different sequence makes an update at each run", b.CIDR)
		}
	}
}

func TestNetworkPolicyFollowsTheNetworkSettings(t *testing.T) {
	e := newTestEnv(t, nil)
	closed, db := templateRef("closed", "alice"), templateRef("database", "alice")
	e.acquire(closed).Release()
	e.acquire(db).Release()

	// "none" with no other key: no rule, thus no connection, also no DNS.
	if np := e.policyOf(closed); len(np.Spec.Egress) != 0 || len(np.Spec.Ingress) != 1 ||
		!slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		t.Errorf("closed policy: egress = %+v, types = %v", np.Spec.Egress, np.Spec.PolicyTypes)
	}
	np := e.policyOf(db)
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("database policy: egress rules = %d, want DNS and two networks: %+v", len(np.Spec.Egress), np.Spec.Egress)
	}
	for _, rule := range np.Spec.Egress {
		for _, to := range rule.To {
			if to.IPBlock != nil && (to.IPBlock.CIDR == "0.0.0.0/0" || to.IPBlock.CIDR == "::/0") {
				t.Errorf("a policy with the mode none permits all addresses: %+v", rule)
			}
		}
	}
	first := np.Spec.Egress[1]
	if len(first.To) != 1 || first.To[0].IPBlock.CIDR != "10.30.1.0/24" || len(first.To[0].IPBlock.Except) != 0 || len(first.Ports) != 2 {
		t.Fatalf("rule for the first network = %+v", first)
	}
	protocols := map[corev1.Protocol]bool{}
	for _, p := range first.Ports {
		if p.Port.IntValue() != 5432 {
			t.Errorf("port = %v, want 5432", p.Port)
		}
		protocols[*p.Protocol] = true
	}
	if !protocols[corev1.ProtocolTCP] || !protocols[corev1.ProtocolUDP] {
		t.Errorf("protocols = %v, want TCP and UDP", protocols)
	}
	// A network with no port list permits all ports.
	if second := np.Spec.Egress[2]; second.To[0].IPBlock.CIDR != "2001:db8::/32" || len(second.Ports) != 0 {
		t.Errorf("rule for the second network = %+v", second)
	}
}

func TestNetworkConfiguration(t *testing.T) {
	e := newTestEnv(t, func(c *Config) {
		c.Network.IngressCIDRs = []string{"192.0.2.10/32"}
		c.Network.DNSNamespace = "dns-system"
		c.Network.DNSPodLabels = map[string]string{"app": "coredns"}
		c.Network.DNSCIDRs = []string{"169.254.20.10/32"}
		c.Network.BlockedCIDRs = []string{"203.0.113.0/24", "2001:db8:1::/48"}
	})
	e.acquire(userRef("alice")).Release()
	np := e.policyOf(userRef("alice"))
	if from := np.Spec.Ingress[0].From; len(from) != 2 || from[1].IPBlock == nil || from[1].IPBlock.CIDR != "192.0.2.10/32" {
		t.Errorf("ingress sources = %+v", from)
	}
	dns := np.Spec.Egress[0]
	if len(dns.To) != 2 || dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "dns-system" ||
		dns.To[0].PodSelector.MatchLabels["app"] != "coredns" || dns.To[1].IPBlock.CIDR != "169.254.20.10/32" {
		t.Errorf("DNS rule = %+v", dns)
	}
	internet := np.Spec.Egress[1]
	if !slices.Contains(internet.To[0].IPBlock.Except, "203.0.113.0/24") || slices.Contains(internet.To[0].IPBlock.Except, "2001:db8:1::/48") ||
		!slices.Contains(internet.To[1].IPBlock.Except, "2001:db8:1::/48") {
		t.Errorf("blocked networks = %v and %v", internet.To[0].IPBlock.Except, internet.To[1].IPBlock.Except)
	}

	// Configurations that warren must refuse.
	base := func() Config {
		return Config{
			Client: fake.NewSimpleClientset(), Namespace: testNamespace, Image: "example.test/terminal:1",
			Policies: policy.NewStore(testPolicies(t)), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Network: NetworkConfig{WarrenNamespace: "warren-system", WarrenPodLabels: map[string]string{"app": "warren"}},
		}
	}
	bad := map[string]func(*Config){
		// The policy of the namespace refuses all connections of all its
		// pods. Warren must not be one of them.
		"sandboxes in the namespace of warren": func(c *Config) { c.Network.WarrenNamespace = testNamespace },
		"no source for ingress":                func(c *Config) { c.Network.WarrenNamespace = "" },
		"no pod labels":                        func(c *Config) { c.Network.WarrenPodLabels = nil },
		"bad ingress network":                  func(c *Config) { c.Network.IngressCIDRs = []string{"10.1.2.3"} },
		"bad DNS network":                      func(c *Config) { c.Network.DNSCIDRs = []string{"dns.example"} },
		"blocked network with host bits":       func(c *Config) { c.Network.BlockedCIDRs = []string{"203.0.113.7/24"} },
	}
	for name, change := range bad {
		cfg := base()
		change(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New gave no error", name)
		}
	}
	if _, err := New(base()); err != nil {
		t.Fatalf("the base configuration is not valid: %v", err)
	}
	// A warren that is not a pod needs an ingress network.
	cfg := base()
	cfg.Network.WarrenNamespace, cfg.Network.WarrenPodLabels, cfg.Network.IngressCIDRs = "", nil, []string{"192.0.2.0/24"}
	if _, err := New(cfg); err != nil {
		t.Errorf("configuration with an ingress network only: %v", err)
	}
}

// The sandbox namespace has a policy that refuses all connections. No
// sandbox starts before warren made sure that the policy exists.
func TestNoSandboxBeforeTheNamespacePolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	deny, ok := e.networkPolicies()[defaultDenyName]
	if !ok {
		t.Fatal("no policy for the namespace")
	}
	if len(deny.Spec.PodSelector.MatchLabels) != 0 || len(deny.Spec.PodSelector.MatchExpressions) != 0 ||
		len(deny.Spec.Ingress) != 0 || len(deny.Spec.Egress) != 0 || len(deny.Spec.PolicyTypes) != 2 {
		t.Errorf("namespace policy = %+v", deny.Spec)
	}

	// A manager that cannot make the policy is not ready and starts nothing.
	client := fake.NewSimpleClientset()
	refuse := func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("forbidden") }
	client.PrependReactor("get", "networkpolicies", refuse)
	client.PrependReactor("create", "networkpolicies", refuse)
	m, err := New(Config{
		Client: client, Namespace: testNamespace, Image: "example.test/terminal:1",
		Policies: policy.NewStore(testPolicies(t)), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Network: NetworkConfig{WarrenNamespace: "warren-system", WarrenPodLabels: map[string]string{"app": "warren"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.factory.Start(ctx.Done())
	waitFor(t, "caches", func() bool { return m.informer.HasSynced() && m.volumes.HasSynced() })
	if err := m.ensureDefaultDeny(ctx); err == nil {
		t.Fatal("no error when Kubernetes refuses the policy")
	}
	// All other conditions are good: only the policy of the namespace
	// decides.
	m.enforcement.Store(enforcementProven)
	m.otherPolicies.Store(0)
	if m.Ready() || !strings.Contains(m.NotReadyReason(), "forbidden") {
		t.Errorf("ready = %v, reason = %q with no policy for the namespace", m.Ready(), m.NotReadyReason())
	}
	if _, err := m.Acquire(ctx, userRef("alice")); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Acquire with no policy for the namespace: %v, want ErrNotReady", err)
	}
	pods, _ := client.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("pods = %d, want 0", len(pods.Items))
	}
}

// A person or a program can change or delete the network policies. The
// reaper puts them back.
func TestReaperRepairsNetworkPolicies(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	e.acquire(req).Release()
	policies := e.client.NetworkingV1().NetworkPolicies(testNamespace)
	ctx := context.Background()
	name := e.policyOf(req).Name

	// The policy of the namespace gets a rule that permits all connections.
	deny, _ := policies.Get(ctx, defaultDenyName, metav1.GetOptions{})
	deny.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{}}
	deny.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	if _, err := policies.Update(ctx, deny, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// The policy of the sandbox gets a rule that permits all destinations.
	np, _ := policies.Get(ctx, name, metav1.GetOptions{})
	np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{})
	np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{}}
	if _, err := policies.Update(ctx, np, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.m.Reap(ctx)
	if got := e.networkPolicies()[defaultDenyName]; len(got.Spec.Ingress) != 0 || len(got.Spec.Egress) != 0 {
		t.Errorf("the reaper did not repair the policy of the namespace: %+v", got.Spec)
	}
	if got := e.policyOf(req); len(got.Spec.Egress) != 2 || len(got.Spec.Ingress) != 1 || len(got.Spec.Ingress[0].From) != 1 {
		t.Errorf("the reaper did not repair the policy of the sandbox: %+v", got.Spec)
	}

	// The two policies are deleted.
	for _, n := range []string{defaultDenyName, name} {
		if err := policies.Delete(ctx, n, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	e.m.Reap(ctx)
	all := e.networkPolicies()
	if _, ok := all[defaultDenyName]; !ok {
		t.Error("the reaper did not make the policy of the namespace again")
	}
	again, ok := all[name]
	if !ok {
		t.Fatal("the reaper did not make the policy of the sandbox again")
	}
	if pod := e.m.pod(req.Ref.Name()); len(again.OwnerReferences) != 1 || again.OwnerReferences[0].UID != pod.UID {
		t.Errorf("owner of the new policy = %+v", again.OwnerReferences)
	}
}

// A change of the network settings in the policy file applies to the
// sandboxes that operate.
func TestNetworkSettingsChangeForARunningSandbox(t *testing.T) {
	e := newTestEnv(t, nil)
	req := templateRef("quick", "alice")
	e.acquire(req).Release()
	if n := len(e.policyOf(req).Spec.Egress); n != 2 {
		t.Fatalf("egress rules at the start = %d, want 2", n)
	}
	uid := e.m.pod(req.Ref.Name()).UID
	name := e.policyOf(req).Name
	// A new permission needs no restart.
	e.usePolicies("policies:\n  - {id: quick, name: Quick, scope: user, storage: {type: ephemeral}, network: {allow: [{cidr: 10.9.0.0/16}]}}\n")
	e.m.Reap(context.Background())
	if np := e.policyOf(req); len(np.Spec.Egress) != 3 {
		t.Errorf("egress rules after a new permission = %d, want 3", len(np.Spec.Egress))
	}
	if p := e.m.pod(req.Ref.Name()); p == nil || p.UID != uid {
		t.Fatal("a new permission stopped the sandbox")
	}
	// A removed permission: the policy changes, and the sandbox stops. A
	// network plugin can let an open connection continue after the change.
	lease := e.acquire(req)
	e.usePolicies("policies:\n  - {id: quick, name: Quick, scope: user, storage: {type: ephemeral}, network: {egress: none}}\n")
	e.m.Reap(context.Background())
	if np := e.networkPolicies()[name]; len(np.Spec.Egress) != 0 {
		t.Errorf("egress rules after the change to none = %+v", np.Spec.Egress)
	}
	waitFor(t, "stop of the sandbox after a removed permission", func() bool { return e.m.pod(req.Ref.Name()) == nil })
	lease.Release()
	// The next start has the new settings from the start.
	e.acquire(req).Release()
	if np := e.policyOf(req); len(np.Spec.Egress) != 0 {
		t.Errorf("egress rules of the new pod = %+v", np.Spec.Egress)
	}
}

func TestNarrowerNetworkSettings(t *testing.T) {
	yes, no := true, false
	internet := policy.Network{Egress: policy.EgressInternet}
	none := policy.Network{Egress: policy.EgressNone}
	db := policy.Destination{CIDR: "10.1.0.0/16", Ports: []int32{5432}}
	cases := []struct {
		name     string
		old, now policy.Network
		want     bool
	}{
		{"no change", internet, internet, false},
		{"internet to none", internet, none, true},
		{"none to internet", none, internet, false},
		{"DNS removed", internet, policy.Network{Egress: policy.EgressInternet, DNS: &no}, true},
		{"DNS added", none, policy.Network{Egress: policy.EgressNone, DNS: &yes}, false},
		{"destination added", none, policy.Network{Egress: policy.EgressNone, Allow: []policy.Destination{db}}, false},
		{"destination removed", policy.Network{Egress: policy.EgressNone, Allow: []policy.Destination{db}}, none, true},
		{"port removed", policy.Network{Egress: policy.EgressNone, Allow: []policy.Destination{db}},
			policy.Network{Egress: policy.EgressNone, Allow: []policy.Destination{{CIDR: "10.1.0.0/16", Ports: []int32{5433}}}}, true},
	}
	for _, c := range cases {
		if got := narrower(networkRecord(c.old), c.now); got != c.want {
			t.Errorf("%s: narrower = %v, want %v", c.name, got, c.want)
		}
	}
	// A record that warren cannot read is not a cause for a restart.
	if narrower("", none) || narrower("not JSON", none) {
		t.Error("a record that warren cannot read caused a restart")
	}
}

// A network policy with no pod must go away. A start that fails must delete
// the policy and the Secret that it made.
func TestNetworkPoliciesWithNoPodGoAway(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	e.acquire(req).Release()
	old := e.policyOf(req).Name
	// The simulated cluster has no garbage collection. The reaper is the
	// second protection.
	e.stop(req)
	e.m.Reap(context.Background())
	if _, ok := e.networkPolicies()[old]; !ok {
		t.Fatal("the reaper deleted a new policy. A start makes the policy before the pod.")
	}
	e.clock.Advance(3 * time.Minute)
	e.m.Reap(context.Background())
	all := e.networkPolicies()
	if _, ok := all[old]; ok {
		t.Error("the reaper did not delete the policy of a pod that is gone")
	}
	if _, ok := all[defaultDenyName]; !ok {
		t.Error("the reaper deleted the policy of the namespace")
	}

	e.client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("violates PodSecurity")
	})
	secrets := len(e.secrets())
	if _, err := e.m.Acquire(context.Background(), userRef("bob")); err == nil {
		t.Fatal("no error for a pod that Kubernetes refuses")
	}
	if got := e.networkPolicies(); len(got) != 1 {
		t.Errorf("network policies after the failed start = %d, want only the policy of the namespace", len(got))
	}
	if n := len(e.secrets()); n != secrets {
		t.Errorf("secrets after the failed start = %d, want %d", n, secrets)
	}
}

// The network policy must exist before the pod. A pod that operates with no
// policy of its own cannot get a connection from warren.
func TestNetworkPolicyIsMadeBeforeThePod(t *testing.T) {
	e := newTestEnv(t, nil)
	e.acquire(userRef("alice")).Release()
	policyAt, podAt := -1, -1
	for i, a := range e.client.Actions() {
		if a.Matches("create", "networkpolicies") && a.(k8stesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy).Name != defaultDenyName && policyAt < 0 {
			policyAt = i
		}
		if a.Matches("create", "pods") && podAt < 0 {
			podAt = i
		}
	}
	if policyAt < 0 || podAt < 0 || policyAt > podAt {
		t.Errorf("create request of the policy at %d, of the pod at %d", policyAt, podAt)
	}
}

// A pod from a version of warren that made no network policies has no
// label for its policy. The policy of the namespace refuses its connections,
// thus the reaper stops it.
func TestPodFromAVersionWithNoNetworkPolicies(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	old := e.m.buildPod(req.Ref.Name(), req.Ref.Name()+"-00000000", req, e.policy(req))
	delete(old.Labels, LabelStart)
	readyStatus(old)
	if _, err := e.client.CoreV1().Pods(testNamespace).Create(context.Background(), old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod in the cache", func() bool { return e.m.pod(req.Ref.Name()) != nil })
	e.m.Reap(context.Background())
	waitFor(t, "stop of the old pod", func() bool { return e.m.pod(req.Ref.Name()) == nil })
}

// SelfCheck finds a configuration of warren with which no request gets to a
// sandbox, or with which a sandbox can connect to the cluster.
func TestSelfCheck(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	net := NetworkConfig{WarrenNamespace: "warren-system", WarrenPodLabels: map[string]string{"app": "warren"}}
	if err := net.validate(testNamespace); err != nil {
		t.Fatal(err)
	}
	pod := func(change func(*corev1.Pod)) *fake.Clientset {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "warren-1", Namespace: "warren-system", Labels: map[string]string{"app": "warren", "pod-template-hash": "x"}},
			Status:     corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: "10.244.1.7"}}},
		}
		if change != nil {
			change(p)
		}
		return fake.NewSimpleClientset(p)
	}
	ctx := context.Background()
	if err := SelfCheck(ctx, pod(nil), "warren-1", net, "10.96.0.1", logger); err != nil {
		t.Errorf("correct configuration: %v", err)
	}
	bad := map[string]struct {
		client      *fake.Clientset
		serviceHost string
	}{
		"labels of the configuration are not on the pod": {pod(func(p *corev1.Pod) { p.Labels["app"] = "other" }), "10.96.0.1"},
		"pod with the network of the node":               {pod(func(p *corev1.Pod) { p.Spec.HostNetwork = true }), "10.96.0.1"},
		"pod network with public addresses":              {pod(func(p *corev1.Pod) { p.Status.PodIPs = []corev1.PodIP{{IP: "11.0.3.4"}} }), "10.96.0.1"},
		"second pod address is public":                   {pod(func(p *corev1.Pod) { p.Status.PodIPs = append(p.Status.PodIPs, corev1.PodIP{IP: "2001:db8::7"}) }), "10.96.0.1"},
		"service network with public addresses":          {pod(nil), "198.51.100.1"},
	}
	for name, c := range bad {
		if err := SelfCheck(ctx, c.client, "warren-1", net, c.serviceHost, logger); err == nil {
			t.Errorf("%s: SelfCheck gave no error", name)
		}
	}
	// The administrator adds the public networks of the cluster.
	wide := net
	wide.BlockedCIDRs = []string{"11.0.0.0/16", "198.51.100.0/24"}
	if err := wide.validate(testNamespace); err != nil {
		t.Fatal(err)
	}
	if err := SelfCheck(ctx, pod(func(p *corev1.Pod) { p.Status.PodIPs = []corev1.PodIP{{IP: "11.0.3.4"}} }), "warren-1", wide, "198.51.100.1", logger); err != nil {
		t.Errorf("public networks that the configuration blocks: %v", err)
	}
	// Warren cannot read its pod, or it is not a pod. This is not an error.
	if err := SelfCheck(ctx, fake.NewSimpleClientset(), "warren-1", net, "10.96.0.1", logger); err != nil {
		t.Errorf("pod that warren cannot read: %v", err)
	}
	if err := SelfCheck(ctx, fake.NewSimpleClientset(), "", net, "", logger); err != nil {
		t.Errorf("warren that is not a pod: %v", err)
	}
}

func TestNetworkPolicyForPodsOfTheCluster(t *testing.T) {
	e := newTestEnv(t, nil)
	e.usePolicies(`
policies:
  - id: quick
    name: Quick
    scope: user
    storage: {type: ephemeral}
    network:
      egress: none
      allow:
        - namespace: databases
          podLabels: {app: postgres}
          ports: [5432]
`)
	req := templateRef("quick", "alice")
	e.acquire(req).Release()
	np := e.policyOf(req)
	if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].To) != 1 {
		t.Fatalf("egress = %+v", np.Spec.Egress)
	}
	to := np.Spec.Egress[0].To[0]
	if to.IPBlock != nil || to.NamespaceSelector == nil || to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "databases" ||
		to.PodSelector == nil || to.PodSelector.MatchLabels["app"] != "postgres" || len(np.Spec.Egress[0].Ports) != 2 {
		t.Errorf("destination = %+v, ports = %+v", to, np.Spec.Egress[0].Ports)
	}
}

// A rule with no source or no destination permits all sources or all
// destinations. No policy of warren must have such a rule.
func TestNoRulePermitsAllPeers(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, req := range []backend.Request{userRef("alice"), templateRef("closed", "alice"), templateRef("database", "alice")} {
		e.acquire(req).Release()
		np := e.policyOf(req)
		for _, rule := range np.Spec.Ingress {
			if len(rule.From) == 0 || len(rule.Ports) == 0 {
				t.Errorf("%s: an ingress rule has no source or no port: %+v", req.Ref.TemplateID, rule)
			}
			for _, from := range rule.From {
				if from.IPBlock == nil && (from.NamespaceSelector == nil || len(from.NamespaceSelector.MatchLabels) == 0 || from.PodSelector == nil || len(from.PodSelector.MatchLabels) == 0) {
					t.Errorf("%s: an ingress source selects all namespaces or all pods: %+v", req.Ref.TemplateID, from)
				}
			}
		}
		for _, rule := range np.Spec.Egress {
			if len(rule.To) == 0 {
				t.Errorf("%s: an egress rule has no destination: %+v", req.Ref.TemplateID, rule)
			}
			for _, to := range rule.To {
				if to.IPBlock == nil && (to.NamespaceSelector == nil || len(to.NamespaceSelector.MatchLabels) == 0 || to.PodSelector == nil || len(to.PodSelector.MatchLabels) == 0) {
					t.Errorf("%s: an egress destination selects all namespaces or all pods: %+v", req.Ref.TemplateID, to)
				}
			}
		}
	}
}

// Kubernetes refuses a network policy that has a bad label or a bad
// namespace. The policy of the test pod has no DNS rule. Thus a bad DNS
// label would pass the test of the network plugin, and then each start of a
// sandbox would fail. Warren must refuse such a configuration at its start.
func TestLabelsThatKubernetesRefuses(t *testing.T) {
	for name, change := range map[string]func(*NetworkConfig){
		// "--dns-pod-labels=k8s-app=kube-dns app=x": a space in place of the comma.
		"DNS label value with a space":     func(c *NetworkConfig) { c.DNSPodLabels = map[string]string{"k8s-app": "kube-dns app=x"} },
		"DNS label value with two signs":   func(c *NetworkConfig) { c.DNSPodLabels = map[string]string{"a": "b=c"} },
		"warren label value with ;":        func(c *NetworkConfig) { c.WarrenPodLabels = map[string]string{"a": "b;c=d"} },
		"warren label key with a space":    func(c *NetworkConfig) { c.WarrenPodLabels = map[string]string{"app name": "warren"} },
		"DNS namespace that is no name":    func(c *NetworkConfig) { c.DNSNamespace = "kube system" },
		"warren namespace that is no name": func(c *NetworkConfig) { c.WarrenNamespace = "Warren_System" },
	} {
		c := NetworkConfig{WarrenNamespace: "warren-system", WarrenPodLabels: map[string]string{"app": "warren"}}
		change(&c)
		if err := c.validate(testNamespace); err == nil || !strings.Contains(err.Error(), "--") {
			t.Errorf("%s: validate = %v, want an error that names the flag", name, err)
		}
	}
	// The usual labels stay valid.
	ok := NetworkConfig{
		WarrenNamespace: "warren-system", WarrenPodLabels: map[string]string{"app.kubernetes.io/name": "warren", "tier": "A_1.b"},
		DNSNamespace: "openshift-dns", DNSPodLabels: map[string]string{"dns.operator.openshift.io/daemonset-dns": "default"},
	}
	if err := ok.validate(testNamespace); err != nil {
		t.Errorf("valid labels: %v", err)
	}
}

// On a cluster with IPv4 and IPv6, a pod has two addresses. The second one
// can be public when the first one is private.
func TestSandboxWithAPublicSecondAddress(t *testing.T) {
	e := newTestEnv(t, nil)
	req := userRef("alice")
	name := req.Ref.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, req)
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	uid := e.m.pod(name).UID
	e.setStatus(name, func(p *corev1.Pod) {
		readyStatus(p)
		p.Status.PodIPs = []corev1.PodIP{{IP: "127.0.0.1"}, {IP: "2a01:4f8::1234"}}
	})
	r := <-res
	if r.err == nil || !strings.Contains(r.err.Error(), "2a01:4f8::1234") {
		t.Fatalf("Acquire = %v, want an error that names the public address", r.err)
	}
	waitFor(t, "stop of the sandbox with a public address", func() bool { p := e.m.pod(name); return p == nil || p.UID != uid })
}

// A problem of the API server for a short time tells nothing about the
// policy of the namespace. Warren stays ready.
func TestNamespacePolicyThatWarrenCannotRead(t *testing.T) {
	e := newTestEnv(t, nil)
	e.client.PrependReactor("get", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server is not available")
	})
	e.m.Reap(context.Background())
	if !e.m.Ready() {
		t.Errorf("the manager is not ready after one read that failed: %s", e.m.NotReadyReason())
	}
}

// slowRequests is a client whose requests for the network policies take
// time: beforeList runs before each list request gets to the API server, and
// beforeUpdate runs before each update request. A reactor of the simulated
// cluster cannot do this, because the simulated cluster accepts no other
// request while a reactor runs.
type slowRequests struct {
	kubernetes.Interface
	beforeList   func()
	beforeUpdate func(name string)
}

func (c slowRequests) NetworkingV1() networkingclient.NetworkingV1Interface {
	return slowNetworking{c.Interface.NetworkingV1(), c}
}

type slowNetworking struct {
	networkingclient.NetworkingV1Interface
	slow slowRequests
}

func (n slowNetworking) NetworkPolicies(namespace string) networkingclient.NetworkPolicyInterface {
	return slowPolicies{n.NetworkingV1Interface.NetworkPolicies(namespace), n.slow}
}

type slowPolicies struct {
	networkingclient.NetworkPolicyInterface
	slow slowRequests
}

func (p slowPolicies) List(ctx context.Context, opts metav1.ListOptions) (*networkingv1.NetworkPolicyList, error) {
	if p.slow.beforeList != nil {
		p.slow.beforeList()
	}
	return p.NetworkPolicyInterface.List(ctx, opts)
}

func (p slowPolicies) Update(ctx context.Context, np *networkingv1.NetworkPolicy, opts metav1.UpdateOptions) (*networkingv1.NetworkPolicy, error) {
	if p.slow.beforeUpdate != nil {
		p.slow.beforeUpdate(np.Name)
	}
	return p.NetworkPolicyInterface.Update(ctx, np, opts)
}

// A pod can get its node, become ready, and get its first lease while a run
// of the reaper is in progress. The policy of the pod then has the address
// of its node (blockNode). The reaper must not write the policy again with
// node addresses that it read before that moment.
func TestReaperKeepsTheNodeAddressOfTheFirstLease(t *testing.T) {
	var before func()
	e := newTestEnv(t, func(c *Config) {
		c.Client = slowRequests{Interface: c.Client, beforeList: func() {
			if before != nil {
				before()
			}
		}}
	})
	req := userRef("alice")
	name := req.Ref.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, req)
	waitFor(t, "pod", func() bool { return e.m.pod(name) != nil })
	blocked := func() bool {
		return slices.Contains(e.policyOf(req).Spec.Egress[1].To[0].IPBlock.Except, "203.0.113.5/32")
	}

	var once sync.Once
	var lease backend.Lease
	before = func() {
		once.Do(func() {
			// The list request of the reaper is in progress.
			e.setStatus(name, func(p *corev1.Pod) {
				readyStatus(p)
				p.Status.HostIP = "203.0.113.5"
				p.Status.HostIPs = []corev1.HostIP{{IP: "203.0.113.5"}}
			})
			r := <-res
			if r.err != nil {
				t.Error(r.err)
				return
			}
			lease = r.lease
			if !blocked() {
				t.Error("the policy does not have the address of the node at the first lease")
			}
		})
	}
	e.m.Reap(context.Background())
	if lease == nil {
		t.Fatal("no lease")
	}
	defer lease.Release()
	if !blocked() {
		t.Error("the reaper removed the address of the node from the policy of a sandbox that has a lease")
	}
}

// The policy of a new sandbox can have the address of its node from its
// start: a different sandbox was on that node at that time. That sandbox can
// go away before the cache has the node of the new pod. A run of the reaper
// that reads the node addresses in that time has no address of that node.
// Its update request must not remove the address from the policy after the
// first lease of the new pod: Kubernetes must refuse that request.
//
// The case "other node" is the control: the policy does not have the address
// of the node at its start, and blockNode adds it.
func TestReaperKeepsTheNodeAddressThatThePolicyHadAtItsStart(t *testing.T) {
	const node = "203.0.113.5"
	for _, tc := range []struct{ name, neighbour string }{
		{"neighbour on the same node", node},
		{"neighbour on an other node", "203.0.113.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before atomic.Pointer[func(string)]
			e := newTestEnv(t, func(c *Config) {
				c.Client = slowRequests{Interface: c.Client, beforeUpdate: func(name string) {
					if f := before.Load(); f != nil {
						(*f)(name)
					}
				}}
			})
			has := func(req backend.Request, address string) bool {
				return slices.Contains(e.policyOf(req).Spec.Egress[1].To[0].IPBlock.Except, address+"/32")
			}
			onNode := func(address string) func(*corev1.Pod) {
				return func(p *corev1.Pod) {
					readyStatus(p)
					p.Status.HostIP = address
					p.Status.HostIPs = []corev1.HostIP{{IP: address}}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			// The sandbox of alice operates on a node.
			alice, bob := userRef("alice"), userRef("bob")
			res := e.acquireAsync(ctx, e.m, alice)
			waitFor(t, "pod of alice", func() bool { return e.m.pod(alice.Ref.Name()) != nil })
			e.setStatus(alice.Ref.Name(), onNode(tc.neighbour))
			r := <-res
			if r.err != nil {
				t.Fatal(r.err)
			}
			r.lease.Release()
			e.m.Reap(context.Background())

			// The sandbox of bob starts. Its policy has the address of the
			// node of alice from its start. Kubernetes did not select a
			// node for the pod of bob yet.
			res = e.acquireAsync(ctx, e.m, bob)
			waitFor(t, "pod of bob", func() bool { return e.m.pod(bob.Ref.Name()) != nil })
			start := e.m.pod(bob.Ref.Name()).Labels[LabelStart]
			if !has(bob, tc.neighbour) {
				t.Fatal("the policy of the new sandbox does not have the address of the node of the other sandbox at its start")
			}
			e.stop(alice)

			// The reaper read the node addresses: it found none. Its update
			// request for the policy of bob is in progress. In that time,
			// the pod of bob gets its node, becomes ready, and gets its
			// first lease. The update request of blockNode comes through
			// the same client, thus only the first call does the work.
			var done atomic.Bool
			var lease backend.Lease
			hook := func(name string) {
				if name != start || !done.CompareAndSwap(false, true) {
					return
				}
				e.setStatus(bob.Ref.Name(), onNode(node))
				r := <-res
				if r.err != nil {
					t.Error(r.err)
					return
				}
				lease = r.lease
				if !has(bob, node) {
					t.Error("the policy does not have the address of the node at the first lease")
				}
			}
			before.Store(&hook)
			e.m.Reap(context.Background())
			before.Store(nil)
			if lease == nil {
				t.Fatal("no lease")
			}
			defer lease.Release()
			if !has(bob, node) {
				t.Error("the reaper removed the address of the node from the policy of a sandbox that has a lease")
			}
			if n := e.policyConflicts.Load(); n != 1 {
				t.Errorf("refused update requests = %d, want 1: the first lease must make the update request of the reaper too old", n)
			}
			// The next run has the current policy and the current addresses.
			e.m.Reap(context.Background())
			if np := e.policyOf(bob); !has(bob, node) || len(np.OwnerReferences) != 1 {
				t.Errorf("policy after the next run of the reaper: exceptions = %v, owners = %d",
					np.Spec.Egress[1].To[0].IPBlock.Except, len(np.OwnerReferences))
			}
		})
	}
}

// A run of the reaper that cannot write the network policies must not record
// the node addresses as present in the policies. If it does, the subsequent
// events of the pod on a new node ask for no run of the reaper. The policies
// of the other sandboxes then get the address of the new node only at the
// next run of the timer.
func TestFailedReaperRunDoesNotHideANewNodeAddress(t *testing.T) {
	const node = "203.0.113.5"
	for _, tc := range []struct {
		name string
		// verb is the request for the network policies that Kubernetes
		// refuses in the first run of the reaper after the pod got its node.
		verb string
	}{
		{"list refused", "list"},
		{"update refused", "update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t, nil)
			ctx := context.Background()
			has := func(req backend.Request) bool {
				return slices.Contains(e.policyOf(req).Spec.Egress[1].To[0].IPBlock.Except, node+"/32")
			}
			// nextRun does the work of Run: it waits for a request for a run
			// of the reaper, and it does that run.
			nextRun := func(what string) {
				t.Helper()
				waitFor(t, what, func() bool { return len(e.m.kick) == 1 })
				<-e.m.kick
				e.m.Reap(ctx)
			}
			alice, bob := userRef("alice"), userRef("bob")
			e.acquire(alice).Release()
			e.m.Reap(ctx)
			other := e.policyOf(alice).Name

			var refuse atomic.Bool
			e.client.PrependReactor(tc.verb, "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if !refuse.Load() {
					return false, nil, nil
				}
				// Of the update requests, Kubernetes refuses only the request
				// for the policy of the other sandbox.
				if u, ok := a.(k8stesting.UpdateAction); ok && u.GetObject().(*networkingv1.NetworkPolicy).Name != other {
					return false, nil, nil
				}
				return true, nil, errors.New("the server is not available")
			})

			// The pod of bob starts. Kubernetes selects a new node for it.
			// That event asks for a run of the reaper, and the run fails.
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			res := e.acquireAsync(c, e.m, bob)
			waitFor(t, "pod of bob", func() bool { return e.m.pod(bob.Ref.Name()) != nil })
			// The reaper writes the activity time to a new pod, and that
			// write is a pod event. Let it come before the node. The only
			// pod events after it are then the events of this test.
			e.m.Reap(ctx)
			waitFor(t, "activity time of the pod of bob in the cache", func() bool {
				return e.m.pod(bob.Ref.Name()).Annotations[AnnotationActivity] != ""
			})
			e.setStatus(bob.Ref.Name(), func(p *corev1.Pod) {
				p.Status.HostIP = node
				p.Status.HostIPs = []corev1.HostIP{{IP: node}}
			})
			refuse.Store(true)
			nextRun("request for a run of the reaper for the new node")
			refuse.Store(false)
			if has(alice) {
				t.Fatal("the test is wrong: the run that failed changed the policy of the other sandbox")
			}

			// A subsequent event of the same pod: the pod is ready.
			e.setStatus(bob.Ref.Name(), readyStatus)
			r := <-res
			if r.err != nil {
				t.Fatal(r.err)
			}
			defer r.lease.Release()
			if !has(bob) {
				t.Error("the policy of the new sandbox does not have the address of its node")
			}
			// The policy of the other sandbox must get the address with no
			// wait for the timer.
			nextRun("request for a run of the reaper after the run that failed")
			if !has(alice) {
				t.Error("the policy of the other sandbox does not have the address of the new node")
			}
			// A successful run records the address. Without the record, each
			// event of the pod would ask for a run.
			e.m.mu.Lock()
			known := e.m.hosts[node+"/32"]
			e.m.mu.Unlock()
			if !known {
				t.Error("a successful run did not record the address of the node")
			}
		})
	}
}

// A person or a program can change the policy of the namespace: it can then
// permit connections for all sandboxes. If warren cannot repair the policy,
// warren is not ready and starts no sandbox (docs/isolation.md, section 3.1).
func TestNamespacePolicyThatWarrenCannotRepair(t *testing.T) {
	e := newTestEnv(t, nil)
	ctx := context.Background()
	policies := e.client.NetworkingV1().NetworkPolicies(testNamespace)
	cur, err := policies.Get(ctx, defaultDenyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A rule with no peer permits all connections to all pods.
	cur.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{}}
	if _, err := policies.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	var refuse atomic.Bool
	refuse.Store(true)
	e.client.PrependReactor("update", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		if refuse.Load() {
			return true, nil, errors.New("networkpolicies is forbidden")
		}
		return false, nil, nil
	})
	e.m.Reap(ctx)
	if e.m.Ready() {
		t.Fatal("the manager is ready with a namespace policy that permits all connections to the sandboxes")
	}
	if reason := e.m.NotReadyReason(); !strings.Contains(reason, "repair the network policy") || !strings.Contains(reason, "forbidden") {
		t.Errorf("reason = %q, want the cause", reason)
	}
	if err := e.m.Start(ctx, userRef("alice")); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Start = %v, want ErrNotReady", err)
	}
	// Kubernetes accepts the change again. The next reaper run repairs the
	// policy, and warren is ready.
	refuse.Store(false)
	e.m.Reap(ctx)
	if !e.m.Ready() {
		t.Errorf("the manager is not ready after the repair: %s", e.m.NotReadyReason())
	}
	if np := e.networkPolicies()[defaultDenyName]; len(np.Spec.Ingress) != 0 || len(np.Spec.Egress) != 0 {
		t.Errorf("rules of the policy after the repair: %+v", np.Spec)
	}
}

// Warren reads a policy of the namespace that permits all connections.
// Kubernetes refuses the repair with the status 409, because a different
// writer changed the policy after the read. The second read then fails. In
// this run, warren saw a wrong policy and did not repair it: it must not
// stay ready (docs/isolation.md, section 3.1).
func TestNamespacePolicyConflictAndThenAReadThatFails(t *testing.T) {
	e := newTestEnv(t, nil)
	ctx := context.Background()
	policies := e.client.NetworkingV1().NetworkPolicies(testNamespace)
	cur, err := policies.Get(ctx, defaultDenyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A rule with no peer permits all connections to all pods.
	cur.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{}}
	if _, err := policies.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	var fail atomic.Bool
	var gets, updates atomic.Int64
	fail.Store(true)
	// The different writer wrote the same wrong policy again, thus the update
	// request of warren has an old resource version.
	e.client.PrependReactor("update", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		np := a.(k8stesting.UpdateAction).GetObject().(*networkingv1.NetworkPolicy)
		if np.Name != defaultDenyName || !fail.Load() {
			return false, nil, nil
		}
		updates.Add(1)
		return true, nil, apierrors.NewConflict(networkingv1.Resource("networkpolicies"), np.Name, errors.New("the object has been modified"))
	})
	// The first read of the run is successful. The second read, after the
	// conflict, gets a timeout of the API server.
	e.client.PrependReactor("get", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() != defaultDenyName || !fail.Load() || gets.Add(1) != 2 {
			return false, nil, nil
		}
		return true, nil, apierrors.NewServerTimeout(networkingv1.Resource("networkpolicies"), "get", 1)
	})
	e.m.Reap(ctx)
	if g, u := gets.Load(), updates.Load(); g != 2 || u != 1 {
		t.Fatalf("requests of the run: %d read requests and %d update requests, want 2 and 1", g, u)
	}
	if e.m.Ready() {
		t.Error("the manager is ready, but this run read a namespace policy that permits all connections and did not repair it")
	}
	if reason := e.m.NotReadyReason(); !strings.Contains(reason, "read the network policy") {
		t.Errorf("reason = %q, want the cause", reason)
	}
	if err := e.m.Start(ctx, userRef("alice")); !errors.Is(err, backend.ErrNotReady) {
		t.Errorf("Start = %v, want ErrNotReady", err)
	}
	// Kubernetes accepts the requests again. The next reaper run repairs the
	// policy, and warren is ready.
	fail.Store(false)
	e.m.Reap(ctx)
	if !e.m.Ready() {
		t.Errorf("the manager is not ready after the repair: %s", e.m.NotReadyReason())
	}
	if np := e.networkPolicies()[defaultDenyName]; len(np.Spec.Ingress) != 0 || len(np.Spec.Egress) != 0 {
		t.Errorf("rules of the policy after the repair: %+v", np.Spec)
	}
}

const (
	publicNodeA = "203.0.113.5"
	publicNodeB = "203.0.113.9"
)

// onPublicNode gives a pod the ready status on a node with a public address.
func onPublicNode(address string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		readyStatus(p)
		p.Status.HostIP = address
		p.Status.HostIPs = []corev1.HostIP{{IP: address}}
	}
}

// refusesNode reports whether the policy of a sandbox does not permit the
// address of a node.
func (e *testEnv) refusesNode(req backend.Request, address string) bool {
	e.t.Helper()
	return slices.Contains(e.policyOf(req).Spec.Egress[1].To[0].IPBlock.Except, address+"/32")
}

// startOnNode starts a sandbox on a node and releases its lease.
func (e *testEnv) startOnNode(req backend.Request, node string) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, e.m, req)
	waitFor(e.t, "pod", func() bool { return e.m.pod(req.Ref.Name()) != nil })
	e.setStatus(req.Ref.Name(), onPublicNode(node))
	r := <-res
	if r.err != nil {
		e.t.Fatal(r.err)
	}
	r.lease.Release()
}

// completeRun does one complete run of the reaper, and waits until the pod
// events of that run (the activity time) are in the cache. It then removes
// a request for a run that is pending.
func (e *testEnv) completeRun(reqs ...backend.Request) {
	e.t.Helper()
	e.m.Reap(context.Background())
	for _, req := range reqs {
		waitFor(e.t, "activity time in the cache", func() bool {
			p := e.m.pod(req.Ref.Name())
			return p != nil && p.Annotations[AnnotationActivity] != ""
		})
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-e.m.kick:
	default:
	}
}

// asksForRun reports whether a pod event asked for a run of the reaper.
func (e *testEnv) asksForRun() bool {
	e.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(e.m.kick) > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// A run of the reaper removes the address of a node that has no sandbox from
// the policies. If one of its writes fails, the record of the node addresses
// must not keep that address: some policies do not have it. A new pod on that
// node must then ask for a run of the reaper.
func TestNewPodOnANodeThatAnIncompleteRunRemoved(t *testing.T) {
	e := newTestEnv(t, nil)
	ctx := context.Background()
	carol, dan, yuri, xena := userRef("carol"), userRef("dan"), userRef("yuri"), userRef("xena")
	e.startOnNode(carol, publicNodeB)
	e.startOnNode(dan, publicNodeB)
	e.startOnNode(yuri, publicNodeA)
	e.completeRun(carol, dan, yuri)
	if !e.refusesNode(carol, publicNodeA) || !e.refusesNode(dan, publicNodeA) {
		t.Fatal("the test is wrong: the policies do not have the address of node A")
	}

	// The last sandbox of node A goes away. The next run removes the address
	// of node A. Its update request for the policy of dan fails.
	e.stop(yuri)
	refused := e.policyOf(dan).Name
	var refuse atomic.Bool
	refuse.Store(true)
	e.client.PrependReactor("update", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if refuse.Load() && a.(k8stesting.UpdateAction).GetObject().(*networkingv1.NetworkPolicy).Name == refused {
			return true, nil, errors.New("the server is not available")
		}
		return false, nil, nil
	})
	e.m.Reap(ctx)
	refuse.Store(false)
	if e.refusesNode(carol, publicNodeA) {
		t.Fatal("the test is wrong: the run did not remove the address of node A from the policy of carol")
	}
	if n := len(e.m.kick); n != 0 {
		t.Fatalf("the test is wrong: %d request for a run is pending before the new pod", n)
	}

	// A new sandbox starts on node A, and it gets its first lease.
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res := e.acquireAsync(c, e.m, xena)
	waitFor(t, "pod of xena", func() bool { return e.m.pod(xena.Ref.Name()) != nil })
	e.setStatus(xena.Ref.Name(), onPublicNode(publicNodeA))
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.lease.Release()
	if !e.asksForRun() {
		t.Error("no event of the pod on node A asked for a run of the reaper, and the policy of carol permits the address of node A")
	}
	// That run puts the address into the policies of the other sandboxes.
	e.m.Reap(ctx)
	if !e.refusesNode(carol, publicNodeA) || !e.refusesNode(dan, publicNodeA) {
		t.Error("the policies of the other sandboxes permit the address of node A after the next run")
	}
}

// A new pod can get a node, become ready, and get its first lease while a
// run of the reaper writes the policies without the address of that node.
// The record of the node addresses must not have the address in that time:
// the event of the pod must ask for one more run.
func TestNewPodOnANodeThatARunRemovesAtThisTime(t *testing.T) {
	var before atomic.Pointer[func(string)]
	e := newTestEnv(t, func(c *Config) {
		c.Client = slowRequests{Interface: c.Client, beforeUpdate: func(name string) {
			if f := before.Load(); f != nil {
				(*f)(name)
			}
		}}
	})
	ctx := context.Background()
	carol, yuri, xena := userRef("carol"), userRef("yuri"), userRef("xena")
	e.startOnNode(carol, publicNodeB)
	e.startOnNode(yuri, publicNodeA)
	e.completeRun(carol, yuri)
	if !e.refusesNode(carol, publicNodeA) {
		t.Fatal("the test is wrong: the policy of carol does not have the address of node A")
	}

	// The last sandbox of node A goes away. A new sandbox starts: its pod
	// has no node.
	e.stop(yuri)
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res := e.acquireAsync(c, e.m, xena)
	waitFor(t, "pod of xena", func() bool { return e.m.pod(xena.Ref.Name()) != nil })
	if n := len(e.m.kick); n != 0 {
		t.Fatalf("the test is wrong: %d request for a run is pending before the run", n)
	}

	target := e.policyOf(carol).Name
	var done atomic.Bool
	var lease atomic.Pointer[backend.Lease]
	hook := func(name string) {
		if name != target || !done.CompareAndSwap(false, true) {
			return
		}
		// The update request of the reaper for the policy of carol is in
		// progress: the run removes the address of node A.
		e.setStatus(xena.Ref.Name(), onPublicNode(publicNodeA))
		r := <-res
		if r.err != nil {
			t.Error(r.err)
			return
		}
		lease.Store(&r.lease)
	}
	before.Store(&hook)
	e.m.Reap(ctx)
	before.Store(nil)
	l := lease.Load()
	if l == nil {
		t.Fatal("no lease")
	}
	defer (*l).Release()
	if e.refusesNode(carol, publicNodeA) {
		t.Fatal("the test is wrong: the policy of carol has the address of node A after the run")
	}
	if !e.asksForRun() {
		t.Error("no event of the pod on node A asked for a run of the reaper, and the policy of carol permits the address of node A")
	}
	e.m.Reap(ctx)
	if !e.refusesNode(carol, publicNodeA) {
		t.Error("the policy of carol permits the address of node A after the next run")
	}
}
