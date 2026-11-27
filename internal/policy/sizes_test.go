package policy

import (
	"strings"
	"testing"
)

// A size above 8Pi does not fit in an int64 as milli-units. The check for a
// whole number of bytes must not refuse it, and must not accept a size that
// the parser of Kubernetes cannot hold.
func TestLargeSizes(t *testing.T) {
	file := func(field string) []byte {
		return []byte("policies:\n  - {id: a, name: A, scope: user, " + field + "}\n")
	}
	for _, size := range []string{"8Pi", "9Pi", "16Pi", "1Ei"} {
		for _, field := range []string{"storage: {size: " + size + "}", "resources: {limits: {memory: " + size + "}}"} {
			if _, err := Parse(file(field)); err != nil {
				t.Errorf("%s: %v, want no error for a whole number of bytes", field, err)
			}
		}
	}
	for _, size := range []string{"2Ei", "8E", "8Ei", "16Ei", "10E", "9223372036854775808", "100000000000000000000"} {
		for _, field := range []string{"storage: {size: " + size + "}", "resources: {limits: {memory: " + size + "}}", "resources: {limits: {ephemeral-storage: " + size + "}}"} {
			_, err := Parse(file(field))
			if err == nil || !strings.Contains(err.Error(), "maximum") {
				t.Errorf("%s: %v, want an error that gives the maximum", field, err)
			}
		}
	}
	// The rule for a part of a byte stays.
	for _, size := range []string{"1500m", "1048576.5", "1048576.0000000001"} {
		_, err := Parse(file("storage: {size: \"" + size + "\"}"))
		if err == nil || !strings.Contains(err.Error(), "whole number of bytes") {
			t.Errorf("storage.size %s: %v, want an error for a part of a byte", size, err)
		}
	}
}
