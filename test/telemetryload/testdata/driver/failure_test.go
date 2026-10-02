package main

import "testing"

func TestClassifyRunFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		output   string
		timedOut bool
		want     string
	}{
		{"refused", "error: class_saturated: too many concurrent mutation requests", false, "class_saturated"},
		{"accepted but status unavailable", "accepted trigger trigger-123\nerror: trigger trigger-123 remains accepted; could not observe dispatch: trigger status returned HTTP 503", false, "accepted_status_unavailable"},
		{"deadline", "", true, "cli_timeout"},
		{"other", "error: unexpected response", false, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRunFailure(tc.output, tc.timedOut); got != tc.want {
				t.Fatalf("classifyRunFailure(%q, %t) = %q, want %q", tc.output, tc.timedOut, got, tc.want)
			}
		})
	}
}
