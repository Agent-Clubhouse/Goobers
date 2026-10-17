package planehttp

import (
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "one second", value: "1", want: time.Second},
		{name: "delta seconds", value: "3", want: 3 * time.Second},
		{name: "surrounding whitespace", value: " \t2\t ", want: 2 * time.Second},
		{name: "at cap", value: "5", want: 5 * time.Second},
		{name: "above cap", value: "6", want: 5 * time.Second},
		{name: "well above cap", value: "60", want: 5 * time.Second},
		{name: "empty", value: "", want: 0},
		{name: "whitespace only", value: " \t ", want: 0},
		{name: "zero", value: "0", want: 0},
		{name: "negative", value: "-1", want: 0},
		{name: "fractional seconds", value: "1.5", want: 0},
		{name: "malformed", value: "later", want: 0},
		{name: "duration units", value: "2s", want: 0},
		// The local control plane emits delta-seconds. HTTP-date values are
		// currently ignored, irrespective of whether they are past or future.
		{name: "past HTTP date", value: "Sun, 06 Nov 1994 08:49:37 GMT", want: 0},
		{name: "future HTTP date", value: "Fri, 06 Nov 2099 08:49:37 GMT", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.value); got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
