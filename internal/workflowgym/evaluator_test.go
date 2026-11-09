package workflowgym

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestEvaluateTournamentGoldenCases(t *testing.T) {
	registry := evaluationRegistry()
	allPass := allEvaluationStatuses(registry, TournamentPass)
	authoritative := evaluationCandidate(registry, "authoritative", allPass)
	tests := []struct {
		name       string
		minSamples int
		threshold  float64
		statuses   map[string]TournamentStatus
	}{
		{
			name:       "pass",
			minSamples: 10,
			threshold:  0.25,
			statuses:   withEvaluationStatus(allPass, apiv1.WorkflowGymSplitHoldout, 2, TournamentFail),
		},
		{
			name:       "regression",
			minSamples: 10,
			threshold:  0.25,
			statuses:   withEvaluationStatus(allPass, apiv1.WorkflowGymSplitTrain, 10, TournamentFail),
		},
		{
			name:       "noisy",
			minSamples: 10,
			threshold:  0.25,
			statuses:   withEvaluationStatus(allPass, apiv1.WorkflowGymSplitTrain, 2, TournamentFail),
		},
		{
			name:       "insufficient",
			minSamples: 25,
			threshold:  0.25,
			statuses:   allPass,
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
			if string(report.Gate.Verdict) != map[string]string{
				"pass": "pass", "regression": "fail", "noisy": "inconclusive", "insufficient": "inconclusive",
			}[test.name] {
				t.Fatalf("verdict = %s, reasons = %v", report.Gate.Verdict, report.Gate.Reasons)
			}
			got, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "evaluation", test.name+".golden.json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("evaluation differs from %s (rerun with UPDATE_GOLDEN=1):\n%s", path, got)
			}
		})
	}
}

func TestEvaluateTournamentHeldOutNeverAffectsSelection(t *testing.T) {
	registry := evaluationRegistry()
	authoritative := evaluationCandidate(registry, "authoritative", allEvaluationStatuses(registry, TournamentPass))
	candidateStatuses := allEvaluationStatuses(registry, TournamentPass)
	firstTournament, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{
		evaluationCandidate(registry, "candidate", candidateStatuses),
	})
	if err != nil {
		t.Fatal(err)
	}
	candidateStatuses = withEvaluationStatus(candidateStatuses, apiv1.WorkflowGymSplitHoldout, 1, TournamentFail)
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
		first.Leaderboard[0].ConfidenceInterval != second.Leaderboard[0].ConfidenceInterval ||
		first.Leaderboard[0].CostUSD != second.Leaderboard[0].CostUSD ||
		first.Leaderboard[0].DurationMS != second.Leaderboard[0].DurationMS {
		t.Fatalf("held-out result changed selection: first=%+v second=%+v", first, second)
	}
	if first.Leaderboard[0].HeldOut.PassRate == second.Leaderboard[0].HeldOut.PassRate {
		t.Fatal("held-out result was not reported separately")
	}
}

func TestEvaluateTournamentZeroChangeWithFewSamplesIsInconclusive(t *testing.T) {
	registry := evaluationRegistryWith(2, 2, 0)
	statuses := allEvaluationStatuses(registry, TournamentPass)
	report := evaluateEvaluationStatuses(t, registry, statuses, statuses, EvaluationConfig{MinSamples: 1, RegressionThreshold: 0.25})
	if report.Gate.Verdict != EvaluationInconclusive {
		t.Fatalf("unchanged slice with two samples passed: %+v", report.Gate)
	}
	for _, slice := range report.Candidates[0].Slices {
		if slice.Delta != 0 || slice.ConfidenceInterval.Lower >= 0 || slice.ConfidenceInterval.Upper <= 0 {
			t.Fatalf("unchanged slice interval collapsed: %+v", slice)
		}
	}
}

