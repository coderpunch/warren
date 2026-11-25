package policy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"

	yaml "go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/h3xcat/warren/internal/scope"
)

// The decoder reads the YAML nodes directly. A conversion to Go values with
// the YAML library is not strict: it ignores a second document, it accepts a
// key in a different letter case, it lets the last of two equal keys win,
// and it can change a value that is not a string, for example the number 007.

// lineError is an error with the line of the YAML node.
func lineError(n *yaml.Node, format string, args ...any) error {
	return fmt.Errorf("line %d: %s", n.Line, fmt.Sprintf(format, args...))
}

// plain refuses the YAML functions that make a file hard to examine.
func plain(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return lineError(n, "anchors and aliases are not permitted")
	}
	return nil
}

// field reads the value of one key.
type field func(v *yaml.Node) error

// mapping reads a mapping with a fixed set of keys.
func mapping(n *yaml.Node, what string, fields map[string]field) error {
	if err := plain(n); err != nil {
		return err
	}
	if n.Kind != yaml.MappingNode {
		return lineError(n, "%s must be a mapping", what)
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if err := plain(k); err != nil {
			return err
		}
		if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
			return lineError(k, "a key of %s must be a string", what)
		}
		read, known := fields[k.Value]
		if !known {
			return lineError(k, "unknown key %q in %s", k.Value, what)
		}
		if seen[k.Value] {
			return lineError(k, "the key %q occurs two times", k.Value)
		}
		seen[k.Value] = true
		if err := plain(v); err != nil {
			return err
		}
		if err := read(v); err != nil {
			return err
		}
	}
	return nil
}

// text reads a string. A value that YAML reads as a number, a boolean, or
// null is an error, because its text form is not certain.
func text(v *yaml.Node, key string) (string, error) {
	if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
		return "", lineError(v, "%q must be a string (put the value in quotation marks)", key)
	}
	return v.Value, nil
}

func textInto(dst *string, key string) field {
	return func(v *yaml.Node) error {
		s, err := text(v, key)
		if err != nil {
			return err
		}
		if s == "" {
			return lineError(v, "%q is empty (remove the key to use the default)", key)
		}
		*dst = s
		return nil
	}
}

// plainNumber is a decimal number with no sign, no exponent, and no
// leading zero. YAML and Kubernetes read such a number the same.
var plainNumber = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// quantity reads a Kubernetes quantity. A CPU quantity is, for example,
// 500m, 0.5, or 2. A size is, for example, 512Mi or 2Gi.
func quantity(v *yaml.Node, key string, size bool) (resource.Quantity, error) {
	var zero resource.Quantity
	example := "for example 500m or 2"
	if size {
		example = "for example 512Mi or 2Gi"
	}
	if v.Kind != yaml.ScalarNode {
		return zero, lineError(v, "%q must be a quantity, %s", key, example)
	}
	switch v.Tag {
	case "!!str":
	case "!!int", "!!float":
		// Use the text of the file. YAML reads 010 and 0x10 as different
		// numbers than Kubernetes does.
		if !plainNumber.MatchString(v.Value) {
			return zero, lineError(v, "%q: put the value %s in quotation marks", key, v.Value)
		}
	default:
		return zero, lineError(v, "%q must be a quantity, %s", key, example)
	}
	q, err := resource.ParseQuantity(v.Value)
	if err != nil {
		return zero, lineError(v, "%q: %q is not a quantity, %s", key, v.Value, example)
	}
	// Open WebUI shows the sizes of a template, and it cannot read the
	// exponent form (docs/protocol.md, section 4.6).
	if size && q.Format == resource.DecimalExponent {
		return zero, lineError(v, "%q: write %q with a unit, %s", key, v.Value, example)
	}
	return q, nil
}

// days is a whole number of days, for example 30d.
var days = regexp.MustCompile(`^([1-9][0-9]{0,4})d$`)

