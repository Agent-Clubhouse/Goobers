package gate

import (
	"context"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/learning"
)

func TestRepeatedUnverifiableFindingRoutesToArbitration(t *testing.T) {
	run := newTestJournal(t)
	prior := repeatedFinding("internal/other/other.go:12", "the helper must validate its input")
	learning.NormalizeFinding(&prior, "review", "sha256:first")
	episode := writeLearningEpisodePointer(t, run.Dir(), 1, []apiv1.Finding{prior})
	// The repass DID change the code — the diff digest and the diff itself
	// moved — so #316's identical-diff guard cannot fire. What has not moved
	// is the finding, which still names a location this diff never touches.
	diff := reviewerDiffPointer(t, run, strings.Join([]string{
		"diff --git a/internal/fixture/fixture.go b/internal/fixture/fixture.go",
		"--- a/internal/fixture/fixture.go",
		"+++ b/internal/fixture/fixture.go",
		"@@ -0,0 +1 @@",
		"+const fixture = 1",
		"",
	}, "\n"))
	reviewer := &fakeGoober{reviewVerdict: apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{repeatedFinding("internal/other/other.go:12", "the helper must validate its input")},
	}}
	evaluator := &Evaluator{
		Reviewer:    &ReviewerEvaluator{Goober: reviewer},
		Journal:     run,
		MaxRepasses: 3,
	}

	result, err := evaluator.Evaluate(
		context.Background(),
		reviewerEvidenceGate(),
		apiv1.InvocationEnvelope{ContextPointers: []apiv1.ContextPointer{episode, diff}},
		"implement",
		apiv1.ResultEnvelope{},
		"sha256:second",
		false,
	)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Escalated || result.Target == "implement" {
		t.Fatalf("result = %+v, want arbitration instead of another repass", result)
	}
	if result.Reason != ReasonFindingUnverifiedRepeat {
		t.Fatalf("reason = %q, want %q", result.Reason, ReasonFindingUnverifiedRepeat)
	}
	if !slices.Equal(result.RepeatedFindingIDs, []string{prior.ID}) ||
		!slices.Equal(result.UnverifiedRepeatFindingIDs, []string{prior.ID}) {
		t.Fatalf("repeat bookkeeping = %+v / %+v, want %q", result.RepeatedFindingIDs, result.UnverifiedRepeatFindingIDs, prior.ID)
	}
	if result.Verdict == nil || result.Verdict.Decision != apiv1.VerdictNeedsChanges ||
		len(result.Verdict.Findings) != 1 {
		t.Fatalf("verdict = %+v, want the finding preserved for a human to arbitrate", result.Verdict)
	}
	if !strings.Contains(result.Verdict.Rationale, ReasonFindingUnverifiedRepeat) {
		t.Fatalf("rationale = %q, want the arbitration explanation", result.Verdict.Rationale)
	}

	events := readGateEvents(t, run)
	if len(events) != 1 || events[0].Runner["reason"] != ReasonFindingUnverifiedRepeat {
		t.Fatalf("gate events = %+v, want journaled arbitration reason", events)
	}
	repeated, ok := events[0].Runner["repeatedFindingIdentities"].([]any)
	if !ok || len(repeated) != 1 {
		t.Fatalf("repeatedFindingIdentities = %#v", events[0].Runner["repeatedFindingIdentities"])
	}
	unverified, ok := events[0].Runner["unverifiedRepeatFindingIdentities"].([]any)
	if !ok || len(unverified) != 1 {
		t.Fatalf("unverifiedRepeatFindingIdentities = %#v", events[0].Runner["unverifiedRepeatFindingIdentities"])
	}
}

