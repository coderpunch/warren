// Package auth authenticates the Open WebUI backend with the connection key.
//
// Warren accepts only an orchestrator connection. On that connection Open
// WebUI sends one static key on each request. The key proves that the caller
// is the Open WebUI backend. It does not prove which user made the request.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

// MinKeyLength is the minimum number of characters in a connection key.
const MinKeyLength = 32

// Keys is a set of accepted connection keys. More than one key lets an
// administrator replace a key with no downtime: add the new key, change the
// key in Open WebUI, then remove the old key. Replace changes the set while
// warren operates.
type Keys struct {
	digests atomic.Pointer[[][sha256.Size]byte]
}

func digestsOf(keys []string) ([][sha256.Size]byte, error) {
	if len(keys) == 0 {
		return nil, errors.New("no connection key")
	}
	out := make([][sha256.Size]byte, 0, len(keys))
	for i, key := range keys {
		if len(key) < MinKeyLength {
			return nil, fmt.Errorf("connection key %d has fewer than %d characters", i+1, MinKeyLength)
		}
		for _, r := range key {
			// Only visible ASCII. A different character is usually a
			// mistake in the file, for example a byte order mark.
			if r <= 0x20 || r >= 0x7f {
				return nil, fmt.Errorf("connection key %d contains a character that is not visible ASCII", i+1)
			}
		}
		out = append(out, sha256.Sum256([]byte(key)))
	}
	return out, nil
}

// NewKeys makes a key set. It rejects an empty set and a short key.
func NewKeys(keys []string) (*Keys, error) {
	d, err := digestsOf(keys)
	if err != nil {
		return nil, err
	}
	k := &Keys{}
	k.digests.Store(&d)
	return k, nil
}

// Replace puts a new list of keys in place of the current list. If the new
// list is not valid, the current list stays.
func (k *Keys) Replace(keys []string) error {
	d, err := digestsOf(keys)
	if err != nil {
		return err
	}
	k.digests.Store(&d)
	return nil
}

// Len returns the number of keys in the set.
func (k *Keys) Len() int {
	return len(*k.digests.Load())
}

// ReadKeyFile reads a list of keys from a file. Each line that is not empty
// and does not start with '#' is one key.
func ReadKeyFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	// Some editors put a byte order mark at the start of a file.
	text := strings.TrimPrefix(string(data), "\uFEFF")
	var keys []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	return keys, nil
}

// LoadKeyFile reads a key set from a file.
func LoadKeyFile(path string) (*Keys, error) {
	keys, err := ReadKeyFile(path)
	if err != nil {
		return nil, err
	}
	k, err := NewKeys(keys)
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return k, nil
}

// Match reports whether candidate is an accepted key. The comparison takes
// the same time for each candidate: it compares digests, and it examines all
// keys.
func (k *Keys) Match(candidate string) bool {
	if k == nil || candidate == "" {
		return false
	}
	d := sha256.Sum256([]byte(candidate))
	ok := 0
	for _, digest := range *k.digests.Load() {
		ok |= subtle.ConstantTimeCompare(d[:], digest[:])
	}
	return ok == 1
}

// BearerToken returns the token of an "Authorization: Bearer" header.
// It returns false if the header is absent, occurs more than one time, or has
// a different scheme.
func BearerToken(h http.Header) (string, bool) {
	values := h.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}

// RequestAuthorized reports whether the request has an accepted key in its
// Authorization header.
func (k *Keys) RequestAuthorized(r *http.Request) bool {
	token, ok := BearerToken(r.Header)
	return ok && k.Match(token)
}
