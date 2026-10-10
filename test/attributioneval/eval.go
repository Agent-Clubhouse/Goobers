package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
)

const (
	evalSetSchemaVersion  = "goobers.dev/backprop/attribution-eval/v1"
	reportSchemaVersion   = "goobers.dev/backprop/attribution-eval-report/v1"
	baselineSchemaVersion = "goobers.dev/backprop/attribution-eval-baseline/v1"
	// calibrationBins is the number of equal-width confidence bins.
	calibrationBins = 5
	// accuracyTolerance absorbs float rounding when comparing to a baseline.
	accuracyTolerance = 1e-9
)

// labelSource names where an eval case's expected attribution came from.
type labelSource string

const (
	// labelSynthetic is a constructed journal whose failure was injected at a
	// known stage and step, so its label is known by construction.
	labelSynthetic labelSource = "synthetic"
	// labelHuman is an operator's annotation of a real failed run.
	labelHuman labelSource = "human"
	// labelFixMarker is derived from a resolved fix marker: the fix that
	// recovered the failure locates the responsible stage and domain.
	labelFixMarker labelSource = "fix-marker"
)

// granularity names one level at which attribution is scored.
type granularity string

const (
	granularityDomain   granularity = "domain"
	granularityWorkflow granularity = "workflow"
	granularityStage    granularity = "stage"
	granularityStep     granularity = "step"
	granularityClass    granularity = "class"
)

// granularities are the scored levels, coarsest first.
var granularities = []granularity{granularityDomain, granularityWorkflow, granularityStage, granularityStep, granularityClass}

// evalSet is a versioned, labeled set of failed runs. Cases carry their
// journal inline so a run over the set is deterministic and needs no run
// directory.
type evalSet struct {
	Schema  string     `json:"schema"`
	Version string     `json:"version"`
	Cases   []evalCase `json:"cases"`
}

type evalCase struct {
	ID          string    `json:"id"`
	Description string    `json:"description,omitempty"`
	Input       evalInput `json:"input"`
	Expected    evalLabel `json:"expected"`
}

// evalInput is the journal and span content attribution reads. SpanData holds
// recorded span content keyed by content digest.
type evalInput struct {
	RunID    string            `json:"runId"`
	Gaggle   string            `json:"gaggle,omitempty"`
	Workflow string            `json:"workflow"`
	Events   []journal.Event   `json:"events"`
	SpanData map[string]string `json:"spanData,omitempty"`
}

// evalLabel is the ground-truth attribution of one failed run. Workflow and
// Stage are empty when no stage of the run is responsible (for example a run
// that failed with every stage passing). Step is the decisive graph node ID.
type evalLabel struct {
	Source   labelSource              `json:"source"`
	Domain   creditgraph.FaultDomain  `json:"domain"`
	Workflow string                   `json:"workflow,omitempty"`
	Stage    string                   `json:"stage,omitempty"`
	Step     string                   `json:"step"`
	Class    creditgraph.FailureClass `json:"class"`
	MAST     mastCategory             `json:"mast"`
}

// prediction is what attribution blamed for one case: its most confident
// cause, projected onto the label's granularities. Attributed is false when
// attribution produced no cause at all.
type prediction struct {
	Attributed bool                     `json:"attributed"`
	Domain     creditgraph.FaultDomain  `json:"domain,omitempty"`
	Workflow   string                   `json:"workflow,omitempty"`
	Stage      string                   `json:"stage,omitempty"`
	Step       string                   `json:"step,omitempty"`
	Class      creditgraph.FailureClass `json:"class,omitempty"`
	MAST       mastCategory             `json:"mast,omitempty"`
	Confidence float64                  `json:"confidence"`
}

type caseResult struct {
	CaseID    string        `json:"caseId"`
	Expected  evalLabel     `json:"expected"`
	Predicted prediction    `json:"predicted"`
	Correct   []granularity `json:"correct"`
}

type accuracyEntry struct {
	Granularity granularity `json:"granularity"`
	Correct     int         `json:"correct"`
	Total       int         `json:"total"`
	Accuracy    float64     `json:"accuracy"`
}

