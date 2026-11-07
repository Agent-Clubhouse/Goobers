package workflowgym

import (
	"encoding/json"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestRunTournamentCanonicalizesCandidatesAndTaskAssignments(t *testing.T) {
	registry := tournamentRegistry()
	authoritative := tournamentCandidate("authoritative", []TournamentCandidateResult{
		{TaskID: "task-a", Status: TournamentPass, CostUSD: 1, DurationMS: 100},
		{TaskID: "task-b", Status: TournamentPass, CostUSD: 2, DurationMS: 200},
	})
	first := tournamentCandidate("z-candidate", []TournamentCandidateResult{
		{TaskID: "task-b", Status: TournamentPass, CostUSD: 3, DurationMS: 300},
		{TaskID: "task-a", Status: TournamentFail, FailureClass: "evaluation", CostUSD: 4, DurationMS: 400},
	})
	second := tournamentCandidate("a-candidate", []TournamentCandidateResult{
		{TaskID: "task-a", Status: TournamentPass, CostUSD: 5, DurationMS: 500},
		{TaskID: "task-b", Status: TournamentPass, CostUSD: 6, DurationMS: 600},
	})

	got, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{first, second})
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{second, first})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, reversed) {
		t.Fatalf("candidate ordering changed report:\n%+v\n%+v", got, reversed)
	}
	for i := 0; i < len(got.Results); i += 2 {
		if got.Results[i].TaskID != "task-a" || got.Results[i+1].TaskID != "task-b" {
			t.Fatalf("candidate task assignment = %q, %q", got.Results[i].TaskID, got.Results[i+1].TaskID)
		}
	}
	firstJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("canonical reports encoded differently")
	}
}

func TestRunTournamentFailsClosedForMissingAndFailedResults(t *testing.T) {
	registry := tournamentRegistry()
	authoritative := tournamentCandidate("authoritative", []TournamentCandidateResult{
		{TaskID: "task-a", Status: TournamentPass},
		{TaskID: "task-b", Status: TournamentPass},
	})
	candidate := tournamentCandidate("candidate", []TournamentCandidateResult{
		{TaskID: "task-a", Status: TournamentFail, FailureClass: "tool"},
	})

	report, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{candidate})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(report.Results))
	}
	for _, result := range report.Results {
		if result.CandidateRun.Status != TournamentFail {
			t.Fatalf("task %q status = %q, want fail", result.TaskID, result.CandidateRun.Status)
		}
		if result.Authoritative != authoritative.Identity || result.Candidate != candidate.Identity {
			t.Fatalf("task %q identities = %+v / %+v", result.TaskID, result.Authoritative, result.Candidate)
		}
		if result.EvaluatorVersion == "" {
			t.Fatalf("task %q omitted evaluator version", result.TaskID)
		}
	}
	if report.Results[1].CandidateRun.FailureClass != "missing-result" {
		t.Fatalf("missing result class = %q", report.Results[1].CandidateRun.FailureClass)
	}
	all := report.Aggregates[0]
	if all.After.Passed != 0 || all.After.Failed != 2 ||
		all.After.FailureClasses["tool"] != 1 || all.After.FailureClasses["missing-result"] != 1 ||
		all.Verdict != rollup.EfficacyRegressed {
		t.Fatalf("aggregate = %+v", all)
	}
}

func TestRunTournamentReportsPerSliceDeltas(t *testing.T) {
	registry := tournamentRegistry()
	authoritative := tournamentCandidate("authoritative", []TournamentCandidateResult{
		{TaskID: "task-a", Status: TournamentFail, FailureClass: "baseline", CostUSD: 2, DurationMS: 20},
		{TaskID: "task-b", Status: TournamentPass, CostUSD: 4, DurationMS: 40},
	})
	candidate := tournamentCandidate("candidate", []TournamentCandidateResult{
		{TaskID: "task-a", Status: TournamentPass, CostUSD: 1, DurationMS: 10},
		{TaskID: "task-b", Status: TournamentPass, CostUSD: 3, DurationMS: 30},
	})

	report, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{candidate})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Aggregates) != 3 {
		t.Fatalf("aggregates = %d, want overall plus two splits", len(report.Aggregates))
	}
	all := report.Aggregates[0]
	if all.Split != "all" || all.Delta.PassRate != .5 || all.Delta.CostUSD != -2 ||
		all.Delta.DurationMS != -20 || all.Verdict != rollup.EfficacyHelped {
		t.Fatalf("overall aggregate = %+v", all)
	}
	if report.Aggregates[1].Split != "train" || report.Aggregates[2].Split != "eval" {
		t.Fatalf("slice order = %q, %q", report.Aggregates[1].Split, report.Aggregates[2].Split)
	}
}

func TestRunTournamentRejectsOutOfDatasetResult(t *testing.T) {
	authoritative := tournamentCandidate("authoritative", []TournamentCandidateResult{
		{TaskID: "task-a", Status: TournamentPass},
	})
	candidate := tournamentCandidate("candidate", []TournamentCandidateResult{
		{TaskID: "other", Status: TournamentPass},
	})
	if _, err := RunTournament(tournamentRegistry(), authoritative, []TournamentCandidateManifest{candidate}); err == nil {
		t.Fatal("expected out-of-dataset result error")
	}
}

func tournamentRegistry() Registry {
	return Registry{
		DatasetVersion: "2026.1",
		Digest:         "sha256:dataset",
		Tasks: []apiv1.WorkflowGymTaskManifest{
			{TaskID: "task-a", Split: apiv1.WorkflowGymSplitTrain, EvaluatorVersion: "eval-1"},
			{TaskID: "task-b", Split: apiv1.WorkflowGymSplitEval, EvaluatorVersion: "eval-2"},
		},
	}
}

func tournamentCandidate(genome string, results []TournamentCandidateResult) TournamentCandidateManifest {
	return TournamentCandidateManifest{
		APIVersion: apiv1.WorkflowGymTaskAPIVersion,
		Kind:       TournamentCandidateKind,
		Identity: TournamentCandidateIdentity{
			EffectiveVersion: rollup.EffectiveVersion{
				WorkflowDigest: "sha256:workflow-" + genome,
				GooberDigest:   "sha256:goober-" + genome,
				Model:          "model-" + genome,
				HarnessVersion: "harness-1",
			},
			GenomeDigest: "sha256:" + genome,
		},
		Results: results,
	}
}
