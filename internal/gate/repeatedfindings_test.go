package gate

import (
	"context"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

func repeatedFindingPatch() string {
	return strings.Join([]string{
		"diff --git a/fixture.go b/fixture.go",
		"--- a/fixture.go",
		"+++ b/fixture.go",
		"@@ -0,0 +1 @@",
		"+const fixture = 1",
		"",
	}, "\n")
}

func TestEvaluatorArbitratesRepeatedFindingTheDiffCannotCorroborate(t *testing.T) {
	run := newTestJournal(t)
	diff := reviewerDiffPointer(t, run, repeatedFindingPatch())
	prior := apiv1.Finding{
		ID: "finding-repeat", LearningSignature: "sig-repeat",
		LearningClassification: apiv1.LearningInstruction,
		Severity:               apiv1.SeverityError,
		Message:                "the helper must be renamed",
		Location:               "elsewhere.go:42",
	}
	episode := writeLearningEpisodePointer(t, run.Dir(), 10, []apiv1.Finding{prior})
	reviewer := &fakeGoober{reviewVerdict: apiv1.Verdict{
		Decision:  apiv1.VerdictNeedsChanges,
		Rationale: "the helper is still misnamed",
		Findings:  []apiv1.Finding{prior},
	}}
	evaluator := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: reviewer}, Journal: run}

	result, err := evaluator.Evaluate(
		context.Background(),
		reviewerEvidenceGate(),
		apiv1.InvocationEnvelope{ContextPointers: []apiv1.ContextPointer{diff, episode}},
		"implement",
		apiv1.ResultEnvelope{},
		"sha256:second",
		false,
	)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Escalated || result.Target != wf.TargetEscalate {
		t.Fatalf("result = %+v, want arbitration escalation instead of another repass", result)
	}
	if result.Reason != ReasonFindingArbitration {
		t.Fatalf("reason = %q, want %q", result.Reason, ReasonFindingArbitration)
	}
	if !slices.Equal(result.ArbitratedFindingIDs, []string{"finding-repeat"}) {
		t.Fatalf("arbitrated identities = %v", result.ArbitratedFindingIDs)
	}
	if len(result.RepeatFindingDispositions) != 1 ||
		result.RepeatFindingDispositions[0].Disposition != RepeatDispositionArbitration {
		t.Fatalf("dispositions = %+v, want one arbitration record", result.RepeatFindingDispositions)
	}
	if result.Verdict == nil || !strings.Contains(result.Verdict.Rationale, "elsewhere.go:42") {
		t.Fatalf("rationale = %+v, want the unverifiable location named", result.Verdict)
	}

	events := readGateEvents(t, run)
	if len(events) != 1 || events[0].Runner["reason"] != ReasonFindingArbitration {
		t.Fatalf("gate events = %+v, want journaled arbitration reason", events)
	}
	if events[0].Runner["arbitratedFindingIdentities"] == nil ||
		events[0].Runner["repeatFindingDispositions"] == nil {
		t.Fatalf("runner annotations = %#v, want the arbitration explanation", events[0].Runner)
	}
}

func TestEvaluatorDispatchesRepassForCorroboratedRepeatedFinding(t *testing.T) {
	run := newTestJournal(t)
	diff := reviewerDiffPointer(t, run, repeatedFindingPatch())
	prior := apiv1.Finding{
		ID: "finding-repeat", LearningSignature: "sig-repeat",
		LearningClassification: apiv1.LearningInstruction,
		Severity:               apiv1.SeverityError,
		Message:                "the constant must be exported",
		Location:               "fixture.go:1",
	}
	episode := writeLearningEpisodePointer(t, run.Dir(), 10, []apiv1.Finding{prior})
	reviewer := &fakeGoober{reviewVerdict: apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{prior},
	}}
	evaluator := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: reviewer}, Journal: run}

	result, err := evaluator.Evaluate(
		context.Background(),
		reviewerEvidenceGate(),
		apiv1.InvocationEnvelope{ContextPointers: []apiv1.ContextPointer{diff, episode}},
		"implement",
		apiv1.ResultEnvelope{},
		"sha256:second",
		false,
	)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Escalated || result.Target != "implement" || result.Attempt != 1 {
		t.Fatalf("result = %+v, want an ordinary repass for a corroborated repeat", result)
	}
	if result.Reason != "" || len(result.ArbitratedFindingIDs) != 0 {
		t.Fatalf("result = %+v, want no arbitration", result)
	}
	if len(result.RepeatFindingDispositions) != 1 ||
		result.RepeatFindingDispositions[0].Disposition != RepeatDispositionDispatch {
		t.Fatalf("dispositions = %+v, want the dispatch explanation", result.RepeatFindingDispositions)
	}
	events := readGateEvents(t, run)
	if len(events) != 1 || events[0].Runner["repeatFindingDispositions"] == nil {
		t.Fatalf("gate events = %+v, want the dispatch explanation journaled", events)
	}
}

