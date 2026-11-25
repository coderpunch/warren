package main

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/h3xcat/warren/internal/policy"
)

var base = []string{"--api-key-file", "k", "--policy-file", "p", "--sandbox-namespace", "sandboxes"}

func TestParseFlags(t *testing.T) {
	c, err := parseFlags(base)
	if err != nil {
		t.Fatal(err)
	}
	if c.listen != ":8080" || c.adminListen != ":9090" || c.allowSetCookie || c.logLevel != "info" ||
		c.idleTimeout != 30*time.Minute || c.maxPerUser != 5 || c.sandboxImage != defaultImage ||
		c.maxVolumes != 20 || c.maxVolumesTotal != 0 || c.volumeDelay != 10*time.Minute {
		t.Errorf("defaults = %+v", c)
	}
	// Development mode needs no namespace.
	if _, err := parseFlags([]string{"--api-key-file", "k", "--policy-file", "p", "--dev-upstream-url", "http://x"}); err != nil {
		t.Errorf("development mode: %v", err)
	}
}

func TestParseFlagsRejectsBadCommandLines(t *testing.T) {
	cases := map[string][]string{
		"no key file":                 {"--policy-file", "p", "--sandbox-namespace", "s"},
		"no policy file":              {"--api-key-file", "k", "--sandbox-namespace", "s"},
		"no namespace":                {"--api-key-file", "k", "--policy-file", "p"},
		"negative limit":              append(append([]string{}, base...), "--max-sandboxes-per-user", "-1"),
		"negative time":               append(append([]string{}, base...), "--idle-timeout", "-1s"),
		"negative volume limit":       append(append([]string{}, base...), "--max-volumes-per-user", "-1"),
		"negative total volume limit": append(append([]string{}, base...), "--max-volumes", "-1"),
		"negative delete delay":       append(append([]string{}, base...), "--volume-delete-delay", "-1m"),
		// The flag package stops at "false" and ignores the TLS flags.
		"positional argument": append(append([]string{}, base...), "--allow-set-cookie", "false", "--tls-cert-file", "c", "--tls-key-file", "k"),
		"certificate only":    append(append([]string{}, base...), "--tls-cert-file", "c"),
		"unknown flag":        append(append([]string{}, base...), "--no-such-flag"),
	}
	for name, args := range cases {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("%s: parseFlags gave no error", name)
		}
	}
	c, err := parseFlags(append(append([]string{}, base...), "--allow-set-cookie=false"))
	if err != nil || c.allowSetCookie {
		t.Errorf("--allow-set-cookie=false: %+v, %v", c, err)
	}
}

func TestEnvironment(t *testing.T) {
	// An empty variable must not replace a default. An empty listen
	// address means port 80.
	t.Setenv("WARREN_LISTEN", "")
	t.Setenv("WARREN_LOG_LEVEL", "")
	c, err := parseFlags(base)
	if err != nil || c.listen != ":8080" || c.logLevel != "info" {
		t.Errorf("empty variables: %+v, %v", c, err)
	}
	t.Setenv("WARREN_LISTEN", ":7000")
	if c, _ := parseFlags(base); c.listen != ":7000" {
		t.Errorf("WARREN_LISTEN: listen = %q", c.listen)
	}
	for value, want := range map[string]bool{"true": true, "TRUE": true, "1": true, "false": false, "0": false} {
		t.Setenv("WARREN_ALLOW_SET_COOKIE", value)
		c, err := parseFlags(base)
		if err != nil || c.allowSetCookie != want {
			t.Errorf("WARREN_ALLOW_SET_COOKIE=%s: %v, %v", value, c.allowSetCookie, err)
		}
	}
	t.Setenv("WARREN_ALLOW_SET_COOKIE", "")
	t.Setenv("WARREN_IDLE_TIMEOUT", "45m")
	t.Setenv("WARREN_MAX_SANDBOXES_PER_USER", "2")
	t.Setenv("WARREN_MAX_VOLUMES_PER_USER", "7")
	t.Setenv("WARREN_MAX_VOLUMES", "300")
	if c, err := parseFlags(base); err != nil || c.idleTimeout != 45*time.Minute || c.maxPerUser != 2 ||
		c.maxVolumes != 7 || c.maxVolumesTotal != 300 {
		t.Errorf("limits from the environment: %+v, %v", c, err)
	}
	t.Setenv("WARREN_IDLE_TIMEOUT", "soon")
	if _, err := parseFlags(base); err == nil {
		t.Error("a bad duration gave no error")
	}
	t.Setenv("WARREN_IDLE_TIMEOUT", "")
	t.Setenv("WARREN_ALLOW_SET_COOKIE", "yes please")
	if _, err := parseFlags(base); err == nil || !strings.Contains(err.Error(), "WARREN_ALLOW_SET_COOKIE") {
		t.Errorf("bad boolean: error = %v", err)
	}
}

func TestWatchPoliciesReplacesTheSet(t *testing.T) {
	old := reloadInterval
	reloadInterval = 10 * time.Millisecond
	defer func() { reloadInterval = old }()

	path := filepath.Join(t.TempDir(), "policies.yaml")
	write := func(content string) {
		t.Helper()
		// Kubernetes replaces the file of a ConfigMap with a rename.
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	first := "policies:\n  - {id: a, name: A, scope: user}\n"
	write(first)
	set, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := policy.NewStore(set)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchPolicies(ctx, path, sha256.Sum256([]byte(first)), store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	has := func(id string) bool { _, ok := store.Get(id); return ok }
	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timeout: %s", what)
	}
	write("policies:\n  - {id: a, name: A, scope: user, users: [alice]}\n  - {id: b, name: B, scope: chat}\n")
	eventually("the new policy", func() bool { return has("b") })
	if p, _ := store.Get("a"); p.Allows("bob") {
		t.Error("the user list of the new file is not in use")
	}
	// A file that is not valid must not replace or remove the policies.
	write("policies:\n  - {id: c, name: C, scope: user, privileged: true}\n")
	time.Sleep(100 * time.Millisecond)
	if !has("a") || !has("b") || has("c") {
		t.Error("a file that is not valid changed the policies")
	}
	// A corrected file comes into use.
	write("policies:\n  - {id: c, name: C, scope: user}\n")
	eventually("the corrected file", func() bool { return has("c") && !has("a") })
}