func TestEvaluateTournamentImprovementNeverWorseThanUnchanged(t *testing.T) {
	for _, registry := range []Registry{evaluationRegistryWith(4, 4, 0), evaluationRegistry()} {
		baseline := withEvaluationStatus(allEvaluationStatuses(registry, TournamentPass), apiv1.WorkflowGymSplitTrain, 1, TournamentFail)
		config := EvaluationConfig{MinSamples: 1, RegressionThreshold: 0.25}
		unchanged := evaluateEvaluationStatuses(t, registry, baseline, baseline, config)
		improved := evaluateEvaluationStatuses(t, registry, baseline, allEvaluationStatuses(registry, TournamentPass), config)
		if evaluationVerdictSeverity(improved.Gate.Verdict) > evaluationVerdictSeverity(unchanged.Gate.Verdict) {
			t.Fatalf("%d tasks: improvement verdict %s worse than unchanged %s", len(registry.Tasks), improved.Gate.Verdict, unchanged.Gate.Verdict)
		}
		improvedTrain, unchangedTrain := improved.Candidates[0].Slices[0], unchanged.Candidates[0].Slices[0]
		if improvedTrain.Delta <= 0 || improvedTrain.ConfidenceInterval.Lower < unchangedTrain.ConfidenceInterval.Lower {
			t.Fatalf("%d tasks: improvement %+v not bounded above unchanged %+v", len(registry.Tasks), improvedTrain, unchangedTrain)
		}
	}
}

func TestEvaluationSliceVerdictMonotoneInOutcomes(t *testing.T) {
	config := EvaluationConfig{MinSamples: 1, RegressionThreshold: 0.1}
	verdict := func(samples, improvements, regressions int) int {
		counts := evaluationCounts{samples: samples, improvements: improvements, regressions: regressions}
		delta, interval := counts.pairedDelta()
		result, _ := evaluationSliceVerdict(counts, delta, interval, config)
		return evaluationVerdictSeverity(result)
	}
	for samples := 1; samples <= 60; samples++ {
		for improvements := 0; improvements <= samples; improvements++ {
			for regressions := 0; improvements+regressions <= samples; regressions++ {
				current := verdict(samples, improvements, regressions)
				if improvements+regressions < samples && verdict(samples, improvements+1, regressions) > current {
					t.Fatalf("n=%d c=%d b=%d: extra improvement worsened verdict", samples, improvements, regressions)
				}
				if regressions > 0 && verdict(samples, improvements, regressions-1) > current {
					t.Fatalf("n=%d c=%d b=%d: removing a regression worsened verdict", samples, improvements, regressions)
				}
			}
		}
	}
}

func TestEvaluateTournamentRegressionAtExactThresholdFails(t *testing.T) {
	registry := evaluationRegistry()
	allPass := allEvaluationStatuses(registry, TournamentPass)
	config := EvaluationConfig{MinSamples: 10, RegressionThreshold: 0.25}
	atThreshold := evaluateEvaluationStatuses(t, registry, allPass, withEvaluationStatus(allPass, apiv1.WorkflowGymSplitTrain, 5, TournamentFail), config)
	if train := atThreshold.Candidates[0].Slices[0]; train.Delta != -0.25 || train.Verdict != EvaluationFail || atThreshold.Gate.Verdict != EvaluationFail {
		t.Fatalf("delta == -threshold did not fail: %+v", train)
	}
	belowThreshold := evaluateEvaluationStatuses(t, registry, allPass, withEvaluationStatus(allPass, apiv1.WorkflowGymSplitTrain, 4, TournamentFail), config)
	if train := belowThreshold.Candidates[0].Slices[0]; train.Verdict == EvaluationFail {
		t.Fatalf("delta above -threshold failed: %+v", train)
	}
}

func TestEvaluateTournamentHoldoutOnlyDatasetIsInconclusive(t *testing.T) {
	registry := evaluationRegistryWith(0, 0, 20)
	allPass := allEvaluationStatuses(registry, TournamentPass)
	report := evaluateEvaluationStatuses(t, registry, allPass, allPass, EvaluationConfig{MinSamples: 1, RegressionThreshold: 0.25})
	if report.Gate.Verdict != EvaluationInconclusive || len(report.Gate.Reasons) != 1 ||
		!strings.HasSuffix(report.Gate.Reasons[0], "no train or eval evidence") {
		t.Fatalf("holdout-only gate = %+v", report.Gate)
	}
	if entry := report.Leaderboard[0]; entry.Samples != 0 || entry.ParetoFrontier || entry.HeldOut == nil || entry.HeldOut.Samples != 20 {
		t.Fatalf("holdout-only leaderboard = %+v", entry)
	}
}

