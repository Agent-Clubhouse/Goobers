package nowork

import (
	"strings"
	"testing"
)

func TestVerdictConflict(t *testing.T) {
	previous := Record{Count: 1, Verdict: "already-fixed", RunID: "run-a"}
	recorded := Record{Count: 2, Verdict: "not-actionable"}
	want := "**This verdict contradicts the previous one:** run `run-a` recorded `already-fixed`, this run recorded `not-actionable`."
	if got := VerdictConflict(previous, recorded); got != want {
		t.Fatalf("conflict = %q, want %q", got, want)
	}
	for _, pair := range [][2]Record{{{}, recorded}, {previous, {}}, {{Count: 1}, recorded}, {previous, previous}} {
		if got := VerdictConflict(pair[0], pair[1]); got != "" {
			t.Fatalf("unexpected conflict %q", got)
		}
	}
}

func TestVerdictCommentsCarryRecordedEvidence(t *testing.T) {
	recorded := Record{Count: 1, Stage: "implement", Verdict: "already-fixed", Reason: "fixed on main\nverified", Evidence: "02642a86a", RunID: "run-a"}
	for _, tc := range []struct{ name, comment, marker, want string }{
		{"verdict", VerdictComment(recorded, "conflict detail", "run-b", "https://runs.example/run-b"), VerdictMarker, "1 of 3"},
		{"park", ParkComment(recorded, "conflict detail", "run-b", "https://runs.example/run-b"), ParkMarker, "Is this item actually actionable?"},
		{"contradiction", ContradictionComment(recorded, "run-b", "https://runs.example/run-b"), ContradictionMarker, "Run `run-a` had concluded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.HasPrefix(tc.comment, tc.marker) {
				t.Fatalf("missing marker: %s", tc.comment)
			}
			for _, want := range []string{"`implement`", "> fixed on main\n> verified", "02642a86a", "https://runs.example/run-b", tc.want} {
				if !strings.Contains(tc.comment, want) {
					t.Errorf("comment missing %q:\n%s", want, tc.comment)
				}
			}
			if tc.name != "contradiction" && !strings.Contains(tc.comment, "conflict detail") {
				t.Fatalf("missing conflict detail: %s", tc.comment)
			}
		})
	}
}

func TestVerdictCommentsWithoutOptionalDetails(t *testing.T) {
	for _, comment := range []string{VerdictComment(Record{}, "", "run-a", ""), ParkComment(Record{}, "", "run-a", ""), ContradictionComment(Record{}, "run-a", "")} {
		if !strings.Contains(comment, "The stage recorded no reason") || !strings.Contains(comment, "`run-a`") {
			t.Fatalf("missing fallback: %s", comment)
		}
		if strings.Contains(comment, "Cited evidence:") {
			t.Fatalf("unexpected evidence: %s", comment)
		}
	}
	if got := VerdictComment(Record{}, "", "", ""); strings.Contains(got, "Run:") {
		t.Fatalf("unexpected run link: %s", got)
	}
}
