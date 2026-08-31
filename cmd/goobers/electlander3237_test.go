package main

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestConflictOverlapFindingIsOrderingFinding is #3237's first trigger, taken
// verbatim from the live cluster #3227/#3235: the winner's only finding was an
// `error/conflict` whose remedy is literally "sequence these two PRs" — the
// decision FIFO makes. Classed conflict rather than cross-pr-blocked, it
// counted as a non-ordering finding and the crown was withheld, leaving the
// cluster with no lander at all.
func TestConflictOverlapFindingIsOrderingFinding(t *testing.T) {
	findings := []apiv1.Finding{{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingConflict,
		Message:  "Reconcile or sequence the overlapping CLI documentation/help changes so neither PR overwrites or invalidates the other's generated output.",
		Location: "PR #3235: cmd/goobers/testdata/help.golden, docs/cli/README.md",
	}}
	siblings := []int{3235}

	effective := withOverlapBackstop(findings, siblings)
	if !allCrossPRBlocked(effective) {
		t.Fatalf("conflict finding naming only an overlapping sibling was not normalized: %+v", effective)
	}
	if want := []int{3235}; !reflect.DeepEqual(unionBlockingPRs(effective), want) {
		t.Fatalf("blockers = %v, want %v", unionBlockingPRs(effective), want)
	}
	if !electionDecision(effective, 3227, electedLander, nil) {
		t.Fatal("FIFO winner whose only finding is the overlap itself was not crowned")
	}
	if reason := noLanderEscalationReason(
		apiv1.VerdictNeedsChanges, effective, 3227, siblings, electedLander, nil, "fifo",
	); reason != "" {
		t.Fatalf("escalated a crownable winner for human intervention: %s", reason)
	}
}

// TestBaseConflictFindingStillBlocksCrowning is the guard on the first trigger:
// a genuine conflict against the base — no sibling named, or a file the overlap
// set does not cover — is a real defect and must never be laundered into an
// ordering finding.
func TestBaseConflictFindingStillBlocksCrowning(t *testing.T) {
	cases := map[string]apiv1.Finding{
		"conflict against the base names no sibling": {
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingConflict,
			Message:  "rebase does not apply cleanly",
			Location: "internal/engine/engine.go:42",
		},
		"conflict names a PR outside the deterministic overlap set": {
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingConflict,
			Message:  "conflicting change",
			Location: "PR #4242: internal/engine/engine.go",
		},
	}
	for name, finding := range cases {
		t.Run(name, func(t *testing.T) {
			effective := withOverlapBackstop([]apiv1.Finding{finding}, []int{3235})
			if allCrossPRBlocked(effective) {
				t.Fatalf("real conflict was normalized to an ordering finding: %+v", effective)
			}
			if electionDecision(effective, 3227, electedLander, nil) {
				t.Fatal("crowned a PR carrying a genuine conflict")
			}
		})
	}
}

// TestScopeGateEchoDoesNotBlockCrowning is #3237's second trigger, from the
// live clusters #3185/#3187 and #3187/#3190: the reviewer restated the #1313
// scope gate as an error-severity finding on the winner, so the crown was
// withheld. The scope gate is operator-acknowledgeable and is enforced
// deterministically on the merge path after the election, so it must gate the
// merge, not the ordering decision.
func TestScopeGateEchoDoesNotBlockCrowning(t *testing.T) {
	findings := []apiv1.Finding{
		{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Message:  "The PR changes 2,123 lines, exceeding the 2,000-line scope threshold. Reduce or split the change.",
			Location: "PR #3185",
		},
		{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingConflict,
			Message:  "Sibling PR #3187 also changes test/deadcode/exemptions.txt. Reconcile the edits and refresh the selected PR.",
			Location: "PR #3187: test/deadcode/exemptions.txt",
		},
	}
	siblings := []int{3187}

	effective := withOverlapBackstop(findings, siblings)
	if !electionDecision(effective, 3185, electedLander, nil) {
		t.Fatalf("scope-gate echo withheld the crown from the FIFO winner: %+v", effective)
	}
	if reason := noLanderEscalationReason(
		apiv1.VerdictNeedsChanges, effective, 3185, siblings, electedLander, nil, "fifo",
	); reason != "" {
		t.Fatalf("escalated a crownable winner for human intervention: %s", reason)
	}
	if len(effective) != len(findings) {
		t.Fatalf("published findings = %d, want %d — the scope-gate finding stays on the verdict", len(effective), len(findings))
	}
}

func TestFindingIsScopeGateEcho(t *testing.T) {
	cases := []struct {
		name    string
		finding apiv1.Finding
		want    bool
	}{
		{
			name: "size threshold restatement on the whole diff",
			finding: apiv1.Finding{
				Class:    apiv1.FindingSubstantive,
				Severity: apiv1.SeverityError,
				Message:  "The PR changes 2,123 lines, exceeding the 2,000-line scope threshold. Reduce or split the change.",
				Location: "PR #3185",
			},
			want: true,
		},
		{
			name: "scope-creep class naming the gate label",
			finding: apiv1.Finding{
				Class:    apiv1.FindingScopeCreep,
				Severity: apiv1.SeverityError,
				Message:  "This PR is parked by goobers:scope-gate.",
			},
			want: true,
		},
		{
			name: "defect that merely mentions the gate still counts",
			finding: apiv1.Finding{
				Class:    apiv1.FindingSubstantive,
				Severity: apiv1.SeverityError,
				Message:  "scope gate aside, this drops the nil check",
				Location: "cmd/goobers/electlander.go:120",
			},
			want: false,
		},
		{
			name: "ordinary oversized-diff wording without the gate is not an echo",
			finding: apiv1.Finding{
				Class:    apiv1.FindingSubstantive,
				Severity: apiv1.SeverityError,
				Message:  "This change is large; consider splitting it.",
			},
			want: false,
		},
		{
			name: "contract-change class is never a gate echo",
			finding: apiv1.Finding{
				Class:    apiv1.FindingContractChange,
				Severity: apiv1.SeverityError,
				Message:  "scope gate: unauthorized schema change",
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findingIsScopeGateEcho(tc.finding); got != tc.want {
				t.Fatalf("findingIsScopeGateEcho = %v, want %v", got, tc.want)
			}
			if got := findingIsRealDefect(tc.finding); got == tc.want {
				t.Fatalf("findingIsRealDefect = %v, want %v", got, !tc.want)
			}
		})
	}
}

func TestLocationNamesFile(t *testing.T) {
	cases := map[string]bool{
		"":                                  false,
		"PR #3185":                          false,
		"(PR #3185)":                        false,
		"PR #3187, PR #3190":                false,
		"PR #3187: test/deadcode/x.txt":     true,
		"cmd/goobers/electlander.go:120":    true,
		"PR #3187 (docs/cli/README.md)":     true,
		"docs/cli/README.md, PR #3187 note": true,
	}
	for location, want := range cases {
		if got := locationNamesFile(location); got != want {
			t.Errorf("locationNamesFile(%q) = %v, want %v", location, got, want)
		}
	}
}