func TestEvaluateTournamentAggregatesMultipleCandidates(t *testing.T) {
	registry := evaluationRegistry()
	allPass := allEvaluationStatuses(registry, TournamentPass)
	authoritative := evaluationCandidate(registry, "authoritative", allPass)
	good := evaluationCandidate(registry, "good", allPass)
	bad := evaluationCandidate(registry, "bad", withEvaluationStatus(allPass, apiv1.WorkflowGymSplitTrain, 10, TournamentFail))
	for i := range bad.Results {
		bad.Results[i].CostUSD = 0
	}
	tournament, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{good, bad})
	if err != nil {
		t.Fatal(err)
	}
	report, err := EvaluateTournament(registry, tournament, EvaluationConfig{MinSamples: 10, RegressionThreshold: 0.25})
	if err != nil {
		t.Fatal(err)
	}
	goodDigest, badDigest := digestJSON(good.Identity), digestJSON(bad.Identity)
	if report.Gate.Verdict != EvaluationFail || len(report.Gate.Reasons) != 1 ||
		!strings.HasPrefix(report.Gate.Reasons[0], badDigest+": train: pass-rate delta") {
		t.Fatalf("aggregate gate = %+v", report.Gate)
	}
	if len(report.Leaderboard) != 2 ||
		report.Leaderboard[0].CandidateDigest != goodDigest || report.Leaderboard[0].Verdict != EvaluationPass || !report.Leaderboard[0].ParetoFrontier ||
		report.Leaderboard[1].CandidateDigest != badDigest || report.Leaderboard[1].Verdict != EvaluationFail || report.Leaderboard[1].ParetoFrontier {
		t.Fatalf("cheaper failing candidate outranked or joined frontier: %+v", report.Leaderboard)
	}
	if report.Baseline.AuthoritativeDigest != digestJSON(authoritative.Identity) || report.Baseline.Samples != 40 || report.Baseline.PassRate != 1 {
		t.Fatalf("baseline = %+v", report.Baseline)
	}
}

func TestEvaluateTournamentNeverMeasuresMissingResults(t *testing.T) {
	registry := evaluationRegistry()
	allPass := allEvaluationStatuses(registry, TournamentPass)
	authoritative := evaluationCandidate(registry, "authoritative", withEvaluationStatus(allPass, apiv1.WorkflowGymSplitTrain, 1, TournamentFail))
	tournament, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{
		evaluationCandidate(registry, "candidate", allPass),
	})
	if err != nil {
		t.Fatal(err)
	}
	config := EvaluationConfig{MinSamples: 10, RegressionThreshold: 0.25}
	if report, err := EvaluateTournament(registry, tournament, config); err != nil || report.Gate.Verdict != EvaluationPass {
		t.Fatalf("unedited tournament: verdict=%v err=%v", report.Gate.Verdict, err)
	}
	missing := map[string]any{"status": "fail", "failureClass": tournamentMissingResultFailureClass}

	path := writeEditedTournament(t, tournament, "train-00", "candidateRun", missing)
	edited, err := LoadTournamentReport(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := EvaluateTournament(registry, edited, config)
	if err != nil {
		t.Fatal(err)
	}
	train := report.Candidates[0].Slices[0]
	if report.Gate.Verdict != EvaluationInconclusive || train.Samples != 19 || train.MissingResults != 1 ||
		report.Leaderboard[0].ParetoFrontier || report.Leaderboard[0].Samples != 39 {
		t.Fatalf("missing candidate evidence was measured: gate=%+v slice=%+v leaderboard=%+v", report.Gate, train, report.Leaderboard)
	}

	path = writeEditedTournament(t, tournament, "eval-00", "authoritativeRun", missing)
	edited, err = LoadTournamentReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateTournament(registry, edited, config); err == nil || !strings.Contains(err.Error(), "authoritative result is missing") {
		t.Fatalf("missing authoritative evidence accepted: %v", err)
	}
}

