package main

import (
	"strings"
	"testing"
)

var base = []string{"--api-key-file", "k", "--policy-file", "p"}

func TestParseFlags(t *testing.T) {
	c, err := parseFlags(base)
	if err != nil {
		t.Fatal(err)
	}
	if c.listen != ":8080" || c.adminListen != ":9090" || c.allowSetCookie || c.logLevel != "info" {
		t.Errorf("defaults = %+v", c)
	}
}

func TestParseFlagsRejectsBadCommandLines(t *testing.T) {
	cases := map[string][]string{
		"no key file":    {"--policy-file", "p"},
		"no policy file": {"--api-key-file", "k"},
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
	t.Setenv("WARREN_ALLOW_SET_COOKIE", "yes please")
	if _, err := parseFlags(base); err == nil || !strings.Contains(err.Error(), "WARREN_ALLOW_SET_COOKIE") {
		t.Errorf("bad boolean: error = %v", err)
	}
}
