// Package policy reads the named sandbox policies.
//
// An administrator defines policies in a YAML file. Warren shows each policy
// to Open WebUI as one read-only template (docs/protocol.md, section 4.6).
// docs/policies.md describes the file.
package policy

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/coderpunch/warren/internal/scope"
)

// Policy is one named set of sandbox settings.
type Policy struct {
	// ID is the template ID that Open WebUI sends in X-Terminal-Template-Id.
	ID string
	// Name is the name that the user sees.
	Name string
	// Scope is user, chat, model, or shared.
	Scope scope.Kind
	// Users is the list of the user IDs that can use the policy. An empty
	// list permits each user that Open WebUI lets through.
	Users []string
	// Spec has the settings of the sandbox.
	Spec
}

// Spec has the settings that a policy and the defaults of the file can set.
// A zero value means "not set".
type Spec struct {
	// Image is the container image. Empty means the default image of warren.
	Image            string
	ImagePullPolicy  corev1.PullPolicy
	ImagePullSecrets []string
	RuntimeClassName string
	// Resources has the requests and the limits for cpu, memory, and
	// ephemeral-storage.
	Resources corev1.ResourceRequirements
	Storage   Storage
	// Env has more environment variables for the sandbox.
	Env map[string]string
	// IdleTimeout is the time after which warren stops a sandbox that had
	// no use. Nil means the default of warren. Zero means never.
	IdleTimeout  *time.Duration
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
	// Network has the network connections that the sandbox can open.
	Network Network
	// Command is the start command of the sandbox. Nil means the default
	// command of warren, which is correct for the default image.
	Command []string
	// RunAsUser is the user ID of the processes in the sandbox. Nil means
	// the default of warren.
	RunAsUser *int64
	// UserNamespace gives the sandbox its own user namespace: the user IDs
	// of the sandbox are then different user IDs on the node, and different
	// for each sandbox. The cluster must support it. Nil means no.
	UserNamespace *bool
}

// EgressMode says which connections a sandbox can open by default.
type EgressMode string

const (
	// EgressInternet permits public addresses. Private addresses and the
	// addresses of the cluster stay blocked.
	EgressInternet EgressMode = "internet"
	// EgressNone permits no connection.
	EgressNone EgressMode = "none"
)

// Network has the settings for the outgoing connections of a sandbox. A
// sandbox never accepts a connection, except from warren.
type Network struct {
	Egress EgressMode
	// DNS permits name lookups with the DNS service of the cluster. Nil
	// means the default: yes for EgressInternet, no for EgressNone.
	DNS *bool
	// Allow has more destinations. A destination can be a private address.
	Allow []Destination
}

// Destination is one destination that a sandbox can connect to: a network,
// or the pods of the cluster that have some labels. It has the CIDR, or the
// Namespace and the PodLabels.
type Destination struct {
	// CIDR is the network, for example 10.20.0.0/16 or 2001:db8::/32. Use
	// it for a destination that is not in the cluster.
	CIDR string
	// Namespace and PodLabels select pods of the cluster.
	Namespace string
	PodLabels map[string]string
	// Ports limits the connection to these TCP and UDP ports. An empty
	// list permits all ports.
	Ports []int32
}

// AllowsDNS reports whether the sandbox can use the DNS service of the
// cluster.
func (n Network) AllowsDNS() bool {
	if n.DNS != nil {
		return *n.DNS
	}
	return n.Egress != EgressNone
}

// Profile returns the name of the network settings that Open WebUI shows.
// "none" means no connection at all, also no DNS.
func (n Network) Profile() string {
	switch {
	case len(n.Allow) > 0:
		return "custom"
	case n.Egress == EgressNone && !n.AllowsDNS():
		return string(EgressNone)
	case n.Egress != EgressNone && n.AllowsDNS():
		return string(EgressInternet)
	}
	return "custom"
}

// set reports whether the network settings have a value.
func (n Network) set() bool {
	return n.Egress != "" || n.DNS != nil || n.Allow != nil
}

