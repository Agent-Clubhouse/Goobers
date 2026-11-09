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
	// EvaluationConfidenceLevel is the fixed simultaneous confidence level used by reports.
	EvaluationConfidenceLevel = 0.95

	// evaluationWilsonZ is the standard normal quantile for each of the four
	// one-sided Wilson bounds behind a paired-difference interval. Splitting
	// the 5% error budget four ways (Bonferroni) keeps the interval's
	// simultaneous coverage at least EvaluationConfidenceLevel.
	evaluationWilsonZ = 2.241402727604947
)

// EvaluationVerdict is the machine-readable regression gate result.
type EvaluationVerdict string

const (
	// EvaluationPass means the evidence bounds any regression within the threshold.
	EvaluationPass EvaluationVerdict = "pass"
	// EvaluationFail means the measured regression meets the threshold.
	EvaluationFail EvaluationVerdict = "fail"
	// EvaluationInconclusive means the evidence cannot rule a regression in or out.
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
	Baseline        EvaluationBaseline           `json:"baseline"`
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

// EvaluationBaseline reports the authoritative selection metrics every
// candidate is compared against.
type EvaluationBaseline struct {
	Authoritative       TournamentCandidateIdentity `json:"authoritative"`
	AuthoritativeDigest string                      `json:"authoritativeDigest"`
	Samples             int                         `json:"samples"`
	PassRate            float64                     `json:"passRate"`
	CostUSD             float64                     `json:"costUsd"`
	DurationMS          int64                       `json:"durationMs"`
}

// EvaluationCandidateReport contains selection and held-out slice evidence.
type EvaluationCandidateReport struct {
	Candidate       TournamentCandidateIdentity `json:"candidate"`
	CandidateDigest string                      `json:"candidateDigest"`
	Gate            EvaluationGate              `json:"gate"`
	Slices          []EvaluationSlice           `json:"slices"`
}

// EvaluationSlice is one paired pass-rate estimate and its 95% confidence
// interval. Samples counts measured pairs only; candidate results the
// tournament synthesized for omitted evidence are counted in MissingResults.
type EvaluationSlice struct {
	Split              apiv1.WorkflowGymSplit `json:"split"`
	Selection          bool                   `json:"selection"`
	Samples            int                    `json:"samples"`
	MissingResults     int                    `json:"missingResults,omitempty"`
	AuthoritativeRate  float64                `json:"authoritativeRate"`
	CandidateRate      float64                `json:"candidateRate"`
	Delta              float64                `json:"delta"`
	ConfidenceInterval EvaluationInterval     `json:"confidenceInterval"`
	Verdict            EvaluationVerdict      `json:"verdict,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
}

// EvaluationInterval is a deterministic two-sided 95% interval for a paired
// pass-rate delta. It never collapses to a point: even an unchanged slice
// keeps a width that shrinks only as samples grow.
type EvaluationInterval struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// EvaluationLeaderboardEntry ranks candidates using selection tasks only.
// Candidates whose gate did not pass rank after every passing candidate and
// never join the Pareto frontier.
type EvaluationLeaderboardEntry struct {
	Rank               int                         `json:"rank"`
	Candidate          TournamentCandidateIdentity `json:"candidate"`
	CandidateDigest    string                      `json:"candidateDigest"`
	Verdict            EvaluationVerdict           `json:"verdict"`
	Samples            int                         `json:"samples"`
	PassRate           float64                     `json:"passRate"`
	Delta              float64                     `json:"delta"`
	ConfidenceInterval EvaluationInterval          `json:"confidenceInterval"`
	CostUSD            float64                     `json:"costUsd"`
	DurationMS         int64                       `json:"durationMs"`
	ParetoFrontier     bool                        `json:"paretoFrontier"`
	HeldOut            *EvaluationHeldOutMetrics   `json:"heldOut,omitempty"`
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
		Gate:     EvaluationGate{Verdict: EvaluationPass, Reasons: []string{}},
		Baseline: evaluationBaseline(grouped[tournamentIdentityKey(identities[0])]),
	}
	for _, identity := range identities {
		pairs := grouped[tournamentIdentityKey(identity)]
		candidate := evaluateCandidate(identity, pairs, config)
		report.Candidates = append(report.Candidates, candidate)
		report.Leaderboard = append(report.Leaderboard, evaluationLeaderboardEntry(identity, pairs, candidate.Gate.Verdict))
		report.Gate.Verdict = worseEvaluationVerdict(report.Gate.Verdict, candidate.Gate.Verdict)
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

type evaluationValidation struct {
	tasks                map[string]apiv1.WorkflowGymTaskManifest
	grouped              map[string][]TournamentPairResult
	identityByKey        map[string]TournamentCandidateIdentity
	seen                 map[string]bool
	authoritativeResults map[string]TournamentCandidateResult
	authoritative        *TournamentCandidateIdentity
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
	validation := evaluationValidation{
		tasks:                make(map[string]apiv1.WorkflowGymTaskManifest, len(registry.Tasks)),
		grouped:              make(map[string][]TournamentPairResult),
		identityByKey:        make(map[string]TournamentCandidateIdentity),
		seen:                 make(map[string]bool),
		authoritativeResults: make(map[string]TournamentCandidateResult, len(registry.Tasks)),
	}
	for _, task := range registry.Tasks {
		validation.tasks[task.TaskID] = task
	}
	for _, pair := range report.Results {
		if err := validation.add(pair); err != nil {
			return nil, nil, err
		}
	}
	return validation.finish()
}

func (v *evaluationValidation) add(pair TournamentPairResult) error {
	if err := validateEvaluationPair(v.tasks, pair); err != nil {
		return err
	}
	if prior, ok := v.authoritativeResults[pair.TaskID]; ok && prior != pair.AuthoritativeRun {
		return fmt.Errorf("workflow gym evaluation: task %q has inconsistent authoritative results", pair.TaskID)
	}
	v.authoritativeResults[pair.TaskID] = pair.AuthoritativeRun
	if v.authoritative == nil {
		value := pair.Authoritative
		v.authoritative = &value
	} else if *v.authoritative != pair.Authoritative {
		return errors.New("workflow gym evaluation: tournament uses multiple authoritative identities")
	}
	key := tournamentIdentityKey(pair.Candidate)
	resultKey := key + "\x00" + pair.TaskID
	if v.seen[resultKey] {
		return fmt.Errorf("workflow gym evaluation: duplicate result for candidate %q task %q", key, pair.TaskID)
	}
	v.seen[resultKey] = true
	v.grouped[key] = append(v.grouped[key], pair)
	v.identityByKey[key] = pair.Candidate
	return nil
}

func (v *evaluationValidation) finish() (map[string][]TournamentPairResult, []TournamentCandidateIdentity, error) {
	keys := make([]string, 0, len(v.grouped))
	for key, pairs := range v.grouped {
		if len(pairs) != len(v.tasks) {
			return nil, nil, fmt.Errorf("workflow gym evaluation: candidate %q covers %d of %d tasks", key, len(pairs), len(v.tasks))
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].TaskID < pairs[j].TaskID })
		v.grouped[key] = pairs
		keys = append(keys, key)
	}
	sort.Strings(keys)
	identities := make([]TournamentCandidateIdentity, 0, len(keys))
	for _, key := range keys {
		identities = append(identities, v.identityByKey[key])
	}
	return v.grouped, identities, nil
}

func validateEvaluationPair(tasks map[string]apiv1.WorkflowGymTaskManifest, pair TournamentPairResult) error {
	task, ok := tasks[pair.TaskID]
	if !ok || pair.Split != task.Split || pair.EvaluatorVersion != task.EvaluatorVersion {
		return fmt.Errorf("workflow gym evaluation: task %q does not match loaded dataset", pair.TaskID)
	}
	if pair.AuthoritativeRun.TaskID != pair.TaskID || pair.CandidateRun.TaskID != pair.TaskID ||
		pair.AuthoritativeRun.EvaluatorVersion != pair.EvaluatorVersion || pair.CandidateRun.EvaluatorVersion != pair.EvaluatorVersion {
		return fmt.Errorf("workflow gym evaluation: task %q has inconsistent paired results", pair.TaskID)
	}
	if err := validateEvaluationResult(pair.AuthoritativeRun); err != nil {
		return fmt.Errorf("workflow gym evaluation: task %q authoritative result: %w", pair.TaskID, err)
	}
	if isMissingEvaluationResult(pair.AuthoritativeRun) {
		return fmt.Errorf("workflow gym evaluation: task %q authoritative result is missing; the baseline must measure every task", pair.TaskID)
	}
	if err := validateEvaluationResult(pair.CandidateRun); err != nil {
		return fmt.Errorf("workflow gym evaluation: task %q candidate result: %w", pair.TaskID, err)
	}
	if err := validateEvaluationIdentity(pair.Authoritative); err != nil {
		return fmt.Errorf("workflow gym evaluation: authoritative identity: %w", err)
	}
	if err := validateEvaluationIdentity(pair.Candidate); err != nil {
		return fmt.Errorf("workflow gym evaluation: candidate identity: %w", err)
	}
	if pair.Authoritative == pair.Candidate {
		return errors.New("workflow gym evaluation: candidate matches authoritative identity")
	}
	return nil
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

// isMissingEvaluationResult reports whether a result records absent evidence
// that the tournament synthesized, rather than a measured outcome.
func isMissingEvaluationResult(result TournamentCandidateResult) bool {
	return result.FailureClass == tournamentMissingResultFailureClass
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
		result.Gate.Verdict = worseEvaluationVerdict(result.Gate.Verdict, slice.Verdict)
		if slice.Reason != "" {
			result.Gate.Reasons = append(result.Gate.Reasons, string(split)+": "+slice.Reason)
		}
	}
	if !hasSelectionSlice(result.Slices) {
		result.Gate.Verdict = EvaluationInconclusive
		result.Gate.Reasons = append(result.Gate.Reasons, "no train or eval evidence")
	}
	return result
}

// evaluationCounts tallies paired outcomes for one set of tasks. Pairs whose
// candidate result is missing are counted separately and never measured.
type evaluationCounts struct {
	samples             int
	missing             int
	authoritativePassed int
	candidatePassed     int
	improvements        int
	regressions         int
}

func countEvaluationPairs(pairs []TournamentPairResult) evaluationCounts {
	var counts evaluationCounts
	for _, pair := range pairs {
		if isMissingEvaluationResult(pair.CandidateRun) {
			counts.missing++
			continue
		}
		counts.samples++
		before := pair.AuthoritativeRun.Status == TournamentPass
		after := pair.CandidateRun.Status == TournamentPass
		if before {
			counts.authoritativePassed++
		}
		if after {
			counts.candidatePassed++
		}
		if after && !before {
			counts.improvements++
		}
		if before && !after {
			counts.regressions++
		}
	}
	return counts
}

func (c evaluationCounts) rates() (authoritative, candidate float64) {
	if c.samples == 0 {
		return 0, 0
	}
	return float64(c.authoritativePassed) / float64(c.samples), float64(c.candidatePassed) / float64(c.samples)
}

// pairedDelta returns the paired pass-rate delta and its confidence interval.
// The delta equals the improvement rate minus the regression rate over the
// discordant pairs, so the interval combines Wilson score bounds on both
// rates. Wilson bounds stay wide when no outcome changes, rise monotonically
// with improvements and fall with regressions, so a strictly better candidate
// never receives a lower bound below an unchanged one.
func (c evaluationCounts) pairedDelta() (float64, EvaluationInterval) {
	if c.samples == 0 {
		return 0, EvaluationInterval{Lower: -1, Upper: 1}
	}
	improvementLower, improvementUpper := wilsonBounds(c.improvements, c.samples)
	regressionLower, regressionUpper := wilsonBounds(c.regressions, c.samples)
	return float64(c.improvements-c.regressions) / float64(c.samples), EvaluationInterval{
		Lower: roundEvaluationBound(improvementLower-regressionUpper, math.Floor),
		Upper: roundEvaluationBound(improvementUpper-regressionLower, math.Ceil),
	}
}

func wilsonBounds(successes, samples int) (float64, float64) {
	n := float64(samples)
	x := float64(successes)
	z2 := evaluationWilsonZ * evaluationWilsonZ
	denominator := n + z2
	center := (x + z2/2) / denominator
	margin := evaluationWilsonZ * math.Sqrt(x*(n-x)/n+z2/4) / denominator
	lower, upper := center-margin, center+margin
	if successes == 0 {
		lower = 0
	}
	if successes == samples {
		upper = 1
	}
	return math.Max(0, lower), math.Min(1, upper)
}

// roundEvaluationBound rounds an interval bound outward to six decimals so
// reports are stable across floating-point implementations without ever
// narrowing the interval.
func roundEvaluationBound(value float64, round func(float64) float64) float64 {
	return math.Max(-1, math.Min(1, round(value*1e6)/1e6))
}

func evaluationSlice(split apiv1.WorkflowGymSplit, pairs []TournamentPairResult, config EvaluationConfig) EvaluationSlice {
	counts := countEvaluationPairs(pairs)
	delta, interval := counts.pairedDelta()
	authoritativeRate, candidateRate := counts.rates()
	result := EvaluationSlice{
		Split:              split,
		Selection:          split != apiv1.WorkflowGymSplitHoldout,
		Samples:            counts.samples,
		MissingResults:     counts.missing,
		AuthoritativeRate:  authoritativeRate,
		CandidateRate:      candidateRate,
		Delta:              delta,
		ConfidenceInterval: interval,
	}
	if result.Selection {
		result.Verdict, result.Reason = evaluationSliceVerdict(counts, delta, interval, config)
	}
	return result
}

// evaluationSliceVerdict fails a slice whose measured regression meets the
// threshold and passes it only when measured evidence bounds any regression
// within the threshold; everything else is inconclusive.
func evaluationSliceVerdict(counts evaluationCounts, delta float64, interval EvaluationInterval, config EvaluationConfig) (EvaluationVerdict, string) {
	switch {
	case counts.samples < config.MinSamples:
		return EvaluationInconclusive, fmt.Sprintf("insufficient evidence: %d measured samples, require %d", counts.samples, config.MinSamples)
	case delta < 0 && delta <= -config.RegressionThreshold:
		return EvaluationFail, fmt.Sprintf("pass-rate delta %.6g meets regression threshold -%.6g", delta, config.RegressionThreshold)
	case counts.missing > 0:
		return EvaluationInconclusive, fmt.Sprintf("missing evidence: %d candidate results absent", counts.missing)
	case interval.Lower < -config.RegressionThreshold:
		return EvaluationInconclusive, fmt.Sprintf("confidence interval lower bound %.6g crosses regression threshold -%.6g", interval.Lower, config.RegressionThreshold)
	default:
		return EvaluationPass, ""
	}
}

func worseEvaluationVerdict(current, next EvaluationVerdict) EvaluationVerdict {
	if evaluationVerdictSeverity(next) > evaluationVerdictSeverity(current) {
		return next
	}
	return current
}

func evaluationVerdictSeverity(verdict EvaluationVerdict) int {
	switch verdict {
	case EvaluationPass:
		return 0
	case EvaluationInconclusive:
		return 1
	default:
		return 2
	}
}

func evaluationBaseline(pairs []TournamentPairResult) EvaluationBaseline {
	baseline := EvaluationBaseline{
		Authoritative:       pairs[0].Authoritative,
		AuthoritativeDigest: digestJSON(pairs[0].Authoritative),
	}
	var passed int
	for _, pair := range pairs {
		if pair.Split == apiv1.WorkflowGymSplitHoldout {
			continue
		}
		run := pair.AuthoritativeRun
		baseline.Samples++
		baseline.CostUSD += run.CostUSD
		baseline.DurationMS += run.DurationMS
		if run.Status == TournamentPass {
			passed++
		}
	}
	if baseline.Samples != 0 {
		baseline.PassRate = float64(passed) / float64(baseline.Samples)
	}
	return baseline
}

func evaluationLeaderboardEntry(identity TournamentCandidateIdentity, pairs []TournamentPairResult, verdict EvaluationVerdict) EvaluationLeaderboardEntry {
	entry := EvaluationLeaderboardEntry{Candidate: identity, CandidateDigest: digestJSON(identity), Verdict: verdict}
	var selection []TournamentPairResult
	var heldOut EvaluationHeldOutMetrics
	for _, pair := range pairs {
		run := pair.CandidateRun
		if isMissingEvaluationResult(run) {
			continue
		}
		if pair.Split == apiv1.WorkflowGymSplitHoldout {
			heldOut.Samples++
			heldOut.CostUSD += run.CostUSD
			heldOut.DurationMS += run.DurationMS
			if run.Status == TournamentPass {
				heldOut.PassRate++
			}
			continue
		}
		selection = append(selection, pair)
		entry.CostUSD += run.CostUSD
		entry.DurationMS += run.DurationMS
	}
	counts := countEvaluationPairs(selection)
	entry.Samples = counts.samples
	_, entry.PassRate = counts.rates()
	entry.Delta, entry.ConfidenceInterval = counts.pairedDelta()
	if heldOut.Samples != 0 {
		heldOut.PassRate /= float64(heldOut.Samples)
		entry.HeldOut = &heldOut
	}
	return entry
}

// rankEvaluationLeaderboard orders passing candidates first, then
// inconclusive, then failing ones. Only passing candidates join the Pareto
// frontier, and only other passing candidates can dominate them.
func rankEvaluationLeaderboard(entries []EvaluationLeaderboardEntry) {
	for i := range entries {
		entries[i].ParetoFrontier = entries[i].Verdict == EvaluationPass && !dominatedByPassingEntry(entries, i)
	}
	sort.Slice(entries, func(i, j int) bool {
		left, right := evaluationVerdictSeverity(entries[i].Verdict), evaluationVerdictSeverity(entries[j].Verdict)
		if left != right {
			return left < right
		}
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

func dominatedByPassingEntry(entries []EvaluationLeaderboardEntry, index int) bool {
	for j := range entries {
		if j != index && entries[j].Verdict == EvaluationPass && dominates(entries[j], entries[index]) {
			return true
		}
	}
	return false
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

func hasSelectionSlice(slices []EvaluationSlice) bool {
	for _, slice := range slices {
		if slice.Selection {
			return true
		}
	}
	return false
}
