package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"slices"
	"sort"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/h3xcat/warren/internal/policy"
)

// NetworkConfig has the data that warren needs to make the network policies
// of the sandboxes.
type NetworkConfig struct {
	// WarrenNamespace and WarrenPodLabels identify the pods of warren. Only
	// these pods can connect to a sandbox.
	WarrenNamespace string
	WarrenPodLabels map[string]string
	// IngressCIDRs has more sources that can connect to a sandbox. Use it
	// when warren is not a pod of the cluster.
	IngressCIDRs []string
	// DNSNamespace and DNSPodLabels identify the DNS pods of the cluster.
	DNSNamespace string
	DNSPodLabels map[string]string
	// DNSCIDRs has more DNS servers, for example a DNS cache on each node.
	DNSCIDRs []string
	// DNSPorts has the ports of the DNS pods. The default is 53. Most
	// network plugins examine a connection after the change of the service
	// address to the pod address, thus the port of the pod is the correct
	// one (for example 5353 on OpenShift).
	DNSPorts []int32
	// AcceptedPolicies has the names of network policies in the sandbox
	// namespace that warren did not make, and that the administrator
	// accepts. A different policy with a rule stops the start of sandboxes.
	AcceptedPolicies []string
	// AllowOtherPods lets warren make the policy that refuses all
	// connections in a namespace that has pods of a different owner.
	AllowOtherPods bool
	// SkipEnforcementCheck turns off the test that shows whether the
	// cluster applies network policies.
	SkipEnforcementCheck bool

	// blocked has the networks of Blocked. validate makes it.
	blocked []netip.Prefix
	// BlockedCIDRs has more networks that the "internet" mode must not
	// permit, for example the public addresses of the nodes.
	BlockedCIDRs []string
}

// blockedIPv4 and blockedIPv6 are the networks that the "internet" mode does
// not permit: private networks, addresses of the same host or link (the
// metadata service of a cloud is one of them), and special networks.
var (
	blockedIPv4 = []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		// The platform service of Azure. Each node of Azure can connect to
		// it, and it has a public address.
		"168.63.129.16/32",
	}
	blockedIPv6 = []string{
		"::/128", "::1/128",
		// The local NAT64 network and 6to4. Their addresses contain IPv4
		// addresses. Kubernetes does not accept the network ::ffff:0:0/96 in
		// a policy: such an address is never in a packet on the network.
		"64:ff9b:1::/48", "2002::/16",
		"fc00::/7", "fe80::/10", "ff00::/8",
	}
)

// nat64 returns the IPv6 network that a NAT64 gateway changes into the given
// IPv4 network. A sandbox must not get to a private IPv4 network through
// such a gateway. The public IPv4 addresses stay possible.
func nat64(cidr string) (string, bool) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() {
		return "", false
	}
	v4 := prefix.Addr().As4()
	addr := netip.AddrFrom16([16]byte{0x00, 0x64, 0xff, 0x9b, 0, 0, 0, 0, 0, 0, 0, 0, v4[0], v4[1], v4[2], v4[3]})
	return netip.PrefixFrom(addr, 96+prefix.Bits()).String(), true
}

// blockedNetworks returns the networks that the "internet" mode does not
// permit, for IPv4 and for IPv6. extra has more networks: the networks of
// the configuration and the addresses of the nodes that warren found.
func blockedNetworks(extra []string) (v4, v6 []string) {
	seen := map[string]bool{}
	add := func(cidr string) {
		if seen[cidr] {
			return
		}
		seen[cidr] = true
		if isIPv6(cidr) {
			v6 = append(v6, cidr)
			return
		}
		v4 = append(v4, cidr)
		if mapped, ok := nat64(cidr); ok && !seen[mapped] {
			seen[mapped] = true
			v6 = append(v6, mapped)
		}
	}
	for _, cidr := range blockedIPv4 {
		add(cidr)
	}
	for _, cidr := range blockedIPv6 {
		add(cidr)
	}
	for _, cidr := range extra {
		add(cidr)
	}
	sort.Strings(v4)
	sort.Strings(v6)
	return v4, v6
}