// StorageType is the type of the home directory of a sandbox.
type StorageType string

const (
	// StoragePersistent keeps the home directory on a volume that stays
	// when the sandbox stops.
	StoragePersistent StorageType = "persistent"
	// StorageEphemeral deletes the home directory when the sandbox stops.
	StorageEphemeral StorageType = "ephemeral"
)

// Storage has the settings of the home directory.
type Storage struct {
	Type StorageType
	// Size is the size of the home directory.
	Size *resource.Quantity
	// StorageClassName is the storage class of the volume. Empty means the
	// default class of the cluster.
	StorageClassName string
	// Retention is the time that warren keeps a volume that no sandbox
	// uses. Nil or zero means that warren keeps the volume.
	Retention *time.Duration
}

// Persistent reports whether the home directory stays when the sandbox stops.
func (p Policy) Persistent() bool { return p.Storage.Type == StoragePersistent }

// Allows reports whether a user can use the policy.
func (p Policy) Allows(userID string) bool {
	return len(p.Users) == 0 || slices.Contains(p.Users, userID)
}

// Set is a group of policies with unique IDs.
type Set struct {
	byID  map[string]Policy
	order []string
}

var idPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Load reads and examines a policy file.
func Load(path string) (*Set, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	set, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("policy file %s: %w", path, err)
	}
	return set, nil
}

// Parse examines the content of a policy file. The rules are strict: an
// unknown key, a duplicate key, a second YAML document, or a value of the
// wrong type is an error.
func Parse(data []byte) (*Set, error) {
	defaults, policies, lines, err := decodeFile(data)
	if err != nil {
		return nil, err
	}
	for i := range policies {
		spec := defaults.clone()
		spec.overlay(policies[i].Spec)
		policies[i].Spec = spec
	}
	return newSet(policies, lines)
}

// NewSet makes a set from a list of policies. It gives the built-in default
// to each setting that has no value.
func NewSet(policies []Policy) (*Set, error) { return newSet(policies, nil) }

// newSet makes a set. lines has the line of each policy in the file. It is
// nil for policies that do not come from a file.
func newSet(policies []Policy, lines []int) (*Set, error) {
	if len(policies) == 0 {
		return nil, errors.New("no policies")
	}
	where := func(i int) string {
		if lines == nil {
			return fmt.Sprintf("policy %d", i+1)
		}
		return fmt.Sprintf("line %d: policy %d", lines[i], i+1)
	}
	s := &Set{byID: make(map[string]Policy, len(policies))}
	for i, p := range policies {
		p.Spec = p.Spec.clone()
		p.complete()
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("%s (%q): %w", where(i), p.ID, err)
		}
		if _, dup := s.byID[p.ID]; dup {
			return nil, fmt.Errorf("%s: duplicate ID %q", where(i), p.ID)
		}
		s.byID[p.ID] = p
		s.order = append(s.order, p.ID)
	}
	sort.Strings(s.order)
	return s, nil
}

// Built-in defaults. A sandbox with no limits can use all resources of its
// node, thus each sandbox gets limits unless the file sets other values.
var (
	defaultRequests = corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("100m"),
		corev1.ResourceMemory:           resource.MustParse("256Mi"),
		corev1.ResourceEphemeralStorage: resource.MustParse("100Mi"),
	}
	defaultLimits = corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("2"),
		corev1.ResourceMemory:           resource.MustParse("2Gi"),
		corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
	}
	defaultStorageSize = resource.MustParse("1Gi")
)

