package auth

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	keyA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	keyB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestNewKeysRejectsBadSets(t *testing.T) {
	cases := map[string][]string{
		"empty set":   nil,
		"short key":   {"short"},
		"31 chars":    {strings.Repeat("x", MinKeyLength-1)},
		"white space": {keyA[:20] + " " + keyA[:20]},
		"not ASCII":   {keyA + "\u00e9"},
		"BOM":         {"\ufeff" + keyA},
		"one bad key": {keyA, "short"},
	}
	for name, keys := range cases {
		if _, err := NewKeys(keys); err == nil {
			t.Errorf("%s: NewKeys gave no error", name)
		}
	}
}

func TestMatch(t *testing.T) {
	k, err := NewKeys([]string{keyA, keyB})
	if err != nil {
		t.Fatal(err)
	}
	for _, good := range []string{keyA, keyB} {
		if !k.Match(good) {
			t.Errorf("Match(%q) = false", good)
		}
	}
	for _, bad := range []string{"", keyA[:39], keyA + "a", strings.ToUpper(keyA), keyA + "\n", " " + keyA} {
		if k.Match(bad) {
			t.Errorf("Match(%q) = true", bad)
		}
	}
	var none *Keys
	if none.Match(keyA) {
		t.Error("nil key set accepted a key")
	}
}

func TestReplace(t *testing.T) {
	k, _ := NewKeys([]string{keyA})
	if err := k.Replace([]string{keyA, keyB}); err != nil || k.Len() != 2 {
		t.Fatalf("Replace = %v, Len = %d", err, k.Len())
	}
	if err := k.Replace([]string{keyB}); err != nil {
		t.Fatal(err)
	}
	if k.Match(keyA) || !k.Match(keyB) {
		t.Error("the old key is accepted, or the new key is not accepted")
	}
	// A bad list must not remove the keys that operate.
	for _, bad := range [][]string{nil, {"short"}} {
		if err := k.Replace(bad); err == nil {
			t.Errorf("Replace(%q) gave no error", bad)
		}
	}
	if !k.Match(keyB) {
		t.Error("a refused replacement removed the current key")
	}
}

func TestLoadKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	// A byte order mark at the start and Windows line ends are permitted.
	content := "\ufeff# current key\r\n" + keyA + "\r\n\n  " + keyB + "  \n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !k.Match(keyA) || !k.Match(keyB) {
		t.Error("a key from the file does not match")
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n# no key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(empty); err == nil {
		t.Error("an empty key file gave no error")
	}
	if _, err := LoadKeyFile(filepath.Join(dir, "absent")); err == nil {
		t.Error("an absent key file gave no error")
	}
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		values []string
		token  string
		ok     bool
	}{
		{[]string{"Bearer " + keyA}, keyA, true},
		{[]string{"bearer " + keyA}, keyA, true},
		{[]string{"Bearer   " + keyA + " "}, keyA, true},
		{nil, "", false},
		{[]string{""}, "", false},
		{[]string{"Bearer"}, "", false},
		{[]string{"Bearer "}, "", false},
		{[]string{"Basic " + keyA}, "", false},
		{[]string{keyA}, "", false},
		// Two headers are ambiguous. Refuse them.
		{[]string{"Bearer " + keyA, "Bearer " + keyB}, "", false},
	}
	for _, c := range cases {
		h := http.Header{}
		for _, v := range c.values {
			h.Add("Authorization", v)
		}
		token, ok := BearerToken(h)
		if token != c.token || ok != c.ok {
			t.Errorf("BearerToken(%q) = %q, %v; want %q, %v", c.values, token, ok, c.token, c.ok)
		}
	}
}

func TestRequestAuthorized(t *testing.T) {
	k, _ := NewKeys([]string{keyA})
	r, _ := http.NewRequest("GET", "/", nil)
	if k.RequestAuthorized(r) {
		t.Error("a request with no header is authorized")
	}
	r.Header.Set("Authorization", "Bearer "+keyB)
	if k.RequestAuthorized(r) {
		t.Error("a request with a wrong key is authorized")
	}
	r.Header.Set("Authorization", "Bearer "+keyA)
	if !k.RequestAuthorized(r) {
		t.Error("a request with the correct key is not authorized")
	}
}