// Blocked reports whether the "internet" mode refuses a connection to an
// address, with the networks of the configuration.
func (c NetworkConfig) Blocked(addr netip.Addr) bool {
	list := c.blocked
	if list == nil {
		list = c.blockedPrefixes()
	}
	for _, prefix := range list {
		if prefix.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

func (c NetworkConfig) blockedPrefixes() []netip.Prefix {
	v4, v6 := blockedNetworks(c.BlockedCIDRs)
	var out []netip.Prefix
	for _, cidr := range append(v4, v6...) {
		if prefix, err := netip.ParsePrefix(cidr); err == nil {
			out = append(out, prefix)
		}
	}
	return out
}

// validate examines the configuration and gives the defaults.
func (c *NetworkConfig) validate(sandboxNamespace string) error {
	if c.WarrenNamespace == "" && len(c.IngressCIDRs) == 0 {
		return errors.New("kube: the namespace of warren or an ingress network is necessary, if not no request can get to a sandbox")
	}
	if c.WarrenNamespace != "" && len(c.WarrenPodLabels) == 0 {
		return errors.New("kube: the pod labels of warren are necessary")
	}
	// The sandbox namespace has a policy that refuses all connections of
	// all its pods. Warren must not be one of them, and a sandbox must not
	// be in the namespace that has the connection key.
	if c.WarrenNamespace == sandboxNamespace {
		return fmt.Errorf("kube: the sandbox namespace %q is the namespace of warren. The sandboxes must have their own namespace", sandboxNamespace)
	}
	if c.DNSNamespace == "" {
		c.DNSNamespace = "kube-system"
	}
	if len(c.DNSPodLabels) == 0 {
		c.DNSPodLabels = map[string]string{"k8s-app": "kube-dns"}
	}
	if len(c.DNSPorts) == 0 {
		c.DNSPorts = []int32{53}
	}
	for _, port := range c.DNSPorts {
		if port < 1 || port > 65535 {
			return fmt.Errorf("kube: %d is not a DNS port", port)
		}
	}
	c.blocked = c.blockedPrefixes()
	// An empty selector selects all pods. A rule must never have one.
	for name, labels := range map[string]map[string]string{"warren": c.WarrenPodLabels, "DNS": c.DNSPodLabels} {
		for key, value := range labels {
			if key == "" || value == "" {
				return fmt.Errorf("kube: a %s pod label has an empty key or value", name)
			}
		}
	}
	for name, list := range map[string][]string{"ingress": c.IngressCIDRs, "DNS": c.DNSCIDRs, "blocked": c.BlockedCIDRs} {
		for _, cidr := range list {
			prefix, err := netip.ParsePrefix(cidr)
			// Kubernetes does not accept an IPv4 address in IPv6 form, and
			// an exception cannot be all addresses.
			if err != nil || prefix.Masked() != prefix || prefix.Addr().Is4In6() || prefix.Bits() == 0 {
				return fmt.Errorf("kube: the %s network %q is not a network, for example 10.20.0.0/16", name, cidr)
			}
		}
	}
	return nil
}

// isIPv6 reports whether a network that validate accepted is an IPv6 network.
func isIPv6(cidr string) bool {
	prefix, err := netip.ParsePrefix(cidr)
	return err == nil && prefix.Addr().Is6() && !prefix.Addr().Is4In6()
}

// buildDefaultDeny makes the policy that refuses all connections to and from
// all pods of the sandbox namespace. The policy of each sandbox then permits
// the connections of that sandbox. Without this policy, a pod that has no
// policy of its own could connect to all addresses.
func (m *Manager) buildDefaultDeny() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultDenyName,
			Namespace: m.cfg.Namespace,
			Labels:    map[string]string{LabelManagedBy: ManagedByValue},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

// buildNetworkPolicy makes the policy of one start of a sandbox. The policy
// has the name of the Secret of that start, and it selects the pod of that
// start only.
//
// hostAddresses has the addresses of the nodes that warren found. A node can
// have a public address, and the kubelet and other services of the node
// listen on it. The "internet" mode must not permit them.
func (m *Manager) buildNetworkPolicy(start string, podLabels map[string]string, pol policy.Policy, hostAddresses []string) *networkingv1.NetworkPolicy {
	net := m.cfg.Network
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	port := func(protocol *corev1.Protocol, n int32) networkingv1.NetworkPolicyPort {
		p := intstr.FromInt32(n)
		return networkingv1.NetworkPolicyPort{Protocol: protocol, Port: &p}
	}
	block := func(cidr string, except []string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr, Except: except}}
	}

	// Ingress: only warren, and only to the port of the server.
	var from []networkingv1.NetworkPolicyPeer
	if net.WarrenNamespace != "" {
		from = append(from, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: net.WarrenNamespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: net.WarrenPodLabels},
		})
	}
	for _, cidr := range net.IngressCIDRs {
		from = append(from, block(cidr, nil))
	}
	ingress := []networkingv1.NetworkPolicyIngressRule{{
		From:  from,
		Ports: []networkingv1.NetworkPolicyPort{port(&tcp, terminalPort)},
	}}

	egress := []networkingv1.NetworkPolicyEgressRule{}
	if pol.Network.AllowsDNS() {
		to := []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: net.DNSNamespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: net.DNSPodLabels},
		}}
		for _, cidr := range net.DNSCIDRs {
			to = append(to, block(cidr, nil))
		}
		rule := networkingv1.NetworkPolicyEgressRule{To: to}
		for _, n := range net.DNSPorts {
			rule.Ports = append(rule.Ports, port(&udp, n), port(&tcp, n))
		}
		egress = append(egress, rule)
	}
	if pol.Network.Egress == policy.EgressInternet {
		v4, v6 := blockedNetworks(append(append([]string{}, net.BlockedCIDRs...), hostAddresses...))
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{block("0.0.0.0/0", v4), block("::/0", v6)},
		})
	}
	for _, d := range pol.Network.Allow {
		var peer networkingv1.NetworkPolicyPeer
		if d.CIDR != "" {
			peer = block(d.CIDR, nil)
		} else {
			// A destination in the cluster. An address of a pod can change,
			// and some network plugins do not compare a network with the
			// address of a pod.
			peer = networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: d.Namespace}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: d.PodLabels},
			}
		}
		rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{peer}}
		for _, n := range d.Ports {
			rule.Ports = append(rule.Ports, port(&tcp, n), port(&udp, n))
		}
		egress = append(egress, rule)
	}

	labels := map[string]string{LabelManagedBy: ManagedByValue}
	for _, key := range []string{LabelTemplate, LabelScope, LabelOwner, LabelSandbox} {
		if v, ok := podLabels[key]; ok {
			labels[key] = v
		}
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: start, Namespace: m.cfg.Namespace, Labels: labels,
			Annotations: map[string]string{AnnotationNetwork: networkRecord(pol.Network)},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelStart: start}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

