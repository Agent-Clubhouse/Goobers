package workflowcluster

import (
	"slices"
	"testing"
)

func candidateFixture() (Model, []CandidateEvidence) {
	memberIDs := []string{"proposal-a", "proposal-b", "proposal-c", "proposal-d", "proposal-e"}
	model := Model{
		ID: "sha256:0123456789abcdef", FeatureVersion: StructuralFeatureVersion,
		Options: Options{StabilityThreshold: 0.6},
		Clusters: []Cluster{{
			ID: "c0", Stable: true, Stability: 0.9,
			Medoid:         Point{ID: "proposal-c"},
			Members:        memberIDs,
			Distinguishing: []Attribution{{Feature: "nodes.agentic", Effect: 0.8}},
		}},
	}
	var evidence []CandidateEvidence
	for i, id := range memberIDs {
		evidence = append(evidence, CandidateEvidence{
			ProposalID: id, ProposalDigest: "sha256:" + id, RunID: "run-" + id,
			ParentRunID: []string{"parent-1", "parent-2", "parent-3"}[i%3],
			Repository:  []string{"owner/one", "owner/two"}[i%2], TaskCohort: "fix",
			Succeeded:  i != 4,
			Parameters: []TemplateParameter{{Name: "repository"}},
		})
	}
	for i, succeeded := range []bool{true, true, false} {
		id := "alternative-" + string(rune('a'+i))
		evidence = append(evidence, CandidateEvidence{
			ProposalID: id, ProposalDigest: "sha256:" + id, RunID: "run-" + id,
			ParentRunID: "other-parent", Repository: "owner/other", TaskCohort: "fix", Succeeded: succeeded,
		})
	}
	return model, evidence
}

func TestProposeTemplatesEmitsTraceableReviewRequest(t *testing.T) {
	model, evidence := candidateFixture()
	report, err := ProposeTemplates(model, evidence, CandidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Candidates) != 1 || len(report.Suppressed) != 0 {
		t.Fatalf("report = %+v", report)
	}
	candidate := report.Candidates[0]
	if candidate.Proposal.Kind != ProposalKindConfigChange || candidate.Proposal.Target != ProposalTargetCatalog ||
		!candidate.Proposal.ReviewRequired || candidate.Proposal.AutomaticRegistration {
		t.Fatalf("proposal bypasses review: %+v", candidate.Proposal)
	}
	if candidate.Proposal.SourceProposalID != "proposal-c" || candidate.Proposal.SourceProposalDigest != "sha256:proposal-c" {
		t.Fatalf("medoid source = %+v", candidate.Proposal)
	}
	if candidate.Outcomes.CandidateRate != 0.8 || candidate.Outcomes.AlternativeRate != 2.0/3.0 {
		t.Fatalf("outcomes = %+v", candidate.Outcomes)
	}
	gotIDs := make([]string, len(candidate.Evidence))
	for i, item := range candidate.Evidence {
		gotIDs[i] = item.ProposalID
		if item.RunID == "" || item.ParentRunID == "" || item.Repository == "" || item.ProposalDigest == "" {
			t.Fatalf("incomplete trace evidence: %+v", item)
		}
	}
	if want := []string{"proposal-a", "proposal-b", "proposal-c", "proposal-d", "proposal-e"}; !slices.Equal(gotIDs, want) {
		t.Fatalf("evidence IDs = %v, want %v", gotIDs, want)
	}
}

func TestProposeTemplatesSuppressesCandidatesBelowEvidenceBar(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Model, []CandidateEvidence)
		opts   CandidateOptions
		reason string
	}{
		{
			name: "low support", opts: CandidateOptions{MinParentRuns: 4},
			mutate: func(*Model, []CandidateEvidence) {},
			reason: SuppressedLowSupport,
		},
		{
			name: "low stability",
			mutate: func(m *Model, _ []CandidateEvidence) {
				m.Clusters[0].Stable = false
				m.Clusters[0].Stability = 0.4
			},
			reason: SuppressedUnstable,
		},
		{
			name: "worse outcomes",
			mutate: func(_ *Model, evidence []CandidateEvidence) {
				for i := range 4 {
					evidence[i].Succeeded = false
				}
			},
			reason: SuppressedWorseOutcomes,
		},
		{
			name: "safety finding",
			mutate: func(_ *Model, evidence []CandidateEvidence) {
				evidence[0].SafetyFindings = []string{"unsafe capability"}
			},
			reason: SuppressedFindings,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model, evidence := candidateFixture()
			test.mutate(&model, evidence)
			report, err := ProposeTemplates(model, evidence, test.opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Candidates) != 0 || len(report.Suppressed) != 1 || report.Suppressed[0].Reason != test.reason {
				t.Fatalf("report = %+v, want suppression %q", report, test.reason)
			}
		})
	}
}

func TestProposeTemplatesRequiresStructuralModelAndCompleteEvidence(t *testing.T) {
	model, evidence := candidateFixture()
	model.FeatureVersion = BehaviorFeatureVersion
	if _, err := ProposeTemplates(model, evidence, CandidateOptions{}); err == nil {
		t.Fatal("behavior model was accepted")
	}
	model.FeatureVersion = StructuralFeatureVersion
	evidence[0].RunID = ""
	if _, err := ProposeTemplates(model, evidence, CandidateOptions{}); err == nil {
		t.Fatal("incomplete evidence was accepted")
	}
}
