package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// makeFindings builds n minimal findings for coverage-count assertions.
func makeFindings(n int) []apiv1.Finding {
	out := make([]apiv1.Finding, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, apiv1.Finding{
			Severity: apiv1.SeverityError,
			Message:  "finding",
		})
	}
	return out
}

// The defect this exists for: a model emitting findingResponses as nested JSON
// inside a JSON string value consistently drops the outer closing quote --
//
//	"findingResponses":"[{\"finding\":1,...\"detail\":\"...\"}]},"summary":...
//
// The line-oriented fallback carries the same information without nested
// quoting.
func TestParseFindingResponsesAcceptsLineForm(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []findingDisposition
	}{
		{
			name: "single finding",
			raw:  "1: addressed: added the negative assertions",
			want: []findingDisposition{{Finding: 1, Disposition: "addressed", Detail: "added the negative assertions"}},
		},
		{
			name: "two findings",
			raw:  "1: addressed: added malformed-JSON rejection\n2: declined: out of scope for this item",
			want: []findingDisposition{
				{Finding: 1, Disposition: "addressed", Detail: "added malformed-JSON rejection"},
				{Finding: 2, Disposition: "declined", Detail: "out of scope for this item"},
			},
		},
		{
			name: "tolerates list markers and blank lines",
			raw:  "- 1: addressed: first\n\n* 2: declined: second\n",
			want: []findingDisposition{
				{Finding: 1, Disposition: "addressed", Detail: "first"},
				{Finding: 2, Disposition: "declined", Detail: "second"},
			},
		},
		{
			name: "detail may contain colons",
			raw:  "1: addressed: see tests/contract_test.hew:55-63 for the new assertions",
			want: []findingDisposition{{Finding: 1, Disposition: "addressed", Detail: "see tests/contract_test.hew:55-63 for the new assertions"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFindingResponses(tc.raw)
			if err != nil {
				t.Fatalf("parseFindingResponses(%q) failed: %v", tc.raw, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d responses, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i].Finding != tc.want[i].Finding ||
					got[i].Disposition != tc.want[i].Disposition ||
					got[i].Detail != tc.want[i].Detail {
					t.Fatalf("response %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The canonical JSON array must keep working unchanged -- the fallback is
// additive, not a replacement.
func TestParseFindingResponsesStillAcceptsJSON(t *testing.T) {
	raw := `[{"finding":1,"disposition":"addressed","detail":"added assertions"},{"finding":2,"disposition":"declined","detail":"out of scope"}]`
	got, err := parseFindingResponses(raw)
	if err != nil {
		t.Fatalf("JSON form rejected: %v", err)
	}
	if len(got) != 2 || got[0].Finding != 1 || got[1].Disposition != "declined" {
		t.Fatalf("JSON form parsed wrong: %+v", got)
	}
}

// Genuinely unparseable input must still fail, and the error should name the
// canonical form so the diagnostic stays useful.
func TestParseFindingResponsesRejectsGarbage(t *testing.T) {
	if _, err := parseFindingResponses("I fixed everything, trust me"); err == nil {
		t.Fatal("accepted prose as finding responses")
	}
	if _, err := parseFindingResponses(`{"finding":1}`); err == nil {
		t.Fatal("accepted a bare object where an array was required")
	}
}

// End-to-end through validateFindingResponses: the line form must satisfy the
// same per-finding rules as JSON (disposition vocabulary, non-empty detail,
// exact coverage).
func TestValidateFindingResponsesLineFormEnforcesRules(t *testing.T) {
	findings := makeFindings(2)

	ok, err := validateFindingResponses(findings, "1: addressed: did the thing\n2: declined: not in scope")
	if err != nil {
		t.Fatalf("valid line form rejected: %v", err)
	}
	if len(ok) != 2 {
		t.Fatalf("got %d dispositions, want 2", len(ok))
	}

	if _, err := validateFindingResponses(findings, "1: addressed: did the thing"); err == nil {
		t.Fatal("accepted a response set covering only one of two findings")
	}
	if _, err := validateFindingResponses(findings, "1: fixed: did the thing\n2: declined: nope"); err == nil {
		t.Fatal("accepted an invalid disposition word")
	}
	if _, err := validateFindingResponses(findings, "1: addressed: \n2: declined: nope"); err == nil {
		t.Fatal("accepted an empty detail")
	}
}
