package builtincmd

import (
	"sort"
	"testing"
)

func TestNamesIsSortedAndDeduplicated(t *testing.T) {
	got := Names()
	if !sort.StringsAreSorted(got) {
		t.Fatalf("inventory is not sorted: %v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			t.Fatalf("inventory has duplicate entry %q", got[i])
		}
	}
}

func TestNamesReturnsACopy(t *testing.T) {
	first := Names()
	first[0] = "mutated"
	if second := Names(); second[0] == "mutated" {
		t.Fatal("Names() exposes the underlying inventory slice; callers can corrupt it")
	}
}

func TestKnownCoversWorkflowInvokedNonManifestCommands(t *testing.T) {
	// The names shipped/reference workflows shell out to that
	// internal/providerstage's manifest does NOT describe — the whole reason
	// this inventory exists (#2861 wave, part 1). ci-poll and
	// external-telemetry are deliberately absent: those invocations carry a
	// matching inputs.kind and never shell out.
	for _, name := range []string{
		"__demo-provider",
		"check-fail-first",
		"docs-churn",
		"gate-removal-guard",
		"self-update",
		"validate",
	} {
		if !Known(name) {
			t.Errorf("Known(%q) = false, want true (invoked by shipped/reference workflows)", name)
		}
	}
	for _, name := range []string{"ci-poll", "external-telemetry"} {
		if Known(name) {
			t.Errorf("Known(%q) = true, want false (kind-backed placeholder, never a shell-out)", name)
		}
	}
}

func TestKnownRejectsOperatorCommands(t *testing.T) {
	// Real CLI commands that are still not sanctioned stage invocations.
	for _, name := range []string{"up", "down", "status", "init", "dashboard"} {
		if Known(name) {
			t.Errorf("Known(%q) = true, want false (operator command, not a stage invocation)", name)
		}
	}
}

func TestSuggestFindsNearMisses(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"push-brach", "push-branch"},
		{"open-prr", "open-pr"},
		{"merge-prs", "merge-pr"},
		{"validat", "validate"},
	}
	for _, tc := range cases {
		got, ok := Suggest(tc.in)
		if !ok || got != tc.want {
			t.Errorf("Suggest(%q) = %q, %t; want %q, true", tc.in, got, ok, tc.want)
		}
	}
}

func TestSuggestStaysQuietWhenNothingIsClose(t *testing.T) {
	if got, ok := Suggest("definitely-not-a-command"); ok {
		t.Fatalf("Suggest for a far-off name = %q, want no suggestion", got)
	}
	if got, ok := Suggest("open-pr"); ok {
		t.Fatalf("Suggest(%q) for a known name = %q, want no suggestion", "open-pr", got)
	}
}