func evaluateEvaluationStatuses(t *testing.T, registry Registry, authoritative, candidate map[string]TournamentStatus, config EvaluationConfig) EvaluationReport {
	t.Helper()
	tournament, err := RunTournament(registry, evaluationCandidate(registry, "authoritative", authoritative), []TournamentCandidateManifest{
		evaluationCandidate(registry, "candidate", candidate),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := EvaluateTournament(registry, tournament, config)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// writeEditedTournament writes tournament JSON with one run's fields overwritten,
// as a hand edit of the report file would.
func writeEditedTournament(t *testing.T, tournament TournamentReport, taskID, run string, fields map[string]any) string {
	t.Helper()
	data, err := json.Marshal(tournament)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	edited := false
	for _, raw := range document["results"].([]any) {
		pair := raw.(map[string]any)
		if pair["taskId"] != taskID {
			continue
		}
		for key, value := range fields {
			pair[run].(map[string]any)[key] = value
		}
		edited = true
	}
	if !edited {
		t.Fatalf("task %q not found", taskID)
	}
	data, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tournament.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEvaluateTournamentReportsDigestsAndParetoFrontier(t *testing.T) {
	registry := evaluationRegistry()
	authoritative := evaluationCandidate(registry, "authoritative", allEvaluationStatuses(registry, TournamentPass))
	faster := evaluationCandidate(registry, "faster", allEvaluationStatuses(registry, TournamentPass))
	slower := evaluationCandidate(registry, "slower", allEvaluationStatuses(registry, TournamentPass))
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
	authoritative := evaluationCandidate(registry, "authoritative", allEvaluationStatuses(registry, TournamentPass))
	tournament, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{
		evaluationCandidate(registry, "candidate", allEvaluationStatuses(registry, TournamentPass)),
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
	authoritative := evaluationCandidate(registry, "authoritative", allEvaluationStatuses(registry, TournamentPass))
	tournament, err := RunTournament(registry, authoritative, []TournamentCandidateManifest{
		evaluationCandidate(registry, "candidate", allEvaluationStatuses(registry, TournamentPass)),
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

// evaluationRegistry has enough selection tasks per split for an unchanged
// candidate to bound regression below a 0.25 threshold.
func evaluationRegistry() Registry {
	return evaluationRegistryWith(20, 20, 2)
}

func evaluationRegistryWith(train, eval, holdout int) Registry {
	var tasks []apiv1.WorkflowGymTaskManifest
	for _, split := range []struct {
		split apiv1.WorkflowGymSplit
		count int
	}{
		{apiv1.WorkflowGymSplitTrain, train},
		{apiv1.WorkflowGymSplitEval, eval},
		{apiv1.WorkflowGymSplitHoldout, holdout},
	} {
		for i := 0; i < split.count; i++ {
			tasks = append(tasks, evaluationTask(fmt.Sprintf("%s-%02d", split.split, i), split.split))
		}
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

func allEvaluationStatuses(registry Registry, status TournamentStatus) map[string]TournamentStatus {
	statuses := make(map[string]TournamentStatus, len(registry.Tasks))
	for _, task := range registry.Tasks {
		statuses[task.TaskID] = status
	}
	return statuses
}

// withEvaluationStatus returns statuses with the first count tasks of split set to status.
func withEvaluationStatus(statuses map[string]TournamentStatus, split apiv1.WorkflowGymSplit, count int, status TournamentStatus) map[string]TournamentStatus {
	result := make(map[string]TournamentStatus, len(statuses))
	for taskID, value := range statuses {
		result[taskID] = value
	}
	for i := 0; i < count; i++ {
		result[fmt.Sprintf("%s-%02d", split, i)] = status
	}
	return result
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

func TestRankEvaluationLeaderboardPlacesNonPassingLast(t *testing.T) {
	entries := []EvaluationLeaderboardEntry{
		{CandidateDigest: "failing", Verdict: EvaluationFail, PassRate: 1, CostUSD: 0, DurationMS: 0},
		{CandidateDigest: "inconclusive", Verdict: EvaluationInconclusive, PassRate: 1, CostUSD: 0, DurationMS: 0},
		{CandidateDigest: "passing", Verdict: EvaluationPass, PassRate: 0.5, CostUSD: 5, DurationMS: 5},
	}
	rankEvaluationLeaderboard(entries)
	for i, want := range []string{"passing", "inconclusive", "failing"} {
		if entries[i].CandidateDigest != want || entries[i].Rank != i+1 || entries[i].ParetoFrontier != (want == "passing") {
			t.Fatalf("leaderboard = %+v", entries)
		}
	}
}
