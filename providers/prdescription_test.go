package providers

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCapDescriptionWithFooterShortBodyUnchanged(t *testing.T) {
	body := "A concise PR body."
	got := capDescriptionWithFooter(body, "run-1", adoMaxPRDescriptionChars)
	want := withRunIDFooter(body, "run-1")
	if got != want {
		t.Fatalf("short body was altered:\n got=%q\nwant=%q", got, want)
	}
}

func TestCapDescriptionWithFooterTrimsLongBody(t *testing.T) {
	body := strings.Repeat("lorem ipsum dolor sit amet\n", 1000) // ~27k chars
	got := capDescriptionWithFooter(body, "run-42", adoMaxPRDescriptionChars)

	if n := utf8.RuneCountInString(got); n > adoMaxPRDescriptionChars {
		t.Fatalf("result length %d exceeds cap %d", n, adoMaxPRDescriptionChars)
	}
	if !strings.Contains(got, runFooter("run-42")) {
		t.Fatalf("run-id footer was dropped:\n%s", got)
	}
	if !strings.HasSuffix(got, runFooter("run-42")) {
		t.Fatalf("footer must remain at the very end:\n%s", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected a truncation marker:\n%s", got)
	}
}

func TestCapDescriptionWithFooterEmptyRunID(t *testing.T) {
	body := strings.Repeat("x", 10000)
	got := capDescriptionWithFooter(body, "", adoMaxPRDescriptionChars)
	if n := utf8.RuneCountInString(got); n > adoMaxPRDescriptionChars {
		t.Fatalf("result length %d exceeds cap %d", n, adoMaxPRDescriptionChars)
	}
	if strings.Contains(got, "goobers run-id:") {
		t.Fatalf("no footer expected when runID is empty:\n%s", got)
	}
}

func TestCapDescriptionWithFooterZeroDisablesCap(t *testing.T) {
	body := strings.Repeat("y", 10000)
	got := capDescriptionWithFooter(body, "run-9", 0)
	if got != withRunIDFooter(body, "run-9") {
		t.Fatalf("maxChars<=0 should disable the cap")
	}
}

func TestCapDescriptionWithFooterDoesNotSplitRunes(t *testing.T) {
	// Multi-byte runes must never be split by truncation.
	body := strings.Repeat("é", 10000) // each 'é' is 2 bytes
	got := capDescriptionWithFooter(body, "run-u", adoMaxPRDescriptionChars)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8")
	}
	if n := utf8.RuneCountInString(got); n > adoMaxPRDescriptionChars {
		t.Fatalf("result length %d exceeds cap %d", n, adoMaxPRDescriptionChars)
	}
}

// A long body is trimmed from the end, which is where the closing reference
// lives. Post-merge reads that reference to transition the work item, so it
// must survive the cap (an ADO PR otherwise merged without closing its item).
func TestCapDescriptionWithFooterKeepsClosingReferences(t *testing.T) {
	body := "## Summary\n\nImplements #1561.\n\n" + strings.Repeat("local-ci case output line\n", 400) +
		"\nFixes #1561\nCloses https://example.test/org/project/_workitems/edit/77\n"
	got := capDescriptionWithFooter(body, "run-7", adoMaxPRDescriptionChars)
	if n := utf8.RuneCountInString(got); n > adoMaxPRDescriptionChars {
		t.Fatalf("result length %d exceeds cap %d", n, adoMaxPRDescriptionChars)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected the body to be truncated:\n%s", got)
	}
	for _, ref := range []string{"Fixes #1561", "Closes https://example.test/org/project/_workitems/edit/77"} {
		if strings.Count(got, ref) != 1 {
			t.Fatalf("closing reference %q appears %d times, want 1:\n%s", ref, strings.Count(got, ref), got)
		}
	}
	if !strings.HasSuffix(got, runFooter("run-7")) {
		t.Fatalf("footer must remain at the very end:\n%s", got)
	}
}

// A closing reference that survives trimming is not repeated after the marker.
func TestCapDescriptionWithFooterDoesNotRepeatKeptClosingReference(t *testing.T) {
	body := "Fixes #9\n\n" + strings.Repeat("filler line of output\n", 400)
	got := capDescriptionWithFooter(body, "run-8", adoMaxPRDescriptionChars)
	if c := strings.Count(got, "Fixes #9"); c != 1 {
		t.Fatalf("closing reference appears %d times, want 1:\n%s", c, got)
	}
}