// ensureDefaultDeny makes the policy that refuses all connections in the
// sandbox namespace, or repairs it. No sandbox starts before this function
// was successful one time.
func (m *Manager) ensureDefaultDeny(ctx context.Context) error {
	policies := m.cfg.Client.NetworkingV1().NetworkPolicies(m.cfg.Namespace)
	want := m.buildDefaultDeny()
	cur, err := policies.Get(ctx, defaultDenyName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// The policy stops all connections of all pods of the namespace.
		// Before warren makes it the first time, make sure that the
		// namespace has no pods of a different owner.
		if err := m.otherPods(ctx); err != nil {
			return err
		}
		if _, err = policies.Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("make the network policy %s: %w", defaultDenyName, err)
		}
		m.cfg.Logger.Info("network policy made", "log", "audit", "policy", defaultDenyName)
	case err != nil:
		return fmt.Errorf("read the network policy %s: %w", defaultDenyName, err)
	case !apiequality.Semantic.DeepEqual(cur.Spec, want.Spec):
		// A person or a different program changed the policy. It could now
		// permit connections for all sandboxes.
		cur.Spec = want.Spec
		if _, err = policies.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("repair the network policy %s: %w", defaultDenyName, err)
		}
		m.cfg.Logger.Warn("the network policy that refuses all connections was changed. Warren repaired it.",
			"log", "audit", "policy", defaultDenyName)
	}
	m.networkReady.Store(true)
	return nil
}

