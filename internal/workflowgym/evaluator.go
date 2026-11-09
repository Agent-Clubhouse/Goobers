package workflowgym

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	// EvaluationReportKind is the kind of Workflow Gym regression reports.
	EvaluationReportKind = "WorkflowGymEvaluation"
	// DefaultEvaluationMinSamples is the default sample floor for each selection slice.
	DefaultEvaluationMinSamples = 10
	// DefaultRegressionThreshold is the default tolerated pass-rate regression.
	DefaultRegressionThreshold = 0.05
	// EvaluationConfidenceLevel is the fixed confidence level used by reports.
	EvaluationConfidenceLevel = 0.95
)

// EvaluationVerdict is the machine-readable regression gate result.
type EvaluationVerdict string

const (
	EvaluationPass         EvaluationVerdict = "pass"
	EvaluationFail         EvaluationVerdict = "fail"
	EvaluationInconclusive EvaluationVerdict = "inconclusive"
)

// EvaluationConfig controls evidence floors and tolerated regression.
type EvaluationConfig struct {
	MinSamples          int     `json:"minSamples"`
	RegressionThreshold float64 `json:"regressionThreshold"`
}

// EvaluationReport is a deterministic confidence report and regression gate.
type EvaluationReport struct {
	APIVersion      string                       `json:"apiVersion"`
	Kind            string                       `json:"kind"`
	Config          EvaluationConfig             `json:"config"`
	ConfidenceLevel float64                      `json:"confidenceLevel"`
	Digests         EvaluationDigests            `json:"digests"`
	Gate            EvaluationGate               `json:"gate"`
	Candidates      []EvaluationCandidateReport  `json:"candidates"`
	Leaderboard     []EvaluationLeaderboardEntry `json:"leaderboard"`
}

// EvaluationDigests pin the evidence and execution inputs behind a report.
type EvaluationDigests struct {
	Dataset     string `json:"dataset"`
	Evaluator   string `json:"evaluator"`
	Environment string `json:"environment"`
}

// EvaluationGate is the aggregate machine-readable gate result.
type EvaluationGate struct {
	Verdict EvaluationVerdict `json:"verdict"`
	Reasons []string          `json:"reasons"`
}

// EvaluationCandidateReport contains selection and held-out slice evidence.
type EvaluationCandidateReport struct {
	Candidate       TournamentCandidateIdentity `json:"candidate"`
	CandidateDigest string                      `json:"candidateDigest"`
	Gate            EvaluationGate              `json:"gate"`
	Slices          []EvaluationSlice           `json:"slices"`
}

