package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestFindingIsCleanRebaseNeed pins the class line #5576 turns on: the
// finding-class contract already separates "the base advanced" (rebase-needed)
// from "the base does not apply cleanly" (conflict), so no new signal has to be
// plumbed to tell a fast-forwardable base from a conflicting one.
func TestFindingIsCleanRebaseNeed(t *testing.T) {
	cases := []struct {
		name          string
		finding       apiv1.Finding
		wantCleanNeed bool
		wantDefect    bool
	}{
		{
			name:          "rebase-needed at error severity is an ordering fact, not a defect",
			finding:       apiv1.Finding{Class: apiv1.FindingRebaseNeeded, Severity: apiv1.SeverityError, Message: "base is 2 commits behind main"},
			wantCleanNeed: true,
			wantDefect:    false,
		},
		{
			name:          "severity does not promote a behind base into a defect",
			finding:       apiv1.Finding{Class: apiv1.FindingRebaseNeeded, Severity: apiv1.SeverityCritical, Message: "base advanced again"},
			wantCleanNeed: true,
			wantDefect:    false,
		},
		{
			name:          "conflict is the class for a base that does not apply cleanly, and still blocks",
			finding:       apiv1.Finding{Class: apiv1.FindingConflict, Severity: apiv1.SeverityError, Message: "rebase does not apply cleanly"},
			wantCleanNeed: false,
			wantDefect:    true,
		},
		{
			name:          "a substantive finding that merely mentions a rebase still blocks",
			finding:       apiv1.Finding{Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityError, Message: "rebase needed, and this drops the nil check", Location: "cmd/goobers/electlander.go:120"},
			wantCleanNeed: false,
			wantDefect:    true,
		},
		{
			name:          "an unclassified behind-base note is not laundered by its prose",
			finding:       apiv1.Finding{Severity: apiv1.SeverityError, Message: "the base has advanced; please rebase"},
			wantCleanNeed: false,
			wantDefect:    true,
		},
		{
			name:          "#1726: an info finding stays a nit whatever its class",
			finding:       apiv1.Finding{Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityInfo, Message: "there is no semantic conflict"},
			wantCleanNeed: false,
			wantDefect:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findingIsCleanRebaseNeed(tc.finding); got != tc.wantCleanNeed {
				t.Errorf("findingIsCleanRebaseNeed = %v, want %v", got, tc.wantCleanNeed)
			}
			if got := findingIsRealDefect(tc.finding); got != tc.wantDefect {
				t.Errorf("findingIsRealDefect = %v, want %v", got, tc.wantDefect)
			}
		})
	}
}

// TestBehindBaseAloneDoesNotBlockElection is #5576's payoff. A PR whose only
// blocking finding is "your base is behind main" was unlandable on a repo where
// main moves faster than the review cycle: rebase-needed counted as a real
// defect, so withOverlapBackstop never added the ordering finding, the PR was
// never electable, and noLanderEscalationReason deferred the whole cluster —
// forever, because merging main only restarted the cycle (#5517 rode this loop
// while MERGEABLE/CLEAN and green throughout).
func TestBehindBaseAloneDoesNotBlockElection(t *testing.T) {
	behind := apiv1.Finding{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingRebaseNeeded,
		Message:  "base branch has advanced 2 commits since review",
	}
	findings := []apiv1.Finding{behind}
	overlap := []int{5518}

	effective := withOverlapBackstop(findings, overlap)
	if !sequencingOnly(effective) {
		t.Fatalf("a behind-but-clean base still reads as a real defect: %+v", effective)
	}
	if !electionDecision(effective, 5517, electedLander, nil) {
		t.Fatal("PR #5517 is the cluster's lowest member and carries no defect, but was not crowned")
	}
	if reason := noLanderEscalationReason(
		apiv1.VerdictNeedsChanges, effective, 5517, overlap, electedLander, nil, "fifo",
	); reason != "" {
		t.Fatalf("deferred a crownable winner over a behind base: %s", reason)
	}
	if got := verdictLabel(apiv1.VerdictNeedsChanges, effective); got != blockedOnSiblingLabel {
		t.Fatalf("label = %q, want %q — the election and label floors must not disagree (#2988)", got, blockedOnSiblingLabel)
	}
}

// TestConflictingBaseStillBlocksElection is the other half of the distinction:
// a base that does NOT apply cleanly is a `conflict` finding, and that must
// still withhold the crown exactly as before.
func TestConflictingBaseStillBlocksElection(t *testing.T) {
	findings := []apiv1.Finding{{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingConflict,
		Message:  "rebase onto main conflicts in cmd/goobers/applyverdict.go",
		Location: "cmd/goobers/applyverdict.go:115",
	}}
	overlap := []int{5518}

	effective := withOverlapBackstop(findings, overlap)
	if sequencingOnly(effective) {
		t.Fatalf("a conflicting base was treated as sequencing-only: %+v", effective)
	}
	if electionDecision(effective, 5517, electedLander, nil) {
		t.Fatal("crowned a PR whose base does not apply cleanly")
	}
	if got := verdictLabel(apiv1.VerdictNeedsChanges, effective); got != needsRemediationLabel {
		t.Fatalf("label = %q, want %q", got, needsRemediationLabel)
	}
}

// TestBehindBaseElectionPreservesOrderingAndDemotion guards the two invariants
// #5576 must not weaken: the #950 demotion path still refuses to re-crown a
// stuck lander, and the ordering policy still decides who wins.
func TestBehindBaseElectionPreservesOrderingAndDemotion(t *testing.T) {
	findings := []apiv1.Finding{{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingRebaseNeeded,
		Message:  "base branch has advanced",
	}}

	// #950: a demoted PR is never crowned, behind base or not.
	effective := withOverlapBackstop(findings, []int{5518})
	if electionDecision(effective, 5517, electedLander, map[int]bool{5517: true}) {
		t.Fatal("#950: crowned a demoted lander whose only finding was a behind base")
	}

	// A higher-numbered cluster member still defers under fifo.
	effective = withOverlapBackstop(findings, []int{5516})
	if electionDecision(effective, 5517, electedLander, nil) {
		t.Fatal("fifo ordering was bypassed: #5517 was crowned over lower sibling #5516")
	}
	if got := verdictLabel(apiv1.VerdictNeedsChanges, effective); got != blockedOnSiblingLabel {
		t.Fatalf("label = %q, want %q", got, blockedOnSiblingLabel)
	}
}

// TestBehindBaseWithoutSiblingsStillRoutesToRemediation pins the deliberately
// unchanged case: with no cross-PR ordering finding there is no sibling to wait
// for, so a rebase-needed verdict keeps falling through to needs-remediation
// rather than parking on nothing (sequencingOnly's empty-findings rule).
func TestBehindBaseWithoutSiblingsStillRoutesToRemediation(t *testing.T) {
	findings := []apiv1.Finding{{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingRebaseNeeded,
		Message:  "base branch has advanced",
	}}
	if sequencingOnly(findings) {
		t.Fatal("a rebase-needed finding named no sibling but parked as sequencing-only")
	}
	if got := verdictLabel(apiv1.VerdictNeedsChanges, findings); got != needsRemediationLabel {
		t.Fatalf("label = %q, want %q", got, needsRemediationLabel)
	}
	if electionDecision(findings, 5517, electedLander, nil) {
		t.Fatal("crowned a PR with no cluster at all")
	}
}
