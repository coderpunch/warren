// Package auth authenticates the Open WebUI backend with the connection key.
//
// Warren accepts only an orchestrator connection. On that connection Open
// WebUI sends one static key on each request. The key proves that the caller
// is the Open WebUI backend. It does not prove which user made the request.
package auth

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// MinKeyLength is the minimum number of characters in a connection key.
const MinKeyLength = 32

// Keys is a set of accepted connection keys. More than one key lets an
// administrator replace a key with no downtime.
type Keys struct {
	digests [][sha256.Size]byte
}

// NewKeys makes a key set. It rejects an empty set and a short key.
func NewKeys(keys []string) (*Keys, error) {
	if len(keys) == 0 {
		return nil, errors.New("no connection key")
	}
	k := &Keys{}
	for i, key := range keys {
		if len(key) < MinKeyLength {
			return nil, fmt.Errorf("connection key %d has fewer than %d characters", i+1, MinKeyLength)
		}
		if strings.ContainsAny(key, " \t\r\n") {
			return nil, fmt.Errorf("connection key %d contains white space", i+1)
		}
		k.digests = append(k.digests, sha256.Sum256([]byte(key)))
	}
	return k, nil
}

// LoadKeyFile reads a key set from a file. Each line that is not empty and
// does not start with '#' is one key.
func LoadKeyFile(path string) (*Keys, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open key file: %w", err)
	}
	defer f.Close()
	var keys []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
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
	for i := range k.digests {
		ok |= subtle.ConstantTimeCompare(d[:], k.digests[i][:])
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