// EvaluationSlice is one paired pass-rate estimate and its 95% confidence interval.
type EvaluationSlice struct {
	Split              apiv1.WorkflowGymSplit `json:"split"`
	Selection          bool                   `json:"selection"`
	Samples            int                    `json:"samples"`
	AuthoritativeRate  float64                `json:"authoritativeRate"`
	CandidateRate      float64                `json:"candidateRate"`
	Delta              float64                `json:"delta"`
	ConfidenceInterval EvaluationInterval     `json:"confidenceInterval"`
	Verdict            EvaluationVerdict      `json:"verdict,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
}

// EvaluationInterval is a deterministic two-sided 95% interval.
type EvaluationInterval struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// EvaluationLeaderboardEntry ranks candidates using selection tasks only.
type EvaluationLeaderboardEntry struct {
	Rank            int                         `json:"rank"`
	Candidate       TournamentCandidateIdentity `json:"candidate"`
	CandidateDigest string                      `json:"candidateDigest"`
	PassRate        float64                     `json:"passRate"`
	CostUSD         float64                     `json:"costUsd"`
	DurationMS      int64                       `json:"durationMs"`
	ParetoFrontier  bool                        `json:"paretoFrontier"`
	HeldOut         *EvaluationHeldOutMetrics   `json:"heldOut,omitempty"`
}

// EvaluationHeldOutMetrics reports held-out performance without using it for selection.
type EvaluationHeldOutMetrics struct {
	Samples    int     `json:"samples"`
	PassRate   float64 `json:"passRate"`
	CostUSD    float64 `json:"costUsd"`
	DurationMS int64   `json:"durationMs"`
}

// LoadTournamentReport strictly decodes a tournament report.
func LoadTournamentReport(path string) (TournamentReport, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return TournamentReport{}, fmt.Errorf("read tournament report %s: %w", path, err)
	}
	var report TournamentReport
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return TournamentReport{}, fmt.Errorf("decode tournament report %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("report contains multiple JSON values")
		}
		return TournamentReport{}, fmt.Errorf("decode tournament report %s: %w", path, err)
	}
	return report, nil
}

// EvaluateTournament evaluates paired tournament output without mutating it.
func EvaluateTournament(registry Registry, tournament TournamentReport, config EvaluationConfig) (EvaluationReport, error) {
	if err := validateEvaluationConfig(config); err != nil {
		return EvaluationReport{}, err
	}
	grouped, identities, err := validateEvaluationTournament(registry, tournament)
	if err != nil {
		return EvaluationReport{}, err
	}
	report := EvaluationReport{
		APIVersion:      apiv1.WorkflowGymTaskAPIVersion,
		Kind:            EvaluationReportKind,
		Config:          config,
		ConfidenceLevel: EvaluationConfidenceLevel,
		Digests: EvaluationDigests{
			Dataset:     registry.Digest,
			Evaluator:   evaluationDigest(registry, func(task apiv1.WorkflowGymTaskManifest) any { return task.EvaluatorVersion }),
			Environment: evaluationDigest(registry, func(task apiv1.WorkflowGymTaskManifest) any { return task.CandidateEnvironment }),
		},
		Gate: EvaluationGate{Verdict: EvaluationPass, Reasons: []string{}},
	}
	for _, identity := range identities {
		pairs := grouped[tournamentIdentityKey(identity)]
		candidate := evaluateCandidate(identity, pairs, config)
		report.Candidates = append(report.Candidates, candidate)
		report.Leaderboard = append(report.Leaderboard, evaluationLeaderboardEntry(identity, pairs))
		if candidate.Gate.Verdict == EvaluationFail {
			report.Gate.Verdict = EvaluationFail
		} else if candidate.Gate.Verdict == EvaluationInconclusive && report.Gate.Verdict != EvaluationFail {
			report.Gate.Verdict = EvaluationInconclusive
		}
		for _, reason := range candidate.Gate.Reasons {
			report.Gate.Reasons = append(report.Gate.Reasons, candidate.CandidateDigest+": "+reason)
		}
	}
	rankEvaluationLeaderboard(report.Leaderboard)
	return report, nil
}

func validateEvaluationConfig(config EvaluationConfig) error {
	if config.MinSamples < 1 {
		return errors.New("workflow gym evaluation: minSamples must be at least 1")
	}
	if math.IsNaN(config.RegressionThreshold) || math.IsInf(config.RegressionThreshold, 0) ||
		config.RegressionThreshold < 0 || config.RegressionThreshold > 1 {
		return errors.New("workflow gym evaluation: regressionThreshold must be between 0 and 1")
	}
	return nil
}

func validateEvaluationTournament(registry Registry, report TournamentReport) (map[string][]TournamentPairResult, []TournamentCandidateIdentity, error) {
	if report.APIVersion != apiv1.WorkflowGymTaskAPIVersion || report.Kind != TournamentReportKind {
		return nil, nil, errors.New("workflow gym evaluation: invalid tournament apiVersion or kind")
	}
	if report.DatasetVersion != registry.DatasetVersion || report.DatasetDigest != registry.Digest {
		return nil, nil, errors.New("workflow gym evaluation: tournament dataset does not match loaded dataset")
	}
	if len(report.Results) == 0 {
		return nil, nil, errors.New("workflow gym evaluation: tournament has no paired results")
	}
	tasks := make(map[string]apiv1.WorkflowGymTaskManifest, len(registry.Tasks))
	for _, task := range registry.Tasks {
		tasks[task.TaskID] = task
	}
	grouped := make(map[string][]TournamentPairResult)
	identityByKey := make(map[string]TournamentCandidateIdentity)
	seen := make(map[string]bool)
	authoritativeResults := make(map[string]TournamentCandidateResult, len(registry.Tasks))
	var authoritative *TournamentCandidateIdentity
	for _, pair := range report.Results {
		task, ok := tasks[pair.TaskID]
		if !ok || pair.Split != task.Split || pair.EvaluatorVersion != task.EvaluatorVersion {
			return nil, nil, fmt.Errorf("workflow gym evaluation: task %q does not match loaded dataset", pair.TaskID)
		}
		if pair.AuthoritativeRun.TaskID != pair.TaskID || pair.CandidateRun.TaskID != pair.TaskID ||
			pair.AuthoritativeRun.EvaluatorVersion != pair.EvaluatorVersion || pair.CandidateRun.EvaluatorVersion != pair.EvaluatorVersion {
			return nil, nil, fmt.Errorf("workflow gym evaluation: task %q has inconsistent paired results", pair.TaskID)
		}
		if err := validateEvaluationResult(pair.AuthoritativeRun); err != nil {
			return nil, nil, fmt.Errorf("workflow gym evaluation: task %q authoritative result: %w", pair.TaskID, err)
		}
		if err := validateEvaluationResult(pair.CandidateRun); err != nil {
			return nil, nil, fmt.Errorf("workflow gym evaluation: task %q candidate result: %w", pair.TaskID, err)
		}
		if prior, ok := authoritativeResults[pair.TaskID]; ok && prior != pair.AuthoritativeRun {
			return nil, nil, fmt.Errorf("workflow gym evaluation: task %q has inconsistent authoritative results", pair.TaskID)
		}
		authoritativeResults[pair.TaskID] = pair.AuthoritativeRun
		if err := validateEvaluationIdentity(pair.Authoritative); err != nil {
			return nil, nil, fmt.Errorf("workflow gym evaluation: authoritative identity: %w", err)
		}
		if err := validateEvaluationIdentity(pair.Candidate); err != nil {
			return nil, nil, fmt.Errorf("workflow gym evaluation: candidate identity: %w", err)
		}
		if authoritative == nil {
			value := pair.Authoritative
			authoritative = &value
		} else if *authoritative != pair.Authoritative {
			return nil, nil, errors.New("workflow gym evaluation: tournament uses multiple authoritative identities")
		}
		if pair.Authoritative == pair.Candidate {
			return nil, nil, errors.New("workflow gym evaluation: candidate matches authoritative identity")
		}
		key := tournamentIdentityKey(pair.Candidate)
		resultKey := key + "\x00" + pair.TaskID
		if seen[resultKey] {
			return nil, nil, fmt.Errorf("workflow gym evaluation: duplicate result for candidate %q task %q", key, pair.TaskID)
		}
		seen[resultKey] = true
		grouped[key] = append(grouped[key], pair)
		identityByKey[key] = pair.Candidate
	}
	keys := make([]string, 0, len(grouped))
	for key, pairs := range grouped {
		if len(pairs) != len(registry.Tasks) {
			return nil, nil, fmt.Errorf("workflow gym evaluation: candidate %q covers %d of %d tasks", key, len(pairs), len(registry.Tasks))
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].TaskID < pairs[j].TaskID })
		grouped[key] = pairs
		keys = append(keys, key)
	}
	sort.Strings(keys)
	identities := make([]TournamentCandidateIdentity, 0, len(keys))
	for _, key := range keys {
		identities = append(identities, identityByKey[key])
	}
	return grouped, identities, nil
}

func validateEvaluationIdentity(identity TournamentCandidateIdentity) error {
	required := []struct {
		name  string
		value string
	}{
		{"effectiveVersion.workflowDigest", identity.EffectiveVersion.WorkflowDigest},
		{"effectiveVersion.gooberDigest", identity.EffectiveVersion.GooberDigest},
		{"effectiveVersion.model", identity.EffectiveVersion.Model},
		{"effectiveVersion.harnessVersion", identity.EffectiveVersion.HarnessVersion},
		{"genomeDigest", identity.GenomeDigest},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	return nil
}

func validateEvaluationResult(result TournamentCandidateResult) error {
	if result.CostUSD < 0 || result.DurationMS < 0 {
		return errors.New("negative cost or duration")
	}
	switch result.Status {
	case TournamentPass:
		if result.FailureClass != "" {
			return errors.New("passing result has a failureClass")
		}
	case TournamentFail:
		if result.FailureClass == "" {
			return errors.New("failing result has no failureClass")
		}
	default:
		return fmt.Errorf("invalid status %q", result.Status)
	}
	return nil
}

func evaluateCandidate(identity TournamentCandidateIdentity, pairs []TournamentPairResult, config EvaluationConfig) EvaluationCandidateReport {
	result := EvaluationCandidateReport{
		Candidate:       identity,
		CandidateDigest: digestJSON(identity),
		Gate:            EvaluationGate{Verdict: EvaluationPass, Reasons: []string{}},
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
		if len(selected) == 0 {
			continue
		}
		slice := evaluationSlice(split, selected, config)
		result.Slices = append(result.Slices, slice)
		if !slice.Selection {
			continue
		}
		if slice.Verdict == EvaluationFail {
			result.Gate.Verdict = EvaluationFail
		} else if slice.Verdict == EvaluationInconclusive && result.Gate.Verdict != EvaluationFail {
			result.Gate.Verdict = EvaluationInconclusive
		}
		if slice.Reason != "" {
			result.Gate.Reasons = append(result.Gate.Reasons, string(split)+": "+slice.Reason)
		}
	}
	if len(result.Slices) == 0 || !hasSelectionSlice(result.Slices) {
		result.Gate.Verdict = EvaluationInconclusive
		result.Gate.Reasons = append(result.Gate.Reasons, "no train or eval evidence")
	}
	return result
}

func evaluationSlice(split apiv1.WorkflowGymSplit, pairs []TournamentPairResult, config EvaluationConfig) EvaluationSlice {
	var before, after int
	deltas := make([]float64, 0, len(pairs))
	for _, pair := range pairs {
		beforePass := pair.AuthoritativeRun.Status == TournamentPass
		afterPass := pair.CandidateRun.Status == TournamentPass
		if beforePass {
			before++
		}
		if afterPass {
			after++
		}
		deltas = append(deltas, boolFloat(afterPass)-boolFloat(beforePass))
	}
	delta, interval := pairedMeanInterval(deltas)
	result := EvaluationSlice{
		Split:              split,
		Selection:          split != apiv1.WorkflowGymSplitHoldout,
		Samples:            len(pairs),
		AuthoritativeRate:  float64(before) / float64(len(pairs)),
		CandidateRate:      float64(after) / float64(len(pairs)),
		Delta:              delta,
		ConfidenceInterval: interval,
	}
	if !result.Selection {
		return result
	}
	switch {
	case len(pairs) < config.MinSamples:
		result.Verdict = EvaluationInconclusive
		result.Reason = fmt.Sprintf("insufficient evidence: %d samples, require %d", len(pairs), config.MinSamples)
	case delta < 0 && delta <= -config.RegressionThreshold:
		result.Verdict = EvaluationFail
		result.Reason = fmt.Sprintf("pass-rate delta %.6g meets regression threshold -%.6g", delta, config.RegressionThreshold)
	case interval.Lower < -config.RegressionThreshold:
		result.Verdict = EvaluationInconclusive
		result.Reason = "confidence interval crosses the regression threshold"
	default:
		result.Verdict = EvaluationPass
	}
	return result
}

func pairedMeanInterval(values []float64) (float64, EvaluationInterval) {
	var sum float64
	for _, value := range values {
		sum += value
	}
	mean := sum / float64(len(values))
	if len(values) == 1 {
		return mean, EvaluationInterval{Lower: mean, Upper: mean}
	}
	var squares float64
	for _, value := range values {
		difference := value - mean
		squares += difference * difference
	}
	standardError := math.Sqrt(squares/float64(len(values)-1)) / math.Sqrt(float64(len(values)))
	margin := 1.959963984540054 * standardError
	return mean, EvaluationInterval{
		Lower: math.Max(-1, mean-margin),
		Upper: math.Min(1, mean+margin),
	}
}

func evaluationLeaderboardEntry(identity TournamentCandidateIdentity, pairs []TournamentPairResult) EvaluationLeaderboardEntry {
	entry := EvaluationLeaderboardEntry{Candidate: identity, CandidateDigest: digestJSON(identity)}
	var selectionSamples, selectionPassed int
	var heldOut EvaluationHeldOutMetrics
	for _, pair := range pairs {
		run := pair.CandidateRun
		if pair.Split == apiv1.WorkflowGymSplitHoldout {
			heldOut.Samples++
			heldOut.CostUSD += run.CostUSD
			heldOut.DurationMS += run.DurationMS
			if run.Status == TournamentPass {
				heldOut.PassRate++
			}
			continue
		}
		selectionSamples++
		entry.CostUSD += run.CostUSD
		entry.DurationMS += run.DurationMS
		if run.Status == TournamentPass {
			selectionPassed++
		}
	}
	if selectionSamples != 0 {
		entry.PassRate = float64(selectionPassed) / float64(selectionSamples)
	}
	if heldOut.Samples != 0 {
		heldOut.PassRate /= float64(heldOut.Samples)
		entry.HeldOut = &heldOut
	}
	return entry
}

func rankEvaluationLeaderboard(entries []EvaluationLeaderboardEntry) {
	for i := range entries {
		entries[i].ParetoFrontier = true
		for j := range entries {
			if i != j && dominates(entries[j], entries[i]) {
				entries[i].ParetoFrontier = false
				break
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].PassRate != entries[j].PassRate {
			return entries[i].PassRate > entries[j].PassRate
		}
		if entries[i].CostUSD != entries[j].CostUSD {
			return entries[i].CostUSD < entries[j].CostUSD
		}
		if entries[i].DurationMS != entries[j].DurationMS {
			return entries[i].DurationMS < entries[j].DurationMS
		}
		return entries[i].CandidateDigest < entries[j].CandidateDigest
	})
	for i := range entries {
		entries[i].Rank = i + 1
	}
}

func dominates(left, right EvaluationLeaderboardEntry) bool {
	noWorse := left.PassRate >= right.PassRate && left.CostUSD <= right.CostUSD && left.DurationMS <= right.DurationMS
	strictlyBetter := left.PassRate > right.PassRate || left.CostUSD < right.CostUSD || left.DurationMS < right.DurationMS
	return noWorse && strictlyBetter
}

func evaluationDigest(registry Registry, value func(apiv1.WorkflowGymTaskManifest) any) string {
	rows := make([]struct {
		TaskID string `json:"taskId"`
		Value  any    `json:"value"`
	}, 0, len(registry.Tasks))
	for _, task := range registry.Tasks {
		rows = append(rows, struct {
			TaskID string `json:"taskId"`
			Value  any    `json:"value"`
		}{TaskID: task.TaskID, Value: value(task)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TaskID < rows[j].TaskID })
	return digestJSON(rows)
}

func digestJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("workflow gym: digest JSON: %v", err))
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func hasSelectionSlice(slices []EvaluationSlice) bool {
	for _, slice := range slices {
		if slice.Selection {
			return true
		}
	}
	return false
}
