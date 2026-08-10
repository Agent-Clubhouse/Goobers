package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestValidateRemediationAlgorithm pins the configurable remediation-algorithm
// contract: fifo (the only supported value) and the unset default validate
// silently, while an unrecognized value warns and is treated as fifo rather
// than failing the stage — mirroring resolveMinSeverity / resolveElectionPolicy.
func TestValidateRemediationAlgorithm(t *testing.T) {
	cases := []struct {
		name      string
		value     string // "" means the input is unset (default)
		wantWarn  bool
		wantValue bool // input explicitly set
	}{
		{name: "unset defaults to fifo, no warning", value: "", wantWarn: false},
		{name: "explicit fifo, no warning", value: "fifo", wantWarn: false, wantValue: true},
		{name: "unknown value warns", value: "lifo", wantWarn: true, wantValue: true},
		{name: "sibling-overlap is not a selectable algorithm", value: "sibling-overlap", wantWarn: true, wantValue: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantValue {
				t.Setenv("GOOBERS_INPUT_REMEDIATIONALGORITHM", tc.value)
			}
			var stderr bytes.Buffer
			validateRemediationAlgorithm(&stderr)
			gotWarn := strings.Contains(stderr.String(), "remediationAlgorithm")
			if gotWarn != tc.wantWarn {
				t.Fatalf("warning emitted = %v, want %v (stderr: %q)", gotWarn, tc.wantWarn, stderr.String())
			}
			if tc.wantWarn && !strings.Contains(stderr.String(), remediationAlgorithmFIFO) {
				t.Fatalf("warning should name the fifo fallback; got %q", stderr.String())
			}
		})
	}
}
