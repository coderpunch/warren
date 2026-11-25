// Package policy reads the named sandbox policies.
//
// An administrator defines policies in a YAML file. Warren shows each policy
// to Open WebUI as one read-only template (docs/protocol.md, section 4.6).
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"

	"github.com/h3xcat/warren/internal/scope"
)

// Policy is one named set of sandbox settings.
type Policy struct {
	// ID is the template ID that Open WebUI sends in X-Terminal-Template-Id.
	ID string `json:"id"`
	// Name is the name that the user sees.
	Name string `json:"name"`
	// Scope is user, chat, model, or shared.
	Scope scope.Kind `json:"scope"`
}

// File is the content of the policy file.
type File struct {
	Policies []Policy `json:"policies"`
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

// Parse examines the content of a policy file. It rejects unknown fields,
// more than one YAML document, and values that are not strings.
func Parse(data []byte) (*Set, error) {
	if err := checkShape(data); err != nil {
		return nil, err
	}
	// Convert with the same YAML parser that examined the structure, then
	// read the result as strict JSON. Thus the two steps always agree on
	// the type of a value.
	var generic any
	if err := yamlv3.Unmarshal(data, &generic); err != nil {
		return nil, err
	}
	asJSON, err := json.Marshal(generic)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(asJSON))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	return NewSet(f.Policies)
}

// policyKeys are the permitted keys of one policy.
var policyKeys = map[string]bool{"id": true, "name": true, "scope": true}

// checkShape examines the YAML structure before the conversion to Go
// values. The conversion alone is not strict: it ignores a second document,
// it accepts a key in a different letter case, and it can change a value
// that is not a string, for example the number 007.
func checkShape(data []byte) error {
	dec := yamlv3.NewDecoder(bytes.NewReader(data))
	var doc yamlv3.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("the file is empty")
		}
		return err
	}
	var second yamlv3.Node
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		return errors.New("the file has more than one YAML document")
	}
	if doc.Kind != yamlv3.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yamlv3.MappingNode {
		return errors.New("the file must be a mapping with the key \"policies\"")
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Value != "policies" {
			return fmt.Errorf("line %d: unknown key %q", key.Line, key.Value)
		}
		if value.Kind != yamlv3.SequenceNode {
			return fmt.Errorf("line %d: \"policies\" must be a list", value.Line)
		}
		for _, item := range value.Content {
			if item.Kind != yamlv3.MappingNode {
				return fmt.Errorf("line %d: a policy must be a mapping", item.Line)
			}
			for j := 0; j+1 < len(item.Content); j += 2 {
				k, v := item.Content[j], item.Content[j+1]
				if !policyKeys[k.Value] {
					return fmt.Errorf("line %d: unknown key %q", k.Line, k.Value)
				}
				if v.Kind != yamlv3.ScalarNode || v.Tag != "!!str" {
					return fmt.Errorf("line %d: %q must be a string. Put the value in quotation marks.", v.Line, k.Value)
				}
			}
		}
	}
	return nil
}

// NewSet makes a set from a list of policies.
func NewSet(policies []Policy) (*Set, error) {
	if len(policies) == 0 {
		return nil, errors.New("no policies")
	}
	s := &Set{byID: make(map[string]Policy, len(policies))}
	for i, p := range policies {
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("policy %d (%q): %w", i+1, p.ID, err)
		}
		if _, dup := s.byID[p.ID]; dup {
			return nil, fmt.Errorf("policy %d: duplicate ID %q", i+1, p.ID)
		}
		s.byID[p.ID] = p
		s.order = append(s.order, p.ID)
	}
	sort.Strings(s.order)
	return s, nil
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
	return nil
}

// Get returns the policy with the given ID.
func (s *Set) Get(id string) (Policy, bool) {
	p, ok := s.byID[id]
	return p, ok
}

// List returns all policies, sorted by ID.
func (s *Set) List() []Policy {
	out := make([]Policy, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.byID[id])
	}
	return out
}