// complete gives the built-in default to each setting that has no value.
func (p *Policy) complete() {
	if p.Storage.Type == "" {
		if unboundedScope(p.Scope) {
			p.Storage.Type = StorageEphemeral
		} else {
			p.Storage.Type = StoragePersistent
		}
	}
	if p.Storage.Size == nil {
		q := defaultStorageSize.DeepCopy()
		p.Storage.Size = &q
	}
	if p.Network.Egress == "" {
		p.Network.Egress = EgressInternet
	}
	if p.Resources.Requests == nil {
		p.Resources.Requests = corev1.ResourceList{}
	}
	if p.Resources.Limits == nil {
		p.Resources.Limits = corev1.ResourceList{}
	}
	for name, limit := range defaultLimits {
		if _, set := p.Resources.Limits[name]; !set {
			p.Resources.Limits[name] = limit.DeepCopy()
		}
	}
	for name, request := range defaultRequests {
		if _, set := p.Resources.Requests[name]; set {
			continue
		}
		// A default request must not be more than the limit of the file.
		if limit, ok := p.Resources.Limits[name]; ok && limit.Cmp(request) < 0 {
			request = limit
		}
		p.Resources.Requests[name] = request.DeepCopy()
	}
}

// unboundedScope reports whether a scope can have very many sandboxes for
// one user. A user makes a new chat at each time, and Open WebUI does not
// examine each model ID (docs/protocol.md, section 6). A volume for each
// chat or model is thus not the default, and it must have an end.
func unboundedScope(k scope.Kind) bool {
	return k == scope.KindChat || k == scope.KindModel
}