// duration reads a time such as 30m, 12h, 30d, or 0. The unit "d" is one
// day of 24 hours. The time format of Go has no unit for days, and a value
// such as 720h is not easy to read.
func duration(v *yaml.Node, key string) (time.Duration, error) {
	if v.Kind == yaml.ScalarNode && v.Tag == "!!int" && v.Value == "0" {
		return 0, nil
	}
	s, err := text(v, key)
	if err != nil {
		return 0, lineError(v, "%q must be a time, for example 30m, 12h, or 30d", key)
	}
	if m := days.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, lineError(v, "%q: %q is not a time, for example 30m, 12h, or 30d", key, s)
	}
	return d, nil
}

// textList reads a list of strings.
func textList(v *yaml.Node, key string) ([]string, error) {
	if v.Kind != yaml.SequenceNode {
		return nil, lineError(v, "%q must be a list", key)
	}
	if len(v.Content) == 0 {
		return nil, lineError(v, "%q is an empty list (remove the key to use the default)", key)
	}
	out := make([]string, 0, len(v.Content))
	for _, item := range v.Content {
		if err := plain(item); err != nil {
			return nil, err
		}
		s, err := text(item, "an item of "+key)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// textMap reads a mapping from strings to strings.
func textMap(v *yaml.Node, key string) (map[string]string, error) {
	if v.Kind != yaml.MappingNode {
		return nil, lineError(v, "%q must be a mapping", key)
	}
	out := map[string]string{}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, val := v.Content[i], v.Content[i+1]
		if err := plain(k); err != nil {
			return nil, err
		}
		if err := plain(val); err != nil {
			return nil, err
		}
		name, err := text(k, "a key of "+key)
		if err != nil {
			return nil, err
		}
		if _, dup := out[name]; dup {
			return nil, lineError(k, "the key %q occurs two times", name)
		}
		value, err := text(val, key+"."+name)
		if err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, nil
}

// decodeFile reads the file into the defaults and the policies. It does not
// merge them.
func decodeFile(data []byte) (Spec, []Policy, error) {
	var defaults Spec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return defaults, nil, errors.New("the file is empty")
		}
		return defaults, nil, err
	}
	var second yaml.Node
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		return defaults, nil, errors.New("the file has more than one YAML document")
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return defaults, nil, errors.New("the file must be a mapping with the key \"policies\"")
	}
	var policies []Policy
	err := mapping(doc.Content[0], "the file", map[string]field{
		"defaults": func(v *yaml.Node) error {
			if err := mapping(v, "\"defaults\"", specFields(&defaults)); err != nil {
				return err
			}
			// The storage type has a different default for each scope. A
			// type in the defaults would change all scopes with no message.
			if defaults.Storage.Type != "" {
				return lineError(v, "\"storage.type\" is not permitted in the defaults. Set it in each policy.")
			}
			return nil
		},
		"policies": func(v *yaml.Node) error {
			if v.Kind != yaml.SequenceNode {
				return lineError(v, "\"policies\" must be a list")
			}
			for _, item := range v.Content {
				var p Policy
				fields := specFields(&p.Spec)
				fields["id"] = textInto(&p.ID, "id")
				fields["name"] = textInto(&p.Name, "name")
				fields["scope"] = func(v *yaml.Node) error {
					s, err := text(v, "scope")
					p.Scope = scope.Kind(s)
					return err
				}
				fields["users"] = func(v *yaml.Node) error {
					list, err := textList(v, "users")
					p.Users = list
					return err
				}
				if err := mapping(item, "a policy", fields); err != nil {
					return err
				}
				// Find errors of one policy here, where the line is known.
				if err := p.Spec.validate(); err != nil {
					return lineError(item, "policy %q: %v", p.ID, err)
				}
				policies = append(policies, p)
			}
			return nil
		},
	})
	if err != nil {
		return defaults, nil, err
	}
	if err := defaults.validate(); err != nil {
		return defaults, nil, fmt.Errorf("defaults: %w", err)
	}
	return defaults, policies, nil
}