// otherPods returns an error if the sandbox namespace has pods that warren
// did not make.
func (m *Manager) otherPods(ctx context.Context) error {
	if m.cfg.Network.AllowOtherPods {
		return nil
	}
	list, err := m.cfg.Client.CoreV1().Pods(m.cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list the pods of the sandbox namespace: %w", err)
	}
	var names []string
	for _, pod := range list.Items {
		if pod.Labels[LabelManagedBy] != ManagedByValue && pod.Labels[LabelCanary] == "" {
			names = append(names, pod.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	if len(names) > 5 {
		names = append(names[:5], "...")
	}
	return fmt.Errorf("the sandbox namespace %s has pods that warren did not make (%s). The network policy of warren would stop all their connections. "+
		"Give the sandboxes a namespace of their own", m.cfg.Namespace, strings.Join(names, ", "))
}

// reconcileNetworkPolicies makes sure that each pod has the network policy
// of the policy file of this moment. Thus a change of the network settings
// applies to the sandboxes that operate, and a policy that a person deleted
// comes back. It also deletes the network policies that have no pod.
func (m *Manager) reconcileNetworkPolicies(ctx context.Context, pods []*corev1.Pod, live map[string]bool) {
	policies := m.cfg.Client.NetworkingV1().NetworkPolicies(m.cfg.Namespace)
	// List all policies of the namespace, not only the policies of warren.
	list, err := policies.List(ctx, metav1.ListOptions{})
	if err != nil {
		m.cfg.Logger.Warn("cannot list the network policies", "error", err.Error())
		return
	}
	have := map[string]*networkingv1.NetworkPolicy{}
	var others []string
	for i := range list.Items {
		np := &list.Items[i]
		if np.Labels[LabelManagedBy] == ManagedByValue {
			have[np.Name] = np
			continue
		}
		// A network policy can only permit connections. A policy with no
		// rule permits nothing, thus it changes nothing.
		if len(np.Spec.Ingress) == 0 && len(np.Spec.Egress) == 0 {
			continue
		}
		if !slices.Contains(m.cfg.Network.AcceptedPolicies, np.Name) {
			others = append(others, np.Name)
		}
	}
	// A policy of a different owner can permit what the policies of warren
	// refuse, for example connections between all pods of the namespace.
	sort.Strings(others)
	m.otherPolicies.Store(int32(len(others)))
	if len(others) > 0 {
		m.cfg.Logger.Error("the sandbox namespace has network policies that warren did not make. They can permit connections that warren refuses. "+
			"No new sandbox starts until they are gone, or until the configuration of warren accepts them (--accepted-network-policies).", "policies", others)
	}
	hosts := m.hostAddresses()

	for _, pod := range pods {
		start := pod.Labels[LabelStart]
		if start == "" || pod.DeletionTimestamp != nil {
			continue
		}
		pol, ok := m.cfg.Policies.Get(pod.Labels[LabelTemplate])
		if !ok || m.orphan(pod.Labels) {
			// The pod has no policy at this time. Its network policy stays
			// as it is until the reaper stops the pod.
			continue
		}
		want := m.buildNetworkPolicy(start, pod.Labels, pol, hosts)
		want.OwnerReferences = []metav1.OwnerReference{podOwner(pod)}
		cur := have[start]
		switch {
		case cur == nil:
			if _, err := policies.Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
				m.cfg.Logger.Warn("cannot make the network policy of a sandbox", "sandbox", pod.Name, "error", err.Error())
				continue
			}
			m.cfg.Logger.Warn("a sandbox had no network policy. Warren made it again.", "log", "audit", "sandbox", pod.Name, "policy", start)
		case !apiequality.Semantic.DeepEqual(cur.Spec, want.Spec) || cur.Annotations[AnnotationNetwork] != want.Annotations[AnnotationNetwork] ||
			len(cur.OwnerReferences) == 0:
			// Did the administrator remove a permission? A network plugin
			// can let an open connection continue after a change of the
			// policy. Only a stop of the pod closes such a connection.
			removed := narrower(cur.Annotations[AnnotationNetwork], pol.Network)
			cur.Spec = want.Spec
			// With the pod as owner, Kubernetes deletes the policy with
			// the pod.
			cur.OwnerReferences = want.OwnerReferences
			if cur.Annotations == nil {
				cur.Annotations = map[string]string{}
			}
			cur.Annotations[AnnotationNetwork] = want.Annotations[AnnotationNetwork]
			if _, err := policies.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
				m.cfg.Logger.Warn("cannot change the network policy of a sandbox", "sandbox", pod.Name, "error", err.Error())
				continue
			}
			m.cfg.Logger.Info("network policy of a sandbox changed", "log", "audit", "sandbox", pod.Name, "policy", start,
				"template", pod.Labels[LabelTemplate], "restart", removed)
			if removed {
				_ = m.deletePod(ctx, pod, "network_changed", false)
			}
		}
	}

	now := m.now()
	for name, np := range have {
		if name == defaultDenyName || live[name] || now.Sub(np.CreationTimestamp.Time) < orphanAge {
			continue
		}
		// A start request makes the network policy before the pod.
		m.mu.Lock()
		st := m.sandboxes[np.Labels[LabelSandbox]]
		starting := st != nil && (st.starting || st.creating)
		m.mu.Unlock()
		if starting {
			continue
		}
		if err := policies.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &np.UID}}); err == nil {
			m.cfg.Logger.Info("deleted a network policy that had no sandbox", "policy", name)
		}
	}
}

