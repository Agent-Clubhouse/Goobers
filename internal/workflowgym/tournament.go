package workflowgym

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/strictyaml"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const (
	// TournamentCandidateKind is the kind of candidate result manifests.
	TournamentCandidateKind = "WorkflowGymCandidate"
	// TournamentReportKind is the kind of paired tournament reports.
	TournamentReportKind = "WorkflowGymTournament"
)

// TournamentStatus is one candidate's evaluator outcome for one task.
type TournamentStatus string

const (
	// TournamentPass means the candidate satisfied the task evaluator.
	TournamentPass TournamentStatus = "pass"
	// TournamentFail means the candidate did not satisfy the task evaluator.
	TournamentFail TournamentStatus = "fail"
)

// TournamentCandidateIdentity pins every behavior axis compared by a tournament.
type TournamentCandidateIdentity struct {
	EffectiveVersion rollup.EffectiveVersion `json:"effectiveVersion" yaml:"effectiveVersion"`
	GenomeDigest     string                  `json:"genomeDigest" yaml:"genomeDigest"`
}

// TournamentCandidateManifest records one candidate's results over a task roster.
type TournamentCandidateManifest struct {
	APIVersion string                      `json:"apiVersion" yaml:"apiVersion"`
	Kind       string                      `json:"kind" yaml:"kind"`
	Identity   TournamentCandidateIdentity `json:"identity" yaml:"identity"`
	Results    []TournamentCandidateResult `json:"results" yaml:"results"`
}

// TournamentCandidateResult is one candidate's measured task outcome.
type TournamentCandidateResult struct {
	TaskID       string           `json:"taskId" yaml:"taskId"`
	Status       TournamentStatus `json:"status" yaml:"status"`
	CostUSD      float64          `json:"costUsd" yaml:"costUsd"`
	DurationMS   int64            `json:"durationMs" yaml:"durationMs"`
	FailureClass string           `json:"failureClass,omitempty" yaml:"failureClass,omitempty"`
}

// TournamentReport is the canonical paired comparison for a dataset.
type TournamentReport struct {
	APIVersion     string                 `json:"apiVersion"`
	Kind           string                 `json:"kind"`
	DatasetVersion string                 `json:"datasetVersion"`
	DatasetDigest  string                 `json:"datasetDigest"`
	Results        []TournamentPairResult `json:"results"`
	Aggregates     []TournamentAggregate  `json:"aggregates"`
}

// TournamentPairResult pairs authoritative and candidate outcomes for one task.
type TournamentPairResult struct {
	TaskID           string                      `json:"taskId"`
	Split            apiv1.WorkflowGymSplit      `json:"split"`
	EvaluatorVersion string                      `json:"evaluatorVersion"`
	Authoritative    TournamentCandidateIdentity `json:"authoritative"`
	Candidate        TournamentCandidateIdentity `json:"candidate"`
	AuthoritativeRun TournamentCandidateResult   `json:"authoritativeRun"`
	CandidateRun     TournamentCandidateResult   `json:"candidateRun"`
}

// TournamentAggregate summarizes one candidate pair over all tasks or one split.
type TournamentAggregate struct {
	Split         string                      `json:"split"`
	Authoritative TournamentCandidateIdentity `json:"authoritative"`
	Candidate     TournamentCandidateIdentity `json:"candidate"`
	TaskCount     int                         `json:"taskCount"`
	Before        TournamentStats             `json:"before"`
	After         TournamentStats             `json:"after"`
	Delta         TournamentDelta             `json:"delta"`
	Verdict       rollup.EfficacyVerdict      `json:"verdict"`
}

// TournamentStats summarizes pass/fail, cost, duration, and failure classes.
type TournamentStats struct {
	Passed         int            `json:"passed"`
	Failed         int            `json:"failed"`
	PassRate       float64        `json:"passRate"`
	CostUSD        float64        `json:"costUsd"`
	DurationMS     int64          `json:"durationMs"`
	FailureClasses map[string]int `json:"failureClasses"`
}

// TournamentDelta is candidate minus authoritative for each aggregate measure.
type TournamentDelta struct {
	PassRate   float64 `json:"passRate"`
	CostUSD    float64 `json:"costUsd"`
	DurationMS int64   `json:"durationMs"`
}

