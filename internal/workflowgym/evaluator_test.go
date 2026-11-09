package workflowgym

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestEvaluateTournamentGoldenCases(t *testing.T) {
	registry := evaluationRegistry()
	authoritative := evaluationCandidate(registry, "authoritative", map[string]TournamentStatus{
		"train-a": TournamentPass,
		"train-b": TournamentPass,
		"eval-a":  TournamentPass,
		"eval-b":  TournamentPass,
		"holdout": TournamentPass,
	})
	tests := []struct {
		name       string
		minSamples int
		threshold  float64
		statuses   map[string]TournamentStatus
	}{
		{
			name:       "pass",
			minSamples: 2,
			threshold:  0.25,
			statuses: map[string]TournamentStatus{
				"train-a": TournamentPass, "train-b": TournamentPass,
				"eval-a": TournamentPass, "eval-b": TournamentPass,
				"holdout": TournamentFail,
			},
		},
		{
			name:       "regression",
			minSamples: 2,
			threshold:  0.25,
			statuses: map[string]TournamentStatus{
				"train-a": TournamentFail, "train-b": TournamentFail,
				"eval-a": TournamentPass, "eval-b": TournamentPass,
				"holdout": TournamentPass,
			},
		},
		{
			name:       "noisy",
			minSamples: 2,
			threshold:  0.75,
			statuses: map[string]TournamentStatus{
				"train-a": TournamentFail, "train-b": TournamentPass,
				"eval-a": TournamentPass, "eval-b": TournamentPass,
				"holdout": TournamentPass,
			},
		},
		{
			name:       "insufficient",
			minSamples: 3,
			threshold:  0.25,
			statuses: map[string]TournamentStatus{
				"train-a": TournamentPass, "train-b": TournamentPass,
				"eval-a": TournamentPass, "eval-b": TournamentPass,
				"holdout": TournamentPass,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tournament, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{
				evaluationCandidate(registry, "candidate", test.statuses),
			})
			if err != nil {
				t.Fatal(err)
			}
			report, err := EvaluateTournament(registry, tournament, EvaluationConfig{
				MinSamples:          test.minSamples,
				RegressionThreshold: test.threshold,
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(evaluationGoldenProjection{
				Gate:        report.Gate.Verdict,
				Candidate:   report.Candidates[0].Gate,
				Slices:      report.Candidates[0].Slices,
				Leaderboard: report.Leaderboard,
			}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "evaluation", test.name+".golden.json")
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("evaluation differs from %s:\n%s", path, got)
			}
		})
	}
}

func TestEvaluateTournamentHeldOutNeverAffectsSelection(t *testing.T) {
	registry := evaluationRegistry()
	authoritative := evaluationCandidate(registry, "authoritative", allEvaluationStatuses(TournamentPass))
	candidateStatuses := allEvaluationStatuses(TournamentPass)
	firstTournament, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{
		evaluationCandidate(registry, "candidate", candidateStatuses),
	})
	if err != nil {
		t.Fatal(err)
	}
	candidateStatuses["holdout"] = TournamentFail
	secondTournament, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{
		evaluationCandidate(registry, "candidate", candidateStatuses),
	})
	if err != nil {
		t.Fatal(err)
	}
	config := EvaluationConfig{MinSamples: 2, RegressionThreshold: 0.25}
	first, err := EvaluateTournament(registry, firstTournament, config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EvaluateTournament(registry, secondTournament, config)
	if err != nil {
		t.Fatal(err)
	}
	if first.Gate.Verdict != second.Gate.Verdict ||
		first.Leaderboard[0].PassRate != second.Leaderboard[0].PassRate ||
		first.Leaderboard[0].CostUSD != second.Leaderboard[0].CostUSD ||
		first.Leaderboard[0].DurationMS != second.Leaderboard[0].DurationMS {
		t.Fatalf("held-out result changed selection: first=%+v second=%+v", first, second)
	}
	if first.Leaderboard[0].HeldOut.PassRate == second.Leaderboard[0].HeldOut.PassRate {
		t.Fatal("held-out result was not reported separately")
	}
}

func TestEvaluateTournamentReportsDigestsAndParetoFrontier(t *testing.T) {
	registry := evaluationRegistry()
	authoritative := evaluationCandidate(registry, "authoritative", allEvaluationStatuses(TournamentPass))
	faster := evaluationCandidate(registry, "faster", allEvaluationStatuses(TournamentPass))
	slower := evaluationCandidate(registry, "slower", allEvaluationStatuses(TournamentPass))
	for i := range faster.Results {
		faster.Results[i].CostUSD = 1
		faster.Results[i].DurationMS = 10
		slower.Results[i].CostUSD = 2
		slower.Results[i].DurationMS = 20
	}
	tournament, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{slower, faster})
	if err != nil {
		t.Fatal(err)
	}
	report, err := EvaluateTournament(registry, tournament, EvaluationConfig{MinSamples: 2, RegressionThreshold: 0.25})
	if err != nil {
		t.Fatal(err)
	}
	if report.Digests.Dataset != registry.Digest || report.Digests.Evaluator == "" || report.Digests.Environment == "" {
		t.Fatalf("digests = %+v", report.Digests)
	}
	if len(report.Leaderboard) != 2 || !report.Leaderboard[0].ParetoFrontier || report.Leaderboard[1].ParetoFrontier {
		t.Fatalf("leaderboard = %+v", report.Leaderboard)
	}
	for _, candidate := range report.Candidates {
		if candidate.CandidateDigest == "" {
			t.Fatalf("candidate omitted digest: %+v", candidate)
		}
	}
}