// networkRecord is the form of the network settings in the annotation of a
// network policy.
type networkRecordData struct {
	Internet bool                 `json:"internet"`
	DNS      bool                 `json:"dns"`
	Allow    []policy.Destination `json:"allow,omitempty"`
}

func networkRecord(n policy.Network) string {
	data, _ := json.Marshal(networkRecordData{Internet: n.Egress == policy.EgressInternet, DNS: n.AllowsDNS(), Allow: n.Allow})
	return string(data)
}

// narrower reports whether new network settings remove a permission of the
// settings in a record. A record that warren cannot read gives false: the
// settings did not come from a policy file.
func narrower(record string, now policy.Network) bool {
	var old networkRecordData
	if err := json.Unmarshal([]byte(record), &old); err != nil {
		return false
	}
	if (old.Internet && now.Egress != policy.EgressInternet) || (old.DNS && !now.AllowsDNS()) {
		return true
	}
	for _, d := range old.Allow {
		found := false
		for _, n := range now.Allow {
			if reflect.DeepEqual(d, n) {
				found = true
			}
		}
		if !found {
			return true
		}
	}
	return false
}

// hostAddresses returns the addresses of the nodes that have a sandbox pod,
// each as a network of one address.
func (m *Manager) hostAddresses() []string {
	seen := map[string]bool{}
	for _, obj := range m.informer.GetStore().List() {
		pod := obj.(*corev1.Pod)
		ips := []string{pod.Status.HostIP}
		for _, h := range pod.Status.HostIPs {
			ips = append(ips, h.IP)
		}
		for _, ip := range ips {
			addr, err := netip.ParseAddr(ip)
			if err != nil {
				continue
			}
			seen[netip.PrefixFrom(addr, addr.BitLen()).String()] = true
		}
	}
	out := make([]string, 0, len(seen))
	for cidr := range seen {
		out = append(out, cidr)
	}
	sort.Strings(out)
	return out
}

