package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/learning"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestReconcileLearningFindingsResolvesSuppressesAndReopensByEvidence(t *testing.T) {
	root := t.TempDir()
	a := apiv1.Finding{
		ID: "finding-a", LearningSignature: "sig-a",
		LearningClassification: apiv1.LearningInstruction,
		EvidenceDigest:         "sha256:old-a",
		Severity:               apiv1.SeverityError,
		Message:                "fix A",
	}
	b := apiv1.Finding{
		ID: "finding-b", LearningSignature: "sig-b",
		LearningClassification: apiv1.LearningValidation,
		EvidenceDigest:         "sha256:old-b",
		Severity:               apiv1.SeverityError,
		Message:                "fix B",
	}
	first := writeLearningEpisodePointer(t, root, 10, []apiv1.Finding{a, b})

	verdict, resolution := reconcileLearningFindings(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{b},
	}, []apiv1.ContextPointer{first}, ArtifactBytesFromRoot(root), "review", "sha256:diff-2")
	if verdict.Decision != apiv1.VerdictNeedsChanges ||
		!slices.Equal(resolution.Resolved, []string{"finding-a"}) ||
		len(verdict.Findings) != 1 || verdict.Findings[0].ID != "finding-b" {
		t.Fatalf("partial resolution = verdict %+v, resolution %+v", verdict, resolution)
	}

	second := writeLearningEpisodePointer(t, root, 20, []apiv1.Finding{b})
	verdict, resolution = reconcileLearningFindings(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{a},
	}, []apiv1.ContextPointer{first, second}, ArtifactBytesFromRoot(root), "review", "sha256:diff-3")
	if verdict.Decision != apiv1.VerdictPass || !resolution.AllSuppressed ||
		!slices.Equal(resolution.Suppressed, []string{"finding-a"}) ||
		len(verdict.Findings) != 0 {
		t.Fatalf("old-evidence reopening was not suppressed: verdict %+v, resolution %+v", verdict, resolution)
	}

	a.EvidenceDigest = ""
	verdict, resolution = reconcileLearningFindings(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{a},
	}, []apiv1.ContextPointer{first, second}, ArtifactBytesFromRoot(root), "review", "sha256:diff-new")
	if verdict.Decision != apiv1.VerdictNeedsChanges ||
		!slices.Equal(resolution.Reopened, []string{"finding-a"}) ||
		len(verdict.Findings) != 1 ||
		verdict.Findings[0].EvidenceDigest != "sha256:diff-new" {
		t.Fatalf("changed diff fallback did not reopen finding: verdict %+v, resolution %+v", verdict, resolution)
	}

	a.EvidenceDigest = "sha256:new-a"
	verdict, resolution = reconcileLearningFindings(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{a},
	}, []apiv1.ContextPointer{first, second}, ArtifactBytesFromRoot(root), "review", "sha256:diff-4")
	if verdict.Decision != apiv1.VerdictNeedsChanges ||
		!slices.Equal(resolution.Reopened, []string{"finding-a"}) ||
		len(verdict.Findings) != 1 {
		t.Fatalf("new-evidence reopening was not retained: verdict %+v, resolution %+v", verdict, resolution)
	}
}

func TestSynthesizedVerdictsDoNotResolveInjectedFindings(t *testing.T) {
	for _, tt := range []struct {
		name      string
		emptyDiff bool
		digest    string
	}{
		{name: "empty diff", emptyDiff: true, digest: "sha256:empty"},
		{name: "duplicate diff", digest: "sha256:duplicate"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run := newTestJournal(t)
			finding := apiv1.Finding{
				ID: "finding-active", LearningSignature: "signature-active",
				LearningClassification: apiv1.LearningCodeDefect,
				EvidenceDigest:         "sha256:old",
				Severity:               apiv1.SeverityError,
				Message:                "active defect",
			}
			pointer := writeLearningEpisodePointer(t, run.Dir(), 1, []apiv1.Finding{finding})
			g := apiv1.Gate{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					string(apiv1.VerdictPass):         wf.TerminalComplete,
					string(apiv1.VerdictNeedsChanges): "implement",
					string(apiv1.VerdictFail):         wf.TargetAbort,
				},
			}
			evaluator := &Evaluator{
				Reviewer:       &ReviewerEvaluator{Goober: &fakeGoober{}},
				Journal:        run,
				MaxRepasses:    3,
				LastDiffDigest: map[string]string{},
			}
			if !tt.emptyDiff {
				evaluator.LastDiffDigest[g.Name] = tt.digest
			}
			result, err := evaluator.Evaluate(
				context.Background(), g,
				apiv1.InvocationEnvelope{ContextPointers: []apiv1.ContextPointer{pointer}},
				"implement", apiv1.ResultEnvelope{}, tt.digest, tt.emptyDiff,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.ResolvedFindingIDs) != 0 {
				t.Fatalf("synthesized result resolved findings: %+v", result)
			}
			events := readGateEvents(t, run)
			if len(events) != 1 {
				t.Fatalf("gate events = %d, want 1", len(events))
			}
			if _, ok := events[0].Runner["resolvedFindingIdentities"]; ok {
				t.Fatalf("synthesized event recorded false resolution: %+v", events[0].Runner)
			}
		})
	}
}

func writeLearningEpisodePointer(t *testing.T, root string, seq uint64, findings []apiv1.Finding) apiv1.ContextPointer {
	t.Helper()
	episode := learning.Episode{
		Schema: learning.EpisodeSchema, SourceRunID: "run", SourceSeq: seq,
		Workflow: "implementation", Gate: "review", Findings: findings,
		Outcome: learning.OutcomeUnresolved,
	}
	episode.ID = learning.EpisodeID(episode)
	data, err := json.Marshal(episode)
	if err != nil {
		t.Fatal(err)
	}
	pointer, err := apiv1.WriteArtifact(root, fmt.Sprintf("artifacts/episode-%d.json", seq), data, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	return apiv1.ContextPointer{Name: "learning.episode", Artifact: &pointer}
}