// clone returns a copy that shares no map and no list with s.
func (s Spec) clone() Spec {
	out := s
	out.ImagePullSecrets = slices.Clone(s.ImagePullSecrets)
	out.Resources = *s.Resources.DeepCopy()
	if s.Storage.Size != nil {
		q := s.Storage.Size.DeepCopy()
		out.Storage.Size = &q
	}
	if s.Storage.Retention != nil {
		d := *s.Storage.Retention
		out.Storage.Retention = &d
	}
	out.Env = cloneMap(s.Env)
	if s.IdleTimeout != nil {
		d := *s.IdleTimeout
		out.IdleTimeout = &d
	}
	out.NodeSelector = cloneMap(s.NodeSelector)
	if s.Network.DNS != nil {
		b := *s.Network.DNS
		out.Network.DNS = &b
	}
	if s.Network.Allow != nil {
		out.Network.Allow = make([]Destination, len(s.Network.Allow))
		for i, d := range s.Network.Allow {
			out.Network.Allow[i] = Destination{CIDR: d.CIDR, Namespace: d.Namespace, PodLabels: cloneMap(d.PodLabels), Ports: slices.Clone(d.Ports)}
		}
	}
	out.Command = slices.Clone(s.Command)
	if s.RunAsUser != nil {
		u := *s.RunAsUser
		out.RunAsUser = &u
	}
	if s.UserNamespace != nil {
		b := *s.UserNamespace
		out.UserNamespace = &b
	}
	if s.Tolerations != nil {
		out.Tolerations = make([]corev1.Toleration, len(s.Tolerations))
		for i := range s.Tolerations {
			out.Tolerations[i] = *s.Tolerations[i].DeepCopy()
		}
	}
	return out
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// overlay puts each setting of src that has a value into s. The environment
// variables and the resources get the keys of src. Each other list or
// mapping of src replaces the value of s: a node selector with one key more
// selects different nodes.
func (s *Spec) overlay(src Spec) {
	src = src.clone()
	if src.Image != "" {
		s.Image = src.Image
	}
	if src.ImagePullPolicy != "" {
		s.ImagePullPolicy = src.ImagePullPolicy
	}
	if src.ImagePullSecrets != nil {
		s.ImagePullSecrets = src.ImagePullSecrets
	}
	if src.RuntimeClassName != "" {
		s.RuntimeClassName = src.RuntimeClassName
	}
	for name, q := range src.Resources.Requests {
		if s.Resources.Requests == nil {
			s.Resources.Requests = corev1.ResourceList{}
		}
		s.Resources.Requests[name] = q
	}
	for name, q := range src.Resources.Limits {
		if s.Resources.Limits == nil {
			s.Resources.Limits = corev1.ResourceList{}
		}
		s.Resources.Limits[name] = q
	}
	if src.Storage.Type != "" {
		s.Storage.Type = src.Storage.Type
	}
	if src.Storage.Size != nil {
		s.Storage.Size = src.Storage.Size
	}
	if src.Storage.StorageClassName != "" {
		s.Storage.StorageClassName = src.Storage.StorageClassName
	}
	if src.Storage.Retention != nil {
		s.Storage.Retention = src.Storage.Retention
	}
	for k, v := range src.Env {
		if s.Env == nil {
			s.Env = map[string]string{}
		}
		s.Env[k] = v
	}
	if src.IdleTimeout != nil {
		s.IdleTimeout = src.IdleTimeout
	}
	if src.NodeSelector != nil {
		s.NodeSelector = src.NodeSelector
	}
	if src.Tolerations != nil {
		s.Tolerations = src.Tolerations
	}
	// A policy that sets "egress" itself replaces all network settings: a
	// policy with "egress: none" must not get a permitted destination or DNS
	// from the defaults. A policy that does not set "egress" keeps the mode
	// of the defaults: a strict default must not become "internet" because
	// a policy adds one destination.
	switch {
	case src.Network.Egress != "":
		s.Network = src.Network
	case src.Network.set():
		if src.Network.DNS != nil {
			s.Network.DNS = src.Network.DNS
		}
		if src.Network.Allow != nil {
			s.Network.Allow = src.Network.Allow
		}
	}
	if src.Command != nil {
		s.Command = src.Command
	}
	if src.RunAsUser != nil {
		s.RunAsUser = src.RunAsUser
	}
	if src.UserNamespace != nil {
		s.UserNamespace = src.UserNamespace
	}
}

// Validate examines one policy.
func (p Policy) Validate() error {
	if !idPattern.MatchString(p.ID) {
		return errors.New("id must be a DNS label: lower-case letters, digits, and '-'")
	}
	if p.Name == "" {
		return errors.New("name is necessary")
	}
	if len(p.Name) > 128 {
		return errors.New("name has more than 128 bytes")
	}
	if p.Name != strings.TrimSpace(p.Name) {
		return errors.New("name starts or ends with white space")
	}
	for _, r := range p.Name {
		if r < 0x20 || r == 0x7f {
			return errors.New("name contains a control character")
		}
	}
	if !scope.ValidKind(p.Scope) {
		return fmt.Errorf("scope must be user, chat, model, or shared, not %q", p.Scope)
	}
	seen := map[string]bool{}
	for _, u := range p.Users {
		if u == "" {
			return errors.New("users: an empty user ID")
		}
		if err := scope.ValidateID(u); err != nil {
			return fmt.Errorf("users: %q: %v", u, err)
		}
		if seen[u] {
			return fmt.Errorf("users: %q occurs two times", u)
		}
		seen[u] = true
	}
	if err := p.Spec.validate(); err != nil {
		return err
	}
	if p.Persistent() && unboundedScope(p.Scope) && (p.Storage.Retention == nil || *p.Storage.Retention <= 0) {
		return fmt.Errorf("a policy with the scope %s and persistent storage must set storage.retention: "+
			"without it, the volumes of all chats or models stay for all time", p.Scope)
	}
	return nil
}

// imagePattern accepts the usual forms of an image reference. Kubernetes
// examines the reference fully when it pulls the image.
var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$`)

// envNamePattern is the form of a variable name that a shell can use.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	maxEnvVariables = 64
	maxEnvValue     = 16 << 10
	// maxExecuteTimeout is the maximum value of OPEN_TERMINAL_EXECUTE_TIMEOUT
	// in seconds. Open WebUI stops a request after 300 seconds
	// (docs/protocol.md, section 9.4).
	maxExecuteTimeout = 300
)

// reservedEnv has the variables that a policy cannot set, with the cause.
var reservedEnv = map[string]string{
	"OPEN_TERMINAL_API_KEY":         "warren makes the key of each sandbox",
	"OPEN_TERMINAL_API_KEY_FILE":    "warren makes the key of each sandbox",
	"OPEN_TERMINAL_ISOLATED":        "warren sets it",
	"OPEN_TERMINAL_MULTI_USER":      "one sandbox for each scope key gives the isolation",
	"OPEN_TERMINAL_PACKAGES":        "it needs root privilege in the sandbox",
	"OPEN_TERMINAL_PIP_PACKAGES":    "it needs a writable package directory and network access at each start",
	"OPEN_TERMINAL_NPM_PACKAGES":    "it needs a writable package directory and network access at each start",
	"OPEN_TERMINAL_ALLOWED_DOMAINS": "it needs root privilege in the sandbox",
	"OPEN_TERMINAL_USER_PREFIX":     "it is for the multi-user mode",
	// The proxy closes an idle connection before the server does. A
	// different time on the server breaks requests.
	"OPEN_TERMINAL_UVICORN_TIMEOUT_KEEP_ALIVE": "warren depends on the default value",
	"HOME":            "warren mounts the home directory at a fixed path",
	"XDG_CONFIG_HOME": "the server reads its configuration from this directory",
	"XDG_STATE_HOME":  "the server writes its logs to this directory",
	"XDG_CACHE_HOME":  "it must be in the home directory",
	"XDG_DATA_HOME":   "it must be in the home directory",
	// These variables select the programs and libraries that the server
	// loads. Put such a change into the image.
	"PATH":            "it selects the programs that the server starts. Change the image.",
	"LD_PRELOAD":      "it selects the libraries that the server loads. Change the image.",
	"LD_LIBRARY_PATH": "it selects the libraries that the server loads. Change the image.",
	"LD_AUDIT":        "it selects the libraries that the server loads. Change the image.",
	"PYTHONPATH":      "it selects the modules that the server loads. Change the image.",
	"PYTHONHOME":      "it selects the modules that the server loads. Change the image.",
	"PYTHONSTARTUP":   "it selects code that Python runs. Change the image.",
	"PYTHONUSERBASE":  "it selects the modules that the server loads. Change the image.",
	"BASH_ENV":        "it selects a script that each shell runs. Change the image.",
	"ENV":             "it selects a script that each shell runs. Change the image.",
}

// Minimum values. A smaller value is an error in the file, for example a
// memory size of 512m, which is less than one byte.
var (
	minCPU     = resource.MustParse("1m")
	minMemory  = resource.MustParse("16Mi")
	minStorage = resource.MustParse("1Mi")
	// maxSize is the maximum of each size. The parser of Kubernetes changes
	// a larger value to 2^63-1 with no error.
	maxSize = resource.MustParse("1Ei")
)

// MinRetention is the minimum retention time of a volume. The reaper needs
// two runs to delete a volume, and a sandbox that starts again must have
// the time to take its volume back.
const MinRetention = time.Minute

func (s Spec) validate() error {
	if s.Image != "" && (len(s.Image) > 512 || !imagePattern.MatchString(s.Image)) {
		return fmt.Errorf("image: %q is not an image reference", s.Image)
	}
	switch s.ImagePullPolicy {
	case "", corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
	default:
		return fmt.Errorf("imagePullPolicy must be Always, IfNotPresent, or Never, not %q", s.ImagePullPolicy)
	}
	for _, name := range s.ImagePullSecrets {
		if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
			return fmt.Errorf("imagePullSecrets: %q is not the name of a Secret", name)
		}
	}
	if s.RuntimeClassName != "" {
		if msgs := validation.IsDNS1123Subdomain(s.RuntimeClassName); len(msgs) > 0 {
			return fmt.Errorf("runtimeClassName: %q is not the name of a RuntimeClass", s.RuntimeClassName)
		}
	}
	if err := validateResources(s.Resources); err != nil {
		return err
	}
	if err := s.Storage.validate(); err != nil {
		return err
	}
	if err := validateEnv(s.Env); err != nil {
		return err
	}
	if s.IdleTimeout != nil && *s.IdleTimeout < 0 {
		return errors.New("idleTimeout is negative")
	}
	for k, v := range s.NodeSelector {
		if msgs := validation.IsQualifiedName(k); len(msgs) > 0 {
			return fmt.Errorf("nodeSelector: %q is not a label key", k)
		}
		if msgs := validation.IsValidLabelValue(v); len(msgs) > 0 {
			return fmt.Errorf("nodeSelector: %q is not a label value", v)
		}
	}
	for i, t := range s.Tolerations {
		if err := validateToleration(t); err != nil {
			return fmt.Errorf("tolerations: item %d: %w", i+1, err)
		}
	}
	if err := s.Network.validate(); err != nil {
		return err
	}
	if s.Command != nil {
		if len(s.Command) == 0 || len(s.Command) > maxCommandItems {
			return fmt.Errorf("command must have from 1 to %d items", maxCommandItems)
		}
		// A name with no path would be found through PATH, and the home
		// directory of the sandbox can be in PATH.
		if !strings.HasPrefix(s.Command[0], "/") {
			return fmt.Errorf("command: the program %q must be an absolute path", s.Command[0])
		}
		for _, item := range s.Command {
			if item == "" || len(item) > maxEnvValue || strings.ContainsRune(item, 0) {
				return errors.New("command: an item is empty, too long, or contains a NUL character")
			}
		}
	}
	if s.RunAsUser != nil && (*s.RunAsUser < 1 || *s.RunAsUser > math.MaxInt32) {
		return errors.New("runAsUser must be a user ID from 1 to 2147483647: the user 0 is root")
	}
	return nil
}

const (
	maxCommandItems = 32
	maxDestinations = 32
	maxPorts        = 16
)

func (n Network) validate() error {
	switch n.Egress {
	case "", EgressInternet, EgressNone:
	default:
		return fmt.Errorf("network.egress must be internet or none, not %q", n.Egress)
	}
	if len(n.Allow) > maxDestinations {
		return fmt.Errorf("network.allow has more than %d items", maxDestinations)
	}
	for i, d := range n.Allow {
		switch {
		case d.CIDR != "" && (d.Namespace != "" || d.PodLabels != nil):
			return fmt.Errorf("network.allow: item %d has a cidr and pods. Use two items", i+1)
		case d.CIDR != "":
			prefix, err := netip.ParsePrefix(d.CIDR)
			// Kubernetes does not accept an IPv4 address in IPv6 form.
			if err != nil || prefix.Addr().Is4In6() {
				return fmt.Errorf("network.allow: item %d: %q is not a network, for example 10.20.0.0/16", i+1, d.CIDR)
			}
			if prefix.Masked() != prefix {
				return fmt.Errorf("network.allow: item %d: %q has bits after the prefix (the network is %s)", i+1, d.CIDR, prefix.Masked())
			}
			// All addresses include the Kubernetes API, the nodes, and warren.
			if prefix.Bits() == 0 {
				return fmt.Errorf("network.allow: item %d: %q is all addresses; give the networks that the sandbox needs", i+1, d.CIDR)
			}
		default:
			// An empty selector selects all namespaces or all pods.
			if msgs := validation.IsDNS1123Label(d.Namespace); len(msgs) > 0 {
				return fmt.Errorf("network.allow: item %d must have a cidr, or a namespace and podLabels", i+1)
			}
			if len(d.PodLabels) == 0 {
				return fmt.Errorf("network.allow: item %d: podLabels is necessary with a namespace. Without it, the item selects all pods of the namespace", i+1)
			}
			for k, v := range d.PodLabels {
				if len(validation.IsQualifiedName(k)) > 0 || v == "" || len(validation.IsValidLabelValue(v)) > 0 {
					return fmt.Errorf("network.allow: item %d: %q=%q is not a pod label", i+1, k, v)
				}
			}
		}
		if len(d.Ports) > maxPorts {
			return fmt.Errorf("network.allow: item %d has more than %d ports", i+1, maxPorts)
		}
		seen := map[int32]bool{}
		for _, port := range d.Ports {
			if port < 1 || port > 65535 {
				return fmt.Errorf("network.allow: item %d: %d is not a port number", i+1, port)
			}
			if seen[port] {
				return fmt.Errorf("network.allow: item %d: the port %d occurs two times", i+1, port)
			}
			seen[port] = true
		}
	}
	return nil
}

// resourceNames are the resources that a policy can set.
var resourceNames = []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage}

func validateResources(r corev1.ResourceRequirements) error {
	for kind, list := range map[string]corev1.ResourceList{"requests": r.Requests, "limits": r.Limits} {
		for name, q := range list {
			if !slices.Contains(resourceNames, name) {
				return fmt.Errorf("resources.%s: %q is not cpu, memory, or ephemeral-storage", kind, name)
			}
			if q.Sign() <= 0 {
				return fmt.Errorf("resources.%s.%s must be more than zero", kind, name)
			}
			minimum := minCPU
			if name == corev1.ResourceMemory {
				minimum = minMemory
			} else if name == corev1.ResourceEphemeralStorage {
				minimum = minStorage
			}
			if q.Cmp(minimum) < 0 {
				return fmt.Errorf("resources.%s.%s is %s, and the minimum is %s", kind, name, q.String(), minimum.String())
			}
			if name != corev1.ResourceCPU && q.Cmp(maxSize) > 0 {
				return fmt.Errorf("resources.%s.%s is %s, and the maximum is %s", kind, name, q.String(), maxSize.String())
			}
			if name != corev1.ResourceCPU && !wholeBytes(q) {
				return fmt.Errorf("resources.%s.%s is %s, which is not a whole number of bytes", kind, name, q.String())
			}
		}
	}
	for _, name := range resourceNames {
		request, hasRequest := r.Requests[name]
		limit, hasLimit := r.Limits[name]
		if hasRequest && hasLimit && request.Cmp(limit) > 0 {
			return fmt.Errorf("resources: the request for %s (%s) is more than the limit (%s); "+
				"one of the two values can come from the defaults of the file or of warren", name, request.String(), limit.String())
		}
	}
	return nil
}

// wholeBytes reports whether a size is a whole number of bytes. A size such
// as 1500m is a mistake for 1500M or 1500Mi.
//
// Value rounds up to a whole number, thus an equal value is a whole number.
// MilliValue is not usable here: it overflows an int64 for sizes from
// approximately 8Pi. The caller must refuse a size above maxSize first,
// because Value overflows above 8Ei.
func wholeBytes(q resource.Quantity) bool {
	return q.Cmp(*resource.NewQuantity(q.Value(), q.Format)) == 0
}

func (s Storage) validate() error {
	switch s.Type {
	case "", StoragePersistent, StorageEphemeral:
	default:
		return fmt.Errorf("storage.type must be persistent or ephemeral, not %q", s.Type)
	}
	if s.Size != nil && s.Size.Cmp(maxSize) > 0 {
		return fmt.Errorf("storage.size is %s, and the maximum is %s", s.Size.String(), maxSize.String())
	}
	if s.Size != nil && (s.Size.Cmp(minStorage) < 0 || !wholeBytes(*s.Size)) {
		return fmt.Errorf("storage.size is %s. It must be a whole number of bytes, and the minimum is %s", s.Size.String(), minStorage.String())
	}
	if s.StorageClassName != "" {
		if msgs := validation.IsDNS1123Subdomain(s.StorageClassName); len(msgs) > 0 {
			return fmt.Errorf("storage.storageClassName: %q is not the name of a StorageClass", s.StorageClassName)
		}
	}
	if s.Retention != nil && *s.Retention != 0 && *s.Retention < MinRetention {
		return fmt.Errorf("storage.retention must be 0 (keep the volume) or a minimum of %s", MinRetention)
	}
	return nil
}

func validateEnv(env map[string]string) error {
	if len(env) > maxEnvVariables {
		return fmt.Errorf("env has more than %d variables", maxEnvVariables)
	}
	for name, value := range env {
		if !envNamePattern.MatchString(name) || len(name) > 128 {
			return fmt.Errorf("env: %q is not a variable name", name)
		}
		if cause, reserved := reservedEnv[name]; reserved {
			return fmt.Errorf("env: a policy cannot set %s: %s", name, cause)
		}
		if len(value) > maxEnvValue {
			return fmt.Errorf("env: the value of %s has more than %d bytes", name, maxEnvValue)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("env: the value of %s contains a NUL character", name)
		}
		if name == "OPEN_TERMINAL_EXECUTE_TIMEOUT" {
			n, ok := parseSeconds(value)
			if !ok || n > maxExecuteTimeout {
				return fmt.Errorf("env: OPEN_TERMINAL_EXECUTE_TIMEOUT must be a number of seconds from 1 to %d", maxExecuteTimeout)
			}
		}
	}
	return nil
}

// parseSeconds reads a positive decimal number with no sign and no unit.
func parseSeconds(s string) (int, bool) {
	if s == "" || len(s) > 6 {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, n > 0
}

func validateToleration(t corev1.Toleration) error {
	switch t.Operator {
	case "", corev1.TolerationOpEqual:
		if t.Key == "" {
			return errors.New("key is necessary with the operator Equal")
		}
	case corev1.TolerationOpExists:
		if t.Value != "" {
			return errors.New("value must be empty with the operator Exists")
		}
	default:
		return fmt.Errorf("operator must be Equal or Exists, not %q", t.Operator)
	}
	if t.Key != "" {
		if msgs := validation.IsQualifiedName(t.Key); len(msgs) > 0 {
			return fmt.Errorf("%q is not a taint key", t.Key)
		}
	}
	if msgs := validation.IsValidLabelValue(t.Value); len(msgs) > 0 {
		return fmt.Errorf("%q is not a taint value", t.Value)
	}
	switch t.Effect {
	case "", corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
	default:
		return fmt.Errorf("effect must be NoSchedule, PreferNoSchedule, or NoExecute, not %q", t.Effect)
	}
	if t.TolerationSeconds != nil && t.Effect != corev1.TaintEffectNoExecute {
		return errors.New("tolerationSeconds is permitted only with the effect NoExecute")
	}
	return nil
}

// Store has the current set of policies. Warren can replace the set while
// it operates.
type Store struct {
	set     atomic.Pointer[Set]
	changed atomic.Pointer[time.Time]
}

// NewStore makes a store with a first set.
func NewStore(set *Set) *Store {
	s := &Store{}
	s.set.Store(set)
	return s
}

// Replace puts a new set into the store.
func (s *Store) Replace(set *Set) {
	now := time.Now()
	// Write the time first. A reader that sees the new set then also sees
	// the new time.
	s.changed.Store(&now)
	s.set.Store(set)
}

// ChangedAt returns the time of the last Replace. It is the zero time if
// the store has its first set.
func (s *Store) ChangedAt() time.Time {
	if t := s.changed.Load(); t != nil {
		return *t
	}
	return time.Time{}
}

// Get returns the policy with the given ID from the current set.
func (s *Store) Get(id string) (Policy, bool) { return s.set.Load().Get(id) }

// List returns all policies of the current set, sorted by ID.
func (s *Store) List() []Policy { return s.set.Load().List() }

// Get returns the policy with the given ID. The result is a copy: a caller
// can change it, and many callers can read it at the same time.
func (s *Set) Get(id string) (Policy, bool) {
	p, ok := s.byID[id]
	if !ok {
		return Policy{}, false
	}
	return p.copy(), true
}

// copy returns a policy that shares no data with p.
func (p Policy) copy() Policy {
	p.Users = slices.Clone(p.Users)
	p.Spec = p.Spec.clone()
	return p
}

// List returns all policies, sorted by ID.
func (s *Set) List() []Policy {
	out := make([]Policy, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.byID[id].copy())
	}
	return out
}