// podOwner returns the owner reference for an object that must go away with
// a pod.
func podOwner(pod *corev1.Pod) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}
}

// SelfCheck examines the pod of warren itself, before warren starts a
// sandbox. A wrong configuration here has a bad result with no message: no
// request gets to a sandbox, or a sandbox can connect to the cluster.
//
// podName is the name of the pod of warren. If warren is not a pod, or if
// warren cannot read its pod, the function examines what it can.
func SelfCheck(ctx context.Context, client kubernetes.Interface, podName string, net NetworkConfig, serviceHost string, logger *slog.Logger) error {
	hint := "Add the network to the blocked networks of the configuration (--egress-blocked-cidrs)."
	if addr, err := netip.ParseAddr(serviceHost); err == nil && !net.Blocked(addr) {
		return fmt.Errorf("the address %s of the Kubernetes API is not in a network that the internet mode refuses. "+
			"A sandbox could connect to the services of the cluster. %s", addr, hint)
	}
	if net.WarrenNamespace == "" || podName == "" {
		return nil
	}
	pod, err := client.CoreV1().Pods(net.WarrenNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		logger.Warn("warren cannot read its own pod, thus it cannot examine its labels and its address. "+
			"If no request gets to a sandbox, examine the pod labels in the configuration of warren.", "pod", podName, "error", err.Error())
		return nil
	}
	for key, value := range net.WarrenPodLabels {
		if pod.Labels[key] != value {
			return fmt.Errorf("the pod of warren does not have the label %s=%s of the configuration (--warren-pod-labels). "+
				"The network policy of a sandbox would refuse warren", key, value)
		}
	}
	if pod.Spec.HostNetwork {
		return errors.New("the pod of warren uses the network of its node (hostNetwork). " +
			"A network policy cannot select such a pod, thus no request would get to a sandbox")
	}
	for _, ip := range pod.Status.PodIPs {
		if addr, err := netip.ParseAddr(ip.IP); err == nil && !net.Blocked(addr) {
			return fmt.Errorf("the pod address %s is not in a network that the internet mode refuses. "+
				"A sandbox could connect to the other pods of the cluster. %s", addr, hint)
		}
	}
	return nil
}

// requiredPermissions are the operations that warren does in the sandbox
// namespace.
var requiredPermissions = []struct {
	group, resource string
	verbs           []string
}{
	{"", "pods", []string{"get", "list", "watch", "create", "delete", "patch"}},
	{"", "secrets", []string{"get", "list", "create", "delete", "patch"}},
	{"", "persistentvolumeclaims", []string{"get", "list", "watch", "create", "delete", "patch"}},
	{"networking.k8s.io", "networkpolicies", []string{"get", "list", "create", "update", "patch", "delete"}},
}

// MissingPermissions asks Kubernetes which of the necessary permissions the
// service account of warren does not have in the sandbox namespace. With a
// permission that is absent, warren would fail later, and some failures have
// no message: for example, a change of the policy file would not get to the
// network policies.
func MissingPermissions(ctx context.Context, client kubernetes.Interface, namespace string) ([]string, error) {
	var missing []string
	for _, p := range requiredPermissions {
		for _, verb := range p.verbs {
			review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Group: p.group, Resource: p.resource, Verb: verb},
			}}
			result, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
			if err != nil {
				return nil, err
			}
			if !result.Status.Allowed {
				missing = append(missing, verb+" "+p.resource)
			}
		}
	}
	return missing, nil
}
