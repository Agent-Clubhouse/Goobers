package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestVerdictLabelSequencingOnlyNeverRemediates is #2988's regression pin.
//
// The live shape (a production instance's merge-review run
// dc872629e885baed30b17b6bf2137530, PR #533 at ef10431):
// gather-sibling-context reported hasSiblingOverlap=true and
// hasSubstantiveFindings=false, the reviewer returned pass, and all four
// findings were severity `info` whose own rationale said they were ordering
// concerns rather than defects. apply-verdict nonetheless labelled the PR
// needs-remediation, and the implementer's remediation run completed without
// changing a line — the no-progress loop #717/#747/#2486 each closed by a
// different route. The cause was a predicate mismatch, not a missing case: the
// label split required EVERY finding to carry class cross-pr-blocked, while
// the election floor (findingIsRealDefect) tolerates info severity and
// scope-gate echoes. A verdict could therefore be clean enough to crown and
// defective enough to remediate at the same time. Both now read the same
// floor.
func TestVerdictLabelSequencingOnlyNeverRemediates(t *testing.T) {
	orderingAsk := apiv1.Finding{
		Class:       apiv1.FindingCrossPRBlocked,
		Severity:    apiv1.SeverityInfo,
		Message:     "land #530 first; this only reorders",
		BlockingPRs: []int{530},
	}
	// The exact shape that regressed: an ordering note the reviewer filed as an
	// ordinary info finding rather than classifying it cross-pr-blocked.
	infoOrderingNote := apiv1.Finding{
		Class:    apiv1.FindingSubstantive,
		Severity: apiv1.SeverityInfo,
		Message:  "ordering concern only, no semantic conflict with #530",
		Location: "internal/runner/runner.go:42",
	}
	realDefect := apiv1.Finding{
		Class:    apiv1.FindingSubstantive,
		Severity: apiv1.SeverityWarning,
		Message:  "nil deref when the slice is empty",
		Location: "internal/runner/runner.go:42",
	}

	tests := []struct {
		name     string
		findings []apiv1.Finding
		want     string
	}{
		{
			name:     "live #2988 shape: ordering ask plus info-severity notes",
			findings: []apiv1.Finding{orderingAsk, infoOrderingNote, infoOrderingNote, infoOrderingNote},
			want:     blockedOnSiblingLabel,
		},
		{
			name:     "info-severity note alongside an ordering ask is not a defect",
			findings: []apiv1.Finding{orderingAsk, infoOrderingNote},
			want:     blockedOnSiblingLabel,
		},
		{
			// Unchanged by #2988: a real defect outranks ordering every time.
			name:     "warning-severity defect alongside an ordering ask still remediates",
			findings: []apiv1.Finding{orderingAsk, realDefect},
			want:     needsRemediationLabel,
		},
		{
			// Unchanged by #2988: no ordering finding means no sibling to wait
			// for, so there is nothing to park on even though nothing is a
			// real defect either.
			name:     "info-only findings with no ordering ask still remediate",
			findings: []apiv1.Finding{infoOrderingNote, infoOrderingNote},
			want:     needsRemediationLabel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verdictLabel(apiv1.VerdictNeedsChanges, tt.findings); got != tt.want {
				t.Fatalf("verdictLabel(needs-changes, %d findings) = %q, want %q",
					len(tt.findings), got, tt.want)
			}
		})
	}
}

// TestElectionAndLabelShareOneFloor proves the invariant #2988 restores: a
// finding set can never be simultaneously crownable by the election and
// dispatched to remediation by the label. That pairing is precisely the
// no-progress loop — pr-remediation is handed a PR whose findings the election
// already judged defect-free, so it reproduces the identical diff, checkpoints
// byte-identical, and escalates.
//
// Asserting the relationship rather than two independent expectations is
// deliberate: the previous code passed every case-by-case test it had, and
// still let the two decisions disagree.
func TestElectionAndLabelShareOneFloor(t *testing.T) {
	classes := []apiv1.FindingClass{
		apiv1.FindingCrossPRBlocked,
		apiv1.FindingSubstantive,
		apiv1.FindingScopeCreep,
	}
	severities := []apiv1.Severity{
		apiv1.SeverityInfo,
		apiv1.SeverityWarning,
		apiv1.Severity(""),
	}

	// Every one- and two-finding set drawn from the class/severity grid.
	var singles []apiv1.Finding
	for _, class := range classes {
		for _, severity := range severities {
			singles = append(singles, apiv1.Finding{
				Class:       class,
				Severity:    severity,
				Message:     "finding",
				Location:    "internal/runner/runner.go:42",
				BlockingPRs: []int{530},
			})
		}
	}

	check := func(t *testing.T, findings []apiv1.Finding) {
		t.Helper()
		crownable := sequencingOnly(findings)
		label := verdictLabel(apiv1.VerdictNeedsChanges, findings)
		if crownable && label == needsRemediationLabel {
			t.Fatalf("findings %+v are crownable by the election but labelled %q: "+
				"remediation would be handed a PR with no defect to fix", findings, label)
		}
		if !crownable && label == blockedOnSiblingLabel {
			t.Fatalf("findings %+v are not crownable but labelled %q: "+
				"the PR would park on a sibling the election will not sequence", findings, label)
		}
	}

	for _, a := range singles {
		check(t, []apiv1.Finding{a})
		for _, b := range singles {
			check(t, []apiv1.Finding{a, b})
		}
	}
}
