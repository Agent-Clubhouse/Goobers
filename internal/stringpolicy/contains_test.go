package stringpolicy

import "testing"

func TestContainsAnySubstringContract(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      string
		substrings []string
		want       bool
	}{
		{name: "no substrings", value: "error"},
		{name: "empty list", value: "error", substrings: []string{}},
		{name: "empty value", substrings: []string{"error"}},
		{name: "empty substring", value: "error", substrings: []string{""}, want: true},
		{name: "both empty", substrings: []string{""}, want: true},
		{name: "later substring", value: "git: index.lock exists", substrings: []string{"unknown revision", "index.lock"}, want: true},
		{name: "complete substring required", value: "index", substrings: []string{"index.lock"}},
		{name: "case sensitive", value: "Permission denied", substrings: []string{"permission denied"}},
		{name: "unicode substring", value: "échec: accès refusé", substrings: []string{"accès refusé"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContainsAny(tc.value, tc.substrings...); got != tc.want {
				t.Fatalf("ContainsAny(%q, %q) = %v, want %v", tc.value, tc.substrings, got, tc.want)
			}
		})
	}
}
