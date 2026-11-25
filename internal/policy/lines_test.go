package policy

import (
	"strings"
	"testing"
)

// docs/policies.md, section 3: an error message gives the line number.
func TestErrorsOfAPolicyHaveLineNumbers(t *testing.T) {
	cases := map[string]struct{ file, line string }{
		"no scope":         {"policies:\n  - id: a\n    name: A\n", "line 2"},
		"wrong scope":      {"policies:\n  - id: a\n    name: A\n    scope: team\n", "line 2"},
		"no id":            {"policies:\n  - name: A\n    scope: user\n", "line 2"},
		"bad id":           {"policies:\n  - id: A_1\n    name: A\n    scope: user\n", "line 2"},
		"no name":          {"policies:\n  - id: a\n    scope: user\n", "line 2"},
		"duplicate id":     {"policies:\n  - {id: a, name: A, scope: user}\n  - {id: a, name: B, scope: user}\n", "line 3"},
		"bad user":         {"policies:\n  - {id: a, name: A, scope: user}\n  - {id: b, name: B, scope: user, users: [\" u\"]}\n", "line 3"},
		"duplicate user":   {"policies:\n  - {id: a, name: A, scope: user, users: [u1, u1]}\n", "line 2"},
		"chat with no end": {"policies:\n  - {id: a, name: A, scope: chat, storage: {type: persistent}}\n", "line 2"},
		"request above a default limit": {
			"defaults:\n  resources:\n    limits: {cpu: 1}\npolicies:\n  - id: a\n    name: A\n    scope: user\n    resources:\n      requests: {cpu: 2}\n", "line 5"},
		"error in the defaults": {"defaults:\n  image: \"bad image\"\npolicies:\n  - {id: a, name: A, scope: user}\n", "line 2"},
	}
	for name, c := range cases {
		_, err := Parse([]byte(c.file))
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if !strings.Contains(err.Error(), c.line) {
			t.Errorf("%s: error = %q, want %q", name, err, c.line)
		}
	}
}