// LoadTournamentCandidate reads and validates one candidate result manifest.
func LoadTournamentCandidate(path string) (TournamentCandidateManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return TournamentCandidateManifest{}, fmt.Errorf("read tournament candidate %s: %w", path, err)
	}
	jsonData, err := strictyaml.YAMLToJSON(raw)
	if err != nil {
		return TournamentCandidateManifest{}, fmt.Errorf("decode tournament candidate %s: %w", path, err)
	}
	var manifest TournamentCandidateManifest
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return TournamentCandidateManifest{}, fmt.Errorf("decode tournament candidate %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("manifest contains multiple JSON values")
		}
		return TournamentCandidateManifest{}, fmt.Errorf("decode tournament candidate %s: %w", path, err)
	}
	if err := validateTournamentCandidate(manifest); err != nil {
		return TournamentCandidateManifest{}, fmt.Errorf("tournament candidate %s: %w", path, err)
	}
	return manifest, nil
}

// RunTournament compares every candidate with the authoritative candidate on
// the same canonical task roster. Candidate input order cannot affect output.
func RunTournament(registry Registry, authoritative TournamentCandidateManifest, candidates []TournamentCandidateManifest) (TournamentReport, error) {
	if len(registry.Tasks) == 0 {
		return TournamentReport{}, errors.New("workflow gym tournament: dataset has no tasks")
	}
	if err := validateTournamentCandidate(authoritative); err != nil {
		return TournamentReport{}, fmt.Errorf("workflow gym tournament: authoritative candidate: %w", err)
	}
	if len(candidates) == 0 {
		return TournamentReport{}, errors.New("workflow gym tournament: at least one candidate is required")
	}

	sortedCandidates := append([]TournamentCandidateManifest(nil), candidates...)
	for i := range sortedCandidates {
		if err := validateTournamentCandidate(sortedCandidates[i]); err != nil {
			return TournamentReport{}, fmt.Errorf("workflow gym tournament: candidate %d: %w", i+1, err)
		}
	}
	sort.Slice(sortedCandidates, func(i, j int) bool {
		return tournamentIdentityKey(sortedCandidates[i].Identity) < tournamentIdentityKey(sortedCandidates[j].Identity)
	})
	if err := validateTournamentRoster(registry, append([]TournamentCandidateManifest{authoritative}, sortedCandidates...)); err != nil {
		return TournamentReport{}, err
	}

	report := TournamentReport{
		APIVersion:     apiv1.WorkflowGymTaskAPIVersion,
		Kind:           TournamentReportKind,
		DatasetVersion: registry.DatasetVersion,
		DatasetDigest:  registry.Digest,
	}
	authoritativeResults := tournamentResultIndex(authoritative)
	for _, candidate := range sortedCandidates {
		candidateResults := tournamentResultIndex(candidate)
		start := len(report.Results)
		for _, task := range registry.Tasks {
			report.Results = append(report.Results, TournamentPairResult{
				TaskID:           task.TaskID,
				Split:            task.Split,
				EvaluatorVersion: task.EvaluatorVersion,
				Authoritative:    authoritative.Identity,
				Candidate:        candidate.Identity,
				AuthoritativeRun: tournamentTaskResult(task.TaskID, authoritativeResults),
				CandidateRun:     tournamentTaskResult(task.TaskID, candidateResults),
			})
		}
		pairs := report.Results[start:]
		report.Aggregates = append(report.Aggregates, tournamentAggregates(authoritative.Identity, candidate.Identity, pairs)...)
	}
	return report, nil
}