func TestArbitrateRepeatedFindingsOnlyStopsUnverifiableRepeats(t *testing.T) {
	run := newTestJournal(t)
	diff := reviewerDiffPointer(t, run, repeatedFindingPatch())
	repeated := apiv1.Finding{
		ID: "finding-repeat", LearningSignature: "sig-repeat",
		LearningClassification: apiv1.LearningInstruction,
		Severity:               apiv1.SeverityError,
		Message:                "the helper must be renamed",
		Location:               "elsewhere.go:42",
	}
	fresh := apiv1.Finding{
		ID: "finding-fresh", LearningSignature: "sig-fresh",
		LearningClassification: apiv1.LearningInstruction,
		Severity:               apiv1.SeverityError,
		Message:                "the new branch is untested",
		Location:               "other.go:7",
	}
	unlocated := repeated
	unlocated.Location = ""
	episode := writeLearningEpisodePointer(t, run.Dir(), 10, []apiv1.Finding{repeated})
	resolve := ArtifactBytesFromRoot(run.Dir())

	tests := []struct {
		name       string
		findings   []apiv1.Finding
		pointers   []apiv1.ContextPointer
		arbitrate  bool
		arbitrated []string
	}{
		{
			name:      "first observation is never arbitrated",
			findings:  []apiv1.Finding{fresh},
			pointers:  []apiv1.ContextPointer{diff, episode},
			arbitrate: false,
		},
		{
			name:       "one live finding keeps the repass",
			findings:   []apiv1.Finding{repeated, fresh},
			pointers:   []apiv1.ContextPointer{diff, episode},
			arbitrate:  false,
			arbitrated: []string{"finding-repeat"},
		},
		{
			name:       "a repeat with no location cannot be verified",
			findings:   []apiv1.Finding{unlocated},
			pointers:   []apiv1.ContextPointer{diff, episode},
			arbitrate:  true,
			arbitrated: []string{"finding-repeat"},
		},
		{
			name:      "no authoritative diff never arbitrates",
			findings:  []apiv1.Finding{repeated},
			pointers:  []apiv1.ContextPointer{episode},
			arbitrate: false,
		},
		{
			name:      "no prior episode never arbitrates",
			findings:  []apiv1.Finding{repeated},
			pointers:  []apiv1.ContextPointer{diff},
			arbitrate: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			verdict, arbitration := arbitrateRepeatedFindings(apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges,
				Findings: tc.findings,
			}, tc.pointers, resolve, "review")
			if arbitration.Arbitrate != tc.arbitrate {
				t.Fatalf("arbitrate = %v, want %v (%+v)", arbitration.Arbitrate, tc.arbitrate, arbitration)
			}
			if !slices.Equal(arbitration.Arbitrated, tc.arbitrated) {
				t.Fatalf("arbitrated = %v, want %v", arbitration.Arbitrated, tc.arbitrated)
			}
			if len(verdict.Findings) != len(tc.findings) {
				t.Fatalf("findings = %+v, want arbitration to remove nothing", verdict.Findings)
			}
			if !tc.arbitrate && strings.Contains(verdict.Rationale, ReasonFindingArbitration) {
				t.Fatalf("rationale = %q, want no arbitration note", verdict.Rationale)
			}
		})
	}
}
