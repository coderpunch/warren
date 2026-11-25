package policy

import (
	"strings"
	"testing"

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