func TestRepeatedFindingCorroboratedByDiffStillRepasses(t *testing.T) {
	run := newTestJournal(t)
	prior := repeatedFinding("internal/fixture/fixture.go:1", "the constant must be documented")
	learning.NormalizeFinding(&prior, "review", "sha256:first")
	episode := writeLearningEpisodePointer(t, run.Dir(), 1, []apiv1.Finding{prior})
	diff := reviewerDiffPointer(t, run, strings.Join([]string{
		"diff --git a/internal/fixture/fixture.go b/internal/fixture/fixture.go",
		"--- a/internal/fixture/fixture.go",
		"+++ b/internal/fixture/fixture.go",
		"@@ -0,0 +1 @@",
		"+const fixture = 1",
		"",
	}, "\n"))
	reviewer := &fakeGoober{reviewVerdict: apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{repeatedFinding("internal/fixture/fixture.go:1", "the constant must be documented")},
	}}
	evaluator := &Evaluator{
		Reviewer:    &ReviewerEvaluator{Goober: reviewer},
		Journal:     run,
		MaxRepasses: 3,
	}

	result, err := evaluator.Evaluate(
		context.Background(),
		reviewerEvidenceGate(),
		apiv1.InvocationEnvelope{ContextPointers: []apiv1.ContextPointer{episode, diff}},
		"implement",
		apiv1.ResultEnvelope{},
		"sha256:second",
		false,
	)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Escalated || result.Target != "implement" || result.Attempt != 1 {
		t.Fatalf("result = %+v, want a corroborated repeat to charge one ordinary repass", result)
	}
	if result.Reason != "" || len(result.UnverifiedRepeatFindingIDs) != 0 {
		t.Fatalf("result = %+v, want no arbitration for a corroborated finding", result)
	}
	if !slices.Equal(result.RepeatedFindingIDs, []string{prior.ID}) {
		t.Fatalf("repeatedFindingIDs = %+v, want the recurring identity journaled", result.RepeatedFindingIDs)
	}
	events := readGateEvents(t, run)
	if len(events) != 1 {
		t.Fatalf("gate events = %d, want 1", len(events))
	}
	if _, ok := events[0].Runner["unverifiedRepeatFindingIdentities"]; ok {
		t.Fatalf("runner annotations = %+v, want no unverified repeats", events[0].Runner)
	}
}

func TestArbitrateRepeatedFindingsIsFailOpen(t *testing.T) {
	run := newTestJournal(t)
	prior := repeatedFinding("internal/other/other.go:12", "the helper must validate its input")
	learning.NormalizeFinding(&prior, "review", "sha256:first")
	episode := writeLearningEpisodePointer(t, run.Dir(), 1, []apiv1.Finding{prior})
	diff := reviewerDiffPointer(t, run, strings.Join([]string{
		"diff --git a/internal/fixture/fixture.go b/internal/fixture/fixture.go",
		"--- a/internal/fixture/fixture.go",
		"+++ b/internal/fixture/fixture.go",
		"@@ -0,0 +1 @@",
		"+const fixture = 1",
		"",
	}, "\n"))
	repeat := prior
	fresh := repeatedFinding("internal/fixture/other.go:3", "a brand new defect")
	resolve := ArtifactBytesFromRoot(run.Dir())

	for _, tt := range []struct {
		name     string
		verdict  apiv1.Verdict
		pointers []apiv1.ContextPointer
	}{
		{
			name:     "no episode history",
			verdict:  apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Findings: []apiv1.Finding{repeat}},
			pointers: []apiv1.ContextPointer{diff},
		},
		{
			name:     "no reachable diff",
			verdict:  apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Findings: []apiv1.Finding{repeat}},
			pointers: []apiv1.ContextPointer{episode},
		},
		{
			name:     "one finding is new",
			verdict:  apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Findings: []apiv1.Finding{repeat, fresh}},
			pointers: []apiv1.ContextPointer{episode, diff},
		},
		{
			name:     "not a needs-changes verdict",
			verdict:  apiv1.Verdict{Decision: apiv1.VerdictFail, Findings: []apiv1.Finding{repeat}},
			pointers: []apiv1.ContextPointer{episode, diff},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			normalized, arbitration := arbitrateRepeatedFindings(tt.verdict, tt.pointers, resolve, "review")
			if arbitration.Arbitrate {
				t.Fatalf("arbitration = %+v, want the repass to proceed", arbitration)
			}
			if normalized.Rationale != tt.verdict.Rationale || len(normalized.Findings) != len(tt.verdict.Findings) {
				t.Fatalf("verdict was rewritten: %+v", normalized)
			}
		})
	}
}

func repeatedFinding(location, message string) apiv1.Finding {
	return apiv1.Finding{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingSubstantive,
		Message:  message,
		Location: location,
	}
}