// specFields returns the readers for the keys that a policy and the
// defaults share.
func specFields(s *Spec) map[string]field {
	return map[string]field{
		"image": textInto(&s.Image, "image"),
		"imagePullPolicy": func(v *yaml.Node) error {
			p, err := text(v, "imagePullPolicy")
			s.ImagePullPolicy = corev1.PullPolicy(p)
			return err
		},
		"imagePullSecrets": func(v *yaml.Node) error {
			list, err := textList(v, "imagePullSecrets")
			s.ImagePullSecrets = list
			return err
		},
		"runtimeClassName": textInto(&s.RuntimeClassName, "runtimeClassName"),
		"resources": func(v *yaml.Node) error {
			return mapping(v, "\"resources\"", map[string]field{
				"requests": resourceList(&s.Resources.Requests, "resources.requests"),
				"limits":   resourceList(&s.Resources.Limits, "resources.limits"),
			})
		},
		"storage": func(v *yaml.Node) error {
			return mapping(v, "\"storage\"", map[string]field{
				"type": func(v *yaml.Node) error {
					t, err := text(v, "storage.type")
					s.Storage.Type = StorageType(t)
					return err
				},
				"size": func(v *yaml.Node) error {
					q, err := quantity(v, "storage.size", true)
					s.Storage.Size = &q
					return err
				},
				"storageClassName": textInto(&s.Storage.StorageClassName, "storage.storageClassName"),
				"retention": func(v *yaml.Node) error {
					d, err := duration(v, "storage.retention")
					s.Storage.Retention = &d
					return err
				},
			})
		},
		"env": func(v *yaml.Node) error {
			m, err := textMap(v, "env")
			s.Env = m
			return err
		},
		"idleTimeout": func(v *yaml.Node) error {
			d, err := duration(v, "idleTimeout")
			s.IdleTimeout = &d
			return err
		},
		"nodeSelector": func(v *yaml.Node) error {
			m, err := textMap(v, "nodeSelector")
			s.NodeSelector = m
			return err
		},
		"tolerations": func(v *yaml.Node) error {
			if v.Kind != yaml.SequenceNode {
				return lineError(v, "\"tolerations\" must be a list")
			}
			s.Tolerations = []corev1.Toleration{}
			for _, item := range v.Content {
				var t corev1.Toleration
				var key, operator, value, effect string
				err := mapping(item, "a toleration", map[string]field{
					"key":      textInto(&key, "key"),
					"operator": textInto(&operator, "operator"),
					"value":    textInto(&value, "value"),
					"effect":   textInto(&effect, "effect"),
					"tolerationSeconds": func(v *yaml.Node) error {
						if v.Kind != yaml.ScalarNode || (v.Tag != "!!int" && v.Tag != "!!str") || !plainNumber.MatchString(v.Value) {
							return lineError(v, "\"tolerationSeconds\" must be a number of seconds")
						}
						n, err := strconv.ParseInt(v.Value, 10, 64)
						if err != nil {
							return lineError(v, "\"tolerationSeconds\" must be a number of seconds")
						}
						t.TolerationSeconds = &n
						return nil
					},
				})
				if err != nil {
					return err
				}
				t.Key, t.Operator, t.Value, t.Effect = key, corev1.TolerationOperator(operator), value, corev1.TaintEffect(effect)
				s.Tolerations = append(s.Tolerations, t)
			}
			return nil
		},
	}
}

// resourceList reads the mapping of one resource list.
func resourceList(dst *corev1.ResourceList, key string) field {
	return func(v *yaml.Node) error {
		list := corev1.ResourceList{}
		read := func(name corev1.ResourceName) field {
			return func(v *yaml.Node) error {
				q, err := quantity(v, key+"."+string(name), name != corev1.ResourceCPU)
				list[name] = q
				return err
			}
		}
		fields := map[string]field{}
		for _, name := range resourceNames {
			fields[string(name)] = read(name)
		}
		if err := mapping(v, "\""+key+"\"", fields); err != nil {
			return err
		}
		*dst = list
		return nil
	}
}