func TestEvaluateTournamentRejectsIncompleteReport(t *testing.T) {
	registry := evaluationRegistry()
	authoritative := evaluationCandidate(registry, "authoritative", allEvaluationStatuses(TournamentPass))
	tournament, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{
		evaluationCandidate(registry, "candidate", allEvaluationStatuses(TournamentPass)),
	})
	if err != nil {
		t.Fatal(err)
	}
	tournament.Results = tournament.Results[:len(tournament.Results)-1]
	if _, err := EvaluateTournament(registry, tournament, EvaluationConfig{MinSamples: 2, RegressionThreshold: 0.25}); err == nil {
		t.Fatal("incomplete tournament report was accepted")
	}
}

func TestEvaluateTournamentCanonicalizesInputOrder(t *testing.T) {
	registry := evaluationRegistry()
	authoritative := evaluationCandidate(registry, "authoritative", allEvaluationStatuses(TournamentPass))
	tournament, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{
		evaluationCandidate(registry, "candidate", allEvaluationStatuses(TournamentPass)),
	})
	if err != nil {
		t.Fatal(err)
	}
	config := EvaluationConfig{MinSamples: 2, RegressionThreshold: 0.25}
	want, err := EvaluateTournament(registry, tournament, config)
	if err != nil {
		t.Fatal(err)
	}
	for left, right := 0, len(tournament.Results)-1; left < right; left, right = left+1, right-1 {
		tournament.Results[left], tournament.Results[right] = tournament.Results[right], tournament.Results[left]
	}
	got, err := EvaluateTournament(registry, tournament, config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("input ordering changed report:\ngot  %+v\nwant %+v", got, want)
	}
}

type evaluationGoldenProjection struct {
	Gate        EvaluationVerdict            `json:"gate"`
	Candidate   EvaluationGate               `json:"candidate"`
	Slices      []EvaluationSlice            `json:"slices"`
	Leaderboard []EvaluationLeaderboardEntry `json:"leaderboard"`
}

func evaluationRegistry() Registry {
	tasks := []apiv1.WorkflowGymTaskManifest{
		evaluationTask("train-a", apiv1.WorkflowGymSplitTrain),
		evaluationTask("train-b", apiv1.WorkflowGymSplitTrain),
		evaluationTask("eval-a", apiv1.WorkflowGymSplitEval),
		evaluationTask("eval-b", apiv1.WorkflowGymSplitEval),
		evaluationTask("holdout", apiv1.WorkflowGymSplitHoldout),
	}
	return Registry{DatasetVersion: "2026.1", Digest: "sha256:evaluation-dataset", Tasks: tasks}
}

func evaluationTask(id string, split apiv1.WorkflowGymSplit) apiv1.WorkflowGymTaskManifest {
	return apiv1.WorkflowGymTaskManifest{
		TaskID: id, Split: split, EvaluatorVersion: "evaluator-1",
		CandidateEnvironment: apiv1.WorkflowGymCandidateEnvironment{
			Image: "example.invalid/workflow-gym@sha256:environment",
			OS:    "linux",
		},
	}
}

func evaluationCandidate(registry Registry, genome string, statuses map[string]TournamentStatus) TournamentCandidateManifest {
	results := make([]TournamentCandidateResult, 0, len(registry.Tasks))
	for _, task := range registry.Tasks {
		status := statuses[task.TaskID]
		result := TournamentCandidateResult{
			TaskID: task.TaskID, EvaluatorVersion: task.EvaluatorVersion, Status: status,
			CostUSD: 1, DurationMS: 10,
		}
		if status == TournamentFail {
			result.FailureClass = "evaluation"
		}
		results = append(results, result)
	}
	return TournamentCandidateManifest{
		APIVersion: apiv1.WorkflowGymTaskAPIVersion, Kind: TournamentCandidateKind,
		DatasetDigest: registry.Digest,
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

func allEvaluationStatuses(status TournamentStatus) map[string]TournamentStatus {
	return map[string]TournamentStatus{
		"train-a": status,
		"train-b": status,
		"eval-a":  status,
		"eval-b":  status,
		"holdout": status,
	}
}

func TestRankEvaluationLeaderboardIgnoresHeldOutMetrics(t *testing.T) {
	left := EvaluationLeaderboardEntry{CandidateDigest: "left", PassRate: 1, CostUSD: 1, DurationMS: 1, HeldOut: &EvaluationHeldOutMetrics{PassRate: 0}}
	right := EvaluationLeaderboardEntry{CandidateDigest: "right", PassRate: 1, CostUSD: 1, DurationMS: 1, HeldOut: &EvaluationHeldOutMetrics{PassRate: 1}}
	entries := []EvaluationLeaderboardEntry{right, left}
	rankEvaluationLeaderboard(entries)
	if entries[0].CandidateDigest != "left" || !reflect.DeepEqual(entries[0].HeldOut, left.HeldOut) {
		t.Fatalf("held-out metrics affected ranking: %+v", entries)
	}
}