// calibrationBin summarizes predictions whose confidence fell in
// [Lower,Upper); the last bin includes 1.
type calibrationBin struct {
	Lower          float64 `json:"lower"`
	Upper          float64 `json:"upper"`
	Count          int     `json:"count"`
	MeanConfidence float64 `json:"meanConfidence"`
	Accuracy       float64 `json:"accuracy"`
}

// calibration measures how well cause confidence predicts a correct
// attribution at Target granularity.
type calibration struct {
	Target                   granularity      `json:"target"`
	BrierScore               float64          `json:"brierScore"`
	ExpectedCalibrationError float64          `json:"expectedCalibrationError"`
	Bins                     []calibrationBin `json:"bins"`
}

type evalReport struct {
	Schema         string          `json:"schema"`
	EvalSetVersion string          `json:"evalSetVersion"`
	EvalSetDigest  string          `json:"evalSetDigest"`
	CaseCount      int             `json:"caseCount"`
	Unattributed   int             `json:"unattributed"`
	Taxonomy       []taxonomyEntry `json:"taxonomy"`
	Accuracy       []accuracyEntry `json:"accuracy"`
	Calibration    calibration     `json:"calibration"`
	Results        []caseResult    `json:"results"`
}

// evalBaseline records the accuracy floors the regression guard holds one
// exact eval set (version and content digest) to.
type evalBaseline struct {
	Schema         string          `json:"schema"`
	EvalSetVersion string          `json:"evalSetVersion"`
	EvalSetDigest  string          `json:"evalSetDigest"`
	Accuracy       []accuracyEntry `json:"accuracy"`
}

func loadEvalSet(path string) (evalSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return evalSet{}, err
	}
	var set evalSet
	if err := json.Unmarshal(data, &set); err != nil {
		return evalSet{}, fmt.Errorf("decode attribution eval set: %w", err)
	}
	if err := set.validate(); err != nil {
		return evalSet{}, err
	}
	return set, nil
}

func loadBaseline(path string) (evalBaseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return evalBaseline{}, err
	}
	var baseline evalBaseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return evalBaseline{}, fmt.Errorf("decode attribution eval baseline: %w", err)
	}
	return baseline, nil
}

// validate rejects a set whose schema, version, case IDs, or labels are not
// well formed. A label's MAST category must agree with its class's taxonomy
// entry so the vocabulary has one meaning.
func (set evalSet) validate() error {
	if set.Schema != evalSetSchemaVersion {
		return fmt.Errorf("unsupported attribution eval set schema %q", set.Schema)
	}
	if strings.TrimSpace(set.Version) == "" {
		return errors.New("attribution eval set version is required")
	}
	if len(set.Cases) == 0 {
		return errors.New("attribution eval set has no cases")
	}
	seen := map[string]bool{}
	for _, evalCase := range set.Cases {
		if strings.TrimSpace(evalCase.ID) == "" {
			return errors.New("attribution eval case ID is required")
		}
		if seen[evalCase.ID] {
			return fmt.Errorf("duplicate attribution eval case %q", evalCase.ID)
		}
		seen[evalCase.ID] = true
		if err := evalCase.Expected.validate(); err != nil {
			return fmt.Errorf("attribution eval case %q: %w", evalCase.ID, err)
		}
	}
	return nil
}

func (label evalLabel) validate() error {
	switch label.Source {
	case labelSynthetic, labelHuman, labelFixMarker:
	default:
		return fmt.Errorf("unsupported label source %q", label.Source)
	}
	switch label.Domain {
	case creditgraph.FaultDomainProductRuntime, creditgraph.FaultDomainExternal,
		creditgraph.FaultDomainWorkflow, creditgraph.FaultDomainUnknown:
	default:
		return fmt.Errorf("unsupported fault domain %q", label.Domain)
	}
	if !knownFailureClass(label.Class) {
		return fmt.Errorf("unsupported failure class %q", label.Class)
	}
	if want := taxonomyOf(label.Class).MAST; label.MAST != want {
		return fmt.Errorf("MAST category %q does not match class %q (want %q)", label.MAST, label.Class, want)
	}
	if strings.TrimSpace(label.Step) == "" {
		return errors.New("expected step is required")
	}
	if label.Stage != "" && label.Workflow == "" {
		return errors.New("a labeled stage requires its workflow")
	}
	return nil
}

