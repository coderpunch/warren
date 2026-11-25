// Package policy reads the named sandbox policies.
//
// An administrator defines policies in a YAML file. Warren shows each policy
// to Open WebUI as one read-only template (docs/protocol.md, section 4.6).
package policy

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"

	"sigs.k8s.io/yaml"

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

// Parse examines the content of a policy file. It rejects unknown fields.
func Parse(data []byte) (*Set, error) {
	var f File
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, err
	}
	return NewSet(f.Policies)
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
