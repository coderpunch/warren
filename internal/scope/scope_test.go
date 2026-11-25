package scope

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

func TestIdentityFromHeaders(t *testing.T) {
	id, err := IdentityFromHeaders(headers(
		HeaderUserID, "user-1",
		HeaderTemplateID, "default",
		HeaderChatID, "chat-9",
		HeaderModelID, "gpt:latest",
	))
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{UserID: "user-1", TemplateID: "default", ChatID: "chat-9", ModelID: "gpt:latest"}
	if id != want {
		t.Errorf("identity = %+v, want %+v", id, want)
	}

	// Absent headers give empty fields and no error.
	id, err = IdentityFromHeaders(http.Header{})
	if err != nil || id != (Identity{}) {
		t.Errorf("empty headers gave %+v, %v", id, err)
	}
}

func TestIdentityFromHeadersRejectsBadValues(t *testing.T) {
	cases := map[string]http.Header{
		"two user IDs":      headers(HeaderUserID, "a", HeaderUserID, "b"),
		"two template IDs":  headers(HeaderTemplateID, "a", HeaderTemplateID, "b"),
		"control character": headers(HeaderUserID, "a\x00b"),
		"line feed":         headers(HeaderChatID, "a\nb"),
		"delete character":  headers(HeaderModelID, "a\x7fb"),
		"too long":          headers(HeaderUserID, strings.Repeat("x", MaxIDLength+1)),
		"not UTF-8":         headers(HeaderUserID, "a\xffb"),
		"space at start":    headers(HeaderUserID, " a"),
	}
	for name, h := range cases {
		if _, err := IdentityFromHeaders(h); !errors.Is(err, ErrInvalidHeader) {
			t.Errorf("%s: error = %v, want ErrInvalidHeader", name, err)
		}
	}
}

func TestResolve(t *testing.T) {
	full := Identity{UserID: "u1", TemplateID: "t1", ChatID: "c1", ModelID: "m1"}
	cases := []struct {
		name    string
		kind    Kind
		id      Identity
		context string
		err     error
	}{
		{"user scope", KindUser, full, "user:u1", nil},
		{"chat scope", KindChat, full, "chat:c1", nil},
		{"model scope", KindModel, full, "model:m1", nil},
		{"shared scope", KindShared, full, "default", nil},
		{"no user", KindShared, Identity{TemplateID: "t1"}, "", ErrMissingUser},
		{"no template", KindUser, Identity{UserID: "u1"}, "", ErrInvalidHeader},
		{"chat scope, no chat", KindChat, Identity{UserID: "u1", TemplateID: "t1"}, "", ErrMissingChat},
		{"chat scope, temporary chat", KindChat, Identity{UserID: "u1", TemplateID: "t1", ChatID: "temporary:abc"}, "", ErrMissingChat},
		{"chat scope, old temporary chat", KindChat, Identity{UserID: "u1", TemplateID: "t1", ChatID: "local:abc"}, "", ErrMissingChat},
		{"chat scope, channel chat", KindChat, Identity{UserID: "u1", TemplateID: "t1", ChatID: "channel:abc"}, "", ErrMissingChat},
		{"model scope, no model", KindModel, Identity{UserID: "u1", TemplateID: "t1"}, "", ErrMissingModel},
		{"unknown kind", Kind("team"), full, "", ErrUnknownKind},
	}
	for _, c := range cases {
		ref, err := Resolve(c.kind, c.id)
		if !errors.Is(err, c.err) {
			t.Errorf("%s: error = %v, want %v", c.name, err, c.err)
			continue
		}
		if err == nil && ref.ContextID() != c.context {
			t.Errorf("%s: context = %q, want %q", c.name, ref.ContextID(), c.context)
		}
	}
}

// The user scope must depend only on the user ID. The chat ID and the model
// ID come from the browser in some cases, thus they must not change the
// sandbox of a user-scope template.
func TestUserScopeIgnoresChatAndModel(t *testing.T) {
	a, _ := Resolve(KindUser, Identity{UserID: "u1", TemplateID: "t", ChatID: "c1", ModelID: "m1"})
	b, _ := Resolve(KindUser, Identity{UserID: "u1", TemplateID: "t", ChatID: "c2", ModelID: "m2"})
	if a.Name() != b.Name() {
		t.Error("the chat ID or model ID changed a user-scope sandbox")
	}
}

func TestNamesSeparateSandboxes(t *testing.T) {
	refs := []Ref{
		{TemplateID: "t", Kind: KindUser, Value: "alice"},
		{TemplateID: "t", Kind: KindUser, Value: "bob"},
		{TemplateID: "t", Kind: KindUser, Value: "Alice"},
		{TemplateID: "t2", Kind: KindUser, Value: "alice"},
		{TemplateID: "t", Kind: KindChat, Value: "alice"},
		{TemplateID: "t", Kind: KindModel, Value: "alice"},
		{TemplateID: "t", Kind: KindShared},
		// A value that looks like a different context ID.
		{TemplateID: "t", Kind: KindUser, Value: "x|user:alice"},
		{TemplateID: "t|user", Kind: KindUser, Value: "alice"},
		{TemplateID: "ab", Kind: KindUser, Value: "c"},
		{TemplateID: "a", Kind: KindUser, Value: "bc"},
	}
	label := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	seen := map[string]Ref{}
	for _, r := range refs {
		name := r.Name()
		if !label.MatchString(name) {
			t.Errorf("name %q is not a DNS label", name)
		}
		if strings.Contains(name, "alice") {
			t.Errorf("name %q shows the user ID", name)
		}
		if other, dup := seen[name]; dup {
			t.Errorf("%+v and %+v have the same name %q", r, other, name)
		}
		seen[name] = r
		if name != r.Name() {
			t.Errorf("name of %+v is not stable", r)
		}
	}
}

func TestParseContextID(t *testing.T) {
	for _, r := range []Ref{
		{Kind: KindUser, Value: "u1"},
		{Kind: KindChat, Value: "c:1"},
		{Kind: KindModel, Value: "agent:a:b:c"},
		{Kind: KindShared},
	} {
		kind, value, err := ParseContextID(r.ContextID())
		if err != nil || kind != r.Kind || value != r.Value {
			t.Errorf("ParseContextID(%q) = %q, %q, %v", r.ContextID(), kind, value, err)
		}
	}
	for _, bad := range []string{"", "user", "user:", "team:x", "shared", ":x"} {
		if _, _, err := ParseContextID(bad); err == nil {
			t.Errorf("ParseContextID(%q) gave no error", bad)
		}
	}
}
