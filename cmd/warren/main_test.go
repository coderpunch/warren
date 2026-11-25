package main

import (
	"strings"
	"testing"
	"time"
)

var base = []string{"--api-key-file", "k", "--policy-file", "p", "--sandbox-namespace", "sandboxes"}

func TestParseFlags(t *testing.T) {
	c, err := parseFlags(base)
	if err != nil {
		t.Fatal(err)
	}
	if c.listen != ":8080" || c.adminListen != ":9090" || c.allowSetCookie || c.logLevel != "info" ||
		c.idleTimeout != 30*time.Minute || c.maxPerUser != 5 || c.sandboxImage != defaultImage {
		t.Errorf("defaults = %+v", c)
	}
	// Development mode needs no namespace.
	if _, err := parseFlags([]string{"--api-key-file", "k", "--policy-file", "p", "--dev-upstream-url", "http://x"}); err != nil {
		t.Errorf("development mode: %v", err)
	}
}

func TestParseFlagsRejectsBadCommandLines(t *testing.T) {
	cases := map[string][]string{
		"no key file":    {"--policy-file", "p", "--sandbox-namespace", "s"},
		"no policy file": {"--api-key-file", "k", "--sandbox-namespace", "s"},
		"no namespace":   {"--api-key-file", "k", "--policy-file", "p"},
		"negative limit": append(append([]string{}, base...), "--max-sandboxes-per-user", "-1"),
		"negative time":  append(append([]string{}, base...), "--idle-timeout", "-1s"),
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
	if c, err := parseFlags(base); err != nil || c.idleTimeout != 45*time.Minute || c.maxPerUser != 2 {
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
