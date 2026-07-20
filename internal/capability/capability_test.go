package capability

import "testing"

func TestKnownAcceptsEveryCanonicalCapability(t *testing.T) {
	for _, c := range All() {
		if !Known(string(c)) {
			t.Errorf("Known(%q) = false, want true (member of All())", c)
		}
	}
}

func TestKnownRejectsUnknownOrMisspelledCapability(t *testing.T) {
	for _, s := range []string{
		"",
		"github:prs:write",   // the issue #74 typo this registry exists to catch
		"github:pulls:write", // the api/v1alpha1 test-fixture drift #74 also found
		"repo:pull",
		"telemetry:write",
	} {
		if Known(s) {
			t.Errorf("Known(%q) = true, want false", s)
		}
	}
}

func TestAllHasNoDuplicates(t *testing.T) {
	seen := map[Capability]bool{}
	for _, c := range All() {
		if seen[c] {
			t.Errorf("duplicate capability %q in All()", c)
		}
		seen[c] = true
	}
}

func TestSuggestReturnsLikelyTypo(t *testing.T) {
	got, ok := Suggest("github:prs:write")
	if !ok || got != GitHubPRWrite {
		t.Fatalf("Suggest(github:prs:write) = %q, %t; want %q, true", got, ok, GitHubPRWrite)
	}
}

func TestSuggestRejectsDistantAndCanonicalValues(t *testing.T) {
	for _, value := range []string{"not-a-capability", string(RepoPush)} {
		if got, ok := Suggest(value); ok {
			t.Errorf("Suggest(%q) = %q, true; want no suggestion", value, got)
		}
	}
}