// digest is a content digest of the set, independent of case order, so a
// report and baseline name exactly which labels they scored.
func (set evalSet) digest() string {
	set.Cases = sortedCases(set.Cases)
	data, _ := json.Marshal(set)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sortedCases(cases []evalCase) []evalCase {
	sorted := append([]evalCase(nil), cases...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return sorted
}

// runEval builds and attributes every case's graph and scores the most
// confident cause against its label. Results are ordered by case ID, so the
// report is deterministic for a fixed set.
func runEval(set evalSet) (evalReport, error) {
	if err := set.validate(); err != nil {
		return evalReport{}, err
	}
	cases := sortedCases(set.Cases)
	results := make([]caseResult, 0, len(cases))
	for _, evalCase := range cases {
		predicted, err := predict(evalCase.Input)
		if err != nil {
			return evalReport{}, fmt.Errorf("attribution eval case %q: %w", evalCase.ID, err)
		}
		results = append(results, score(evalCase, predicted))
	}
	return evalReport{
		Schema:         reportSchemaVersion,
		EvalSetVersion: set.Version,
		EvalSetDigest:  set.digest(),
		CaseCount:      len(results),
		Unattributed:   unattributed(results),
		Taxonomy:       fullTaxonomy(),
		Accuracy:       accuracyOf(results),
		Calibration:    calibrate(results, granularityStep),
		Results:        results,
	}, nil
}

// fullTaxonomy is the label vocabulary the report was scored against, in
// class order.
func fullTaxonomy() []taxonomyEntry {
	entries := make([]taxonomyEntry, 0, len(failureClassMAST))
	for class := range failureClassMAST {
		entries = append(entries, taxonomyOf(class))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Class < entries[j].Class })
	return entries
}

func predict(input evalInput) (prediction, error) {
	spans := make(map[string][]byte, len(input.SpanData))
	for digest, content := range input.SpanData {
		spans[digest] = []byte(content)
	}
	graph, err := creditgraph.Build(creditgraph.Input{
		RunID: input.RunID, Gaggle: input.Gaggle, Workflow: input.Workflow,
		Events: input.Events, SpanData: spans,
	})
	if err != nil {
		return prediction{}, err
	}
	cause, ok := topCause(creditgraph.Attribute(graph).Causes)
	if !ok {
		return prediction{}, nil
	}
	predicted := prediction{
		Attributed: true,
		Domain:     creditgraph.CauseFaultDomain(cause),
		Stage:      cause.Stage,
		Step:       cause.NodeID,
		Class:      cause.Class,
		MAST:       taxonomyOf(cause.Class).MAST,
		Confidence: cause.Confidence,
	}
	if cause.Stage != "" {
		predicted.Workflow = input.Workflow
	}
	return predicted, nil
}

// topCause is the most confident cause; ties keep graph order.
func topCause(causes []creditgraph.CauseFinding) (creditgraph.CauseFinding, bool) {
	if len(causes) == 0 {
		return creditgraph.CauseFinding{}, false
	}
	best := causes[0]
	for _, cause := range causes[1:] {
		if cause.Confidence > best.Confidence {
			best = cause
		}
	}
	return best, true
}

// score credits each granularity the prediction got right. A case with no
// cause earns no credit anywhere, so blaming nothing never matches a
// run-level label's empty workflow and stage.
func score(evalCase evalCase, predicted prediction) caseResult {
	expected := evalCase.Expected
	if !predicted.Attributed {
		return caseResult{CaseID: evalCase.ID, Expected: expected, Predicted: predicted, Correct: []granularity{}}
	}
	hits := map[granularity]bool{
		granularityDomain:   predicted.Domain == expected.Domain,
		granularityWorkflow: predicted.Workflow == expected.Workflow,
		granularityStage:    predicted.Stage == expected.Stage,
		granularityStep:     predicted.Step == expected.Step,
		granularityClass:    predicted.Class == expected.Class,
	}
	correct := []granularity{}
	for _, level := range granularities {
		if hits[level] {
			correct = append(correct, level)
		}
	}
	return caseResult{CaseID: evalCase.ID, Expected: expected, Predicted: predicted, Correct: correct}
}

func unattributed(results []caseResult) int {
	count := 0
	for _, result := range results {
		if !result.Predicted.Attributed {
			count++
		}
	}
	return count
}

func (result caseResult) hit(level granularity) bool {
	for _, correct := range result.Correct {
		if correct == level {
			return true
		}
	}
	return false
}

func accuracyOf(results []caseResult) []accuracyEntry {
	entries := make([]accuracyEntry, 0, len(granularities))
	for _, level := range granularities {
		entry := accuracyEntry{Granularity: level, Total: len(results)}
		for _, result := range results {
			if result.hit(level) {
				entry.Correct++
			}
		}
		if entry.Total > 0 {
			entry.Accuracy = round4(float64(entry.Correct) / float64(entry.Total))
		}
		entries = append(entries, entry)
	}
	return entries
}

// calibrate reports the Brier score and expected calibration error of cause
// confidence against correctness at target granularity. A case with no cause
// predicts with confidence 0.
func calibrate(results []caseResult, target granularity) calibration {
	summary := calibration{Target: target, Bins: make([]calibrationBin, calibrationBins)}
	confidenceSums := make([]float64, calibrationBins)
	hitCounts := make([]int, calibrationBins)
	for index := range summary.Bins {
		summary.Bins[index].Lower = round4(float64(index) / calibrationBins)
		summary.Bins[index].Upper = round4(float64(index+1) / calibrationBins)
	}
	var brier float64
	for _, result := range results {
		confidence := math.Max(0, math.Min(1, result.Predicted.Confidence))
		bin := min(int(confidence*calibrationBins), calibrationBins-1)
		outcome := 0.0
		if result.hit(target) {
			outcome = 1
			hitCounts[bin]++
		}
		brier += (confidence - outcome) * (confidence - outcome)
		confidenceSums[bin] += confidence
		summary.Bins[bin].Count++
	}
	if len(results) == 0 {
		return summary
	}
	var ece float64
	for index := range summary.Bins {
		bin := &summary.Bins[index]
		if bin.Count == 0 {
			continue
		}
		mean := confidenceSums[index] / float64(bin.Count)
		accuracy := float64(hitCounts[index]) / float64(bin.Count)
		bin.MeanConfidence = round4(mean)
		bin.Accuracy = round4(accuracy)
		ece += float64(bin.Count) / float64(len(results)) * math.Abs(accuracy-mean)
	}
	summary.BrierScore = round4(brier / float64(len(results)))
	summary.ExpectedCalibrationError = round4(ece)
	return summary
}

func round4(value float64) float64 {
	return math.Round(value*1e4) / 1e4
}

// baseline records this report's accuracy as the floors later runs of the
// same eval set are held to.
func (report evalReport) baseline() evalBaseline {
	return evalBaseline{
		Schema:         baselineSchemaVersion,
		EvalSetVersion: report.EvalSetVersion,
		EvalSetDigest:  report.EvalSetDigest,
		Accuracy:       append([]accuracyEntry(nil), report.Accuracy...),
	}
}

// regressions lists every way the report falls short of the baseline: a
// different eval set version or content, a granularity the baseline does not
// record, or an accuracy below its recorded floor. Empty means no regression.
func (report evalReport) regressions(baseline evalBaseline) []string {
	if baseline.Schema != baselineSchemaVersion {
		return []string{fmt.Sprintf("unsupported attribution eval baseline schema %q", baseline.Schema)}
	}
	if baseline.EvalSetVersion != report.EvalSetVersion || baseline.EvalSetDigest != report.EvalSetDigest {
		return []string{fmt.Sprintf("baseline records eval set %s (%s), report scored %s (%s); relabeling requires a new baseline",
			baseline.EvalSetVersion, baseline.EvalSetDigest, report.EvalSetVersion, report.EvalSetDigest)}
	}
	floors := map[granularity]float64{}
	for _, entry := range baseline.Accuracy {
		floors[entry.Granularity] = entry.Accuracy
	}
	var regressions []string
	for _, entry := range report.Accuracy {
		floor, ok := floors[entry.Granularity]
		switch {
		case !ok:
			regressions = append(regressions, fmt.Sprintf("%s: no recorded baseline", entry.Granularity))
		case entry.Accuracy+accuracyTolerance < floor:
			regressions = append(regressions, fmt.Sprintf("%s: accuracy %.4f below baseline %.4f", entry.Granularity, entry.Accuracy, floor))
		}
	}
	return regressions
}