func validateTournamentCandidate(manifest TournamentCandidateManifest) error {
	if manifest.APIVersion != apiv1.WorkflowGymTaskAPIVersion {
		return fmt.Errorf("apiVersion must be %q", apiv1.WorkflowGymTaskAPIVersion)
	}
	if manifest.Kind != TournamentCandidateKind {
		return fmt.Errorf("kind must be %q", TournamentCandidateKind)
	}
	identity := manifest.Identity
	required := []struct {
		name  string
		value string
	}{
		{"identity.effectiveVersion.workflowDigest", identity.EffectiveVersion.WorkflowDigest},
		{"identity.effectiveVersion.gooberDigest", identity.EffectiveVersion.GooberDigest},
		{"identity.effectiveVersion.model", identity.EffectiveVersion.Model},
		{"identity.effectiveVersion.harnessVersion", identity.EffectiveVersion.HarnessVersion},
		{"identity.genomeDigest", identity.GenomeDigest},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	seen := make(map[string]bool)
	for i, result := range manifest.Results {
		if strings.TrimSpace(result.TaskID) == "" {
			return fmt.Errorf("results[%d].taskId is required", i)
		}
		if seen[result.TaskID] {
			return fmt.Errorf("duplicate task result %q", result.TaskID)
		}
		seen[result.TaskID] = true
		if result.CostUSD < 0 || result.DurationMS < 0 {
			return fmt.Errorf("task %q has negative cost or duration", result.TaskID)
		}
		switch result.Status {
		case TournamentPass:
			if result.FailureClass != "" {
				return fmt.Errorf("task %q passes but has failureClass %q", result.TaskID, result.FailureClass)
			}
		case TournamentFail:
			if strings.TrimSpace(result.FailureClass) == "" {
				return fmt.Errorf("task %q fails without a failureClass", result.TaskID)
			}
		default:
			return fmt.Errorf("task %q status must be pass or fail", result.TaskID)
		}
	}
	return nil
}

func validateTournamentRoster(registry Registry, manifests []TournamentCandidateManifest) error {
	tasks := make(map[string]bool, len(registry.Tasks))
	for _, task := range registry.Tasks {
		tasks[task.TaskID] = true
	}
	identities := make(map[string]bool, len(manifests))
	for _, manifest := range manifests {
		key := tournamentIdentityKey(manifest.Identity)
		if identities[key] {
			return fmt.Errorf("workflow gym tournament: duplicate candidate identity %q", key)
		}
		identities[key] = true
		for _, result := range manifest.Results {
			if !tasks[result.TaskID] {
				return fmt.Errorf("workflow gym tournament: candidate %q has result for task %q outside dataset", key, result.TaskID)
			}
		}
	}
	return nil
}

func tournamentIdentityKey(identity TournamentCandidateIdentity) string {
	return identity.EffectiveVersion.Hash() + "/" + identity.GenomeDigest
}

func tournamentResultIndex(manifest TournamentCandidateManifest) map[string]TournamentCandidateResult {
	results := make(map[string]TournamentCandidateResult, len(manifest.Results))
	for _, result := range manifest.Results {
		results[result.TaskID] = result
	}
	return results
}

func tournamentTaskResult(taskID string, results map[string]TournamentCandidateResult) TournamentCandidateResult {
	if result, ok := results[taskID]; ok {
		return result
	}
	return TournamentCandidateResult{
		TaskID:       taskID,
		Status:       TournamentFail,
		FailureClass: "missing-result",
	}
}

func tournamentAggregates(authoritative, candidate TournamentCandidateIdentity, pairs []TournamentPairResult) []TournamentAggregate {
	aggregates := []TournamentAggregate{
		tournamentAggregate("all", authoritative, candidate, pairs),
	}
	for _, split := range []apiv1.WorkflowGymSplit{
		apiv1.WorkflowGymSplitTrain,
		apiv1.WorkflowGymSplitEval,
		apiv1.WorkflowGymSplitHoldout,
	} {
		var selected []TournamentPairResult
		for _, pair := range pairs {
			if pair.Split == split {
				selected = append(selected, pair)
			}
		}
		if len(selected) != 0 {
			aggregates = append(aggregates, tournamentAggregate(string(split), authoritative, candidate, selected))
		}
	}
	return aggregates
}

func tournamentAggregate(split string, authoritative, candidate TournamentCandidateIdentity, pairs []TournamentPairResult) TournamentAggregate {
	beforeResults := make([]TournamentCandidateResult, 0, len(pairs))
	afterResults := make([]TournamentCandidateResult, 0, len(pairs))
	for _, pair := range pairs {
		beforeResults = append(beforeResults, pair.AuthoritativeRun)
		afterResults = append(afterResults, pair.CandidateRun)
	}
	before := tournamentStats(beforeResults)
	after := tournamentStats(afterResults)
	verdict := rollup.EfficacyNoChange
	switch {
	case after.PassRate > before.PassRate:
		verdict = rollup.EfficacyHelped
	case after.PassRate < before.PassRate:
		verdict = rollup.EfficacyRegressed
	}
	return TournamentAggregate{
		Split:         split,
		Authoritative: authoritative,
		Candidate:     candidate,
		TaskCount:     len(pairs),
		Before:        before,
		After:         after,
		Delta: TournamentDelta{
			PassRate:   after.PassRate - before.PassRate,
			CostUSD:    after.CostUSD - before.CostUSD,
			DurationMS: after.DurationMS - before.DurationMS,
		},
		Verdict: verdict,
	}
}

func tournamentStats(results []TournamentCandidateResult) TournamentStats {
	stats := TournamentStats{FailureClasses: make(map[string]int)}
	for _, result := range results {
		stats.CostUSD += result.CostUSD
		stats.DurationMS += result.DurationMS
		if result.Status == TournamentPass {
			stats.Passed++
		} else {
			stats.Failed++
			stats.FailureClasses[result.FailureClass]++
		}
	}
	if len(results) != 0 {
		stats.PassRate = float64(stats.Passed) / float64(len(results))
	}
	return stats
}
