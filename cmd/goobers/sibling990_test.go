package main

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func crossPR(blockers ...int) apiv1.Finding {
	return apiv1.Finding{Class: apiv1.FindingCrossPRBlocked, BlockingPRs: blockers}
}

func substantive() apiv1.Finding {
	return apiv1.Finding{Class: apiv1.FindingSubstantive, Message: "a real bug"}
}

// TestWithOverlapBackstop is #990: the deterministic file-overlap set is folded
// into the findings used for sequencing routing — additively, and never over a
// real defect.
func TestWithOverlapBackstop(t *testing.T) {
	t.Run("no overlap is a no-op", func(t *testing.T) {
		in := []apiv1.Finding{crossPR(11)}
		if got := withOverlapBackstop(in, nil); !reflect.DeepEqual(got, in) {
			t.Fatalf("got %+v, want unchanged %+v", got, in)
		}
	})
	t.Run("a real defect is left to remediation", func(t *testing.T) {
		in := []apiv1.Finding{substantive()}
		got := withOverlapBackstop(in, []int{11})
		if allCrossPRBlocked(got) {
			t.Fatalf("a verdict with a real defect must not become all-cross-pr-blocked: %+v", got)
		}
	})
	t.Run("no findings + overlap synthesizes a cross-pr-blocked finding", func(t *testing.T) {
		got := withOverlapBackstop(nil, []int{11})
		if !allCrossPRBlocked(got) {
			t.Fatalf("want all-cross-pr-blocked, got %+v", got)
		}
		if want := []int{11}; !reflect.DeepEqual(unionBlockingPRs(got), want) {
			t.Fatalf("blockers = %v, want %v", unionBlockingPRs(got), want)
		}
	})
	t.Run("union completes an under-named reviewer verdict", func(t *testing.T) {
		// Reviewer named #11 only; deterministic overlap knows #11 and #12.
		got := withOverlapBackstop([]apiv1.Finding{crossPR(11)}, []int{11, 12})
		if want := []int{11, 12}; !reflect.DeepEqual(unionBlockingPRs(got), want) {
			t.Fatalf("blockers = %v, want %v", unionBlockingPRs(got), want)
		}
	})
	t.Run("does not alias the caller's backing array", func(t *testing.T) {
		in := []apiv1.Finding{crossPR(11)}
		_ = withOverlapBackstop(in, []int{12})
		if len(in) != 1 {
			t.Fatalf("caller's slice was mutated: %+v", in)
		}
	})
}

func TestWithOverlapBackstopReclassifiesPR2478OverlapOnlyVerdict(t *testing.T) {
	findings := []apiv1.Finding{
		{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Message:  "PR #2481 also changes docs/guides/quickstart.md; reconcile or sequence these edits.",
			Location: "PR #2481 (docs/guides/quickstart.md)",
		},
		{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Message:  "PR #2476 also changes docs/guides/quickstart.md; reconcile or sequence these edits.",
			Location: "PR #2476 (docs/guides/quickstart.md)",
		},
		{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Message:  "PR #2475 overlaps quickstart documentation; establish merge ordering.",
			Location: "PR #2475 (docs/guides/quickstart.md, docs/guides/quickstart-linux.md)",
		},
	}

	effective := withOverlapBackstop(findings, []int{2481, 2476, 2475})
	if !allCrossPRBlocked(effective) {
		t.Fatalf("#2478 overlap-only findings were not normalized: %+v", effective)
	}
	if want := []int{2475, 2476, 2481}; !reflect.DeepEqual(unionBlockingPRs(effective), want) {
		t.Fatalf("blockers = %v, want %v", unionBlockingPRs(effective), want)
	}
	if got := verdictLabel(apiv1.VerdictNeedsChanges, effective); got != blockedOnSiblingLabel {
		t.Fatalf("label = %q, want %q", got, blockedOnSiblingLabel)
	}
}

func TestWithOverlapBackstopPreservesMixedRemediationFindings(t *testing.T) {
	overlap := apiv1.Finding{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingSubstantive,
		Message:  "PR #11 changes the same file.",
		Location: "PR #11 (docs/guides/quickstart.md)",
	}
	for _, finding := range []apiv1.Finding{
		{Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "selected PR has a broken link", Location: "docs/guides/quickstart.md:42"},
		{Severity: apiv1.SeverityError, Class: apiv1.FindingConflict, Message: "merge conflict"},
		{Severity: apiv1.SeverityError, Class: apiv1.FindingRebaseNeeded, Message: "base advanced"},
	} {
		effective := withOverlapBackstop([]apiv1.Finding{overlap, finding}, []int{11})
		if got := verdictLabel(apiv1.VerdictNeedsChanges, effective); got != needsRemediationLabel {
			t.Errorf("class %q label = %q, want %q", finding.Class, got, needsRemediationLabel)
		}
	}
}

// TestOverlapBackstopDrivesElection is #990's routing payoff: a green PR whose
// reviewer verdict under-named or missed the collision is still correctly
// elected/parked once the deterministic overlap is folded in — and a real
// defect is never elected.
func TestOverlapBackstopDrivesElection(t *testing.T) {
	cases := []struct {
		name        string
		selected    int
		findings    []apiv1.Finding
		overlap     []int
		wantElected bool
	}{
		{
			name:        "lowest in the deterministic cluster is elected even when the reviewer named no blocker",
			selected:    11,
			findings:    nil, // reviewer returned needs-changes with no structured finding
			overlap:     []int{12},
			wantElected: true,
		},
		{
			name:        "non-lowest parks even when the reviewer left blockers empty",
			selected:    12,
			findings:    nil,
			overlap:     []int{11},
			wantElected: false, // #11 is lower — backstop supplies it, so #12 defers
		},
		{
			name:        "a real defect is never elected, overlap notwithstanding",
			selected:    10,
			findings:    []apiv1.Finding{substantive()},
			overlap:     []int{11},
			wantElected: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eff := withOverlapBackstop(c.findings, c.overlap)
			got := electionDecision(eff, c.selected, electedLander, nil)
			if got != c.wantElected {
				t.Fatalf("electionDecision(selected=#%d, overlap=%v) = %v, want %v", c.selected, c.overlap, got, c.wantElected)
			}
			// A non-elected sequencing case must label blocked-on-sibling, not
			// needs-remediation, so it self-heals when its blocker lands.
			if !c.wantElected && len(c.findings) == 0 {
				if got := verdictLabel(apiv1.VerdictNeedsChanges, eff); got != blockedOnSiblingLabel {
					t.Fatalf("label = %q, want %q", got, blockedOnSiblingLabel)
				}
			}
		})
	}
}
