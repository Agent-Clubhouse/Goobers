package main

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/creditgraph"
)

var (
	fixtureSetPath      = filepath.Join("testdata", "v1.json")
	fixtureBaselinePath = filepath.Join("testdata", "v1.baseline.json")
)

func loadFixture(t *testing.T) evalSet {
	t.Helper()
	set, err := loadEvalSet(fixtureSetPath)
	if err != nil {
		t.Fatalf("loadEvalSet: %v", err)
	}
	return set
}

// TestAttributionEvalRegressionGuard is the CI regression guard: attribution
// accuracy on the versioned eval set must not fall below the recorded
// baseline at any granularity. Record an intended change with
// `go run ./test/attributioneval -update`.
func TestAttributionEvalRegressionGuard(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-set", fixtureSetPath, "-baseline", fixtureBaselinePath}, &stdout, &stderr)
	t.Log(stdout.String())
	if code != 0 {
		t.Fatalf("attributioneval exited %d:\n%s", code, stderr.String())
	}
}

func TestRunFailsWhenAccuracyDropsBelowBaseline(t *testing.T) {
	report, err := evaluate(fixtureSetPath)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	raised := report.baseline()
	raised.Accuracy[0].Accuracy = 1.01
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := writeJSON(path, raised); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-set", fixtureSetPath, "-baseline", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1 for a regression", code)
	}
	if !strings.Contains(stderr.String(), "regression: domain") {
		t.Fatalf("stderr = %q, want the domain regression named", stderr.String())
	}
}

// TestRunEvalIsDeterministic pins that a fixed set yields the same report
// regardless of run or case order.
func TestRunEvalIsDeterministic(t *testing.T) {
	set := loadFixture(t)
	first, err := runEval(set)
	if err != nil {
		t.Fatalf("runEval: %v", err)
	}
	reversed := set
	reversed.Cases = append([]evalCase(nil), set.Cases...)
	for i, j := 0, len(reversed.Cases)-1; i < j; i, j = i+1, j-1 {
		reversed.Cases[i], reversed.Cases[j] = reversed.Cases[j], reversed.Cases[i]
	}
	second, err := runEval(reversed)
	if err != nil {
		t.Fatalf("runEval: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("reports differ across case order:\n%+v\n%+v", first, second)
	}
	if first.CaseCount != len(set.Cases) || len(first.Results) != len(set.Cases) {
		t.Fatalf("report scored %d cases, want %d", first.CaseCount, len(set.Cases))
	}
}

// TestRunEvalScoresMisattributionPerGranularity checks that a misattributed
// case is scored independently at each granularity: blaming the right stage
// node for the wrong reason earns stage and step credit but not domain or
// class credit.
func TestRunEvalScoresMisattributionPerGranularity(t *testing.T) {
	report, err := runEval(loadFixture(t))
	if err != nil {
		t.Fatalf("runEval: %v", err)
	}
	var masked *caseResult
	for i := range report.Results {
		if report.Results[i].CaseID == "harness-error-masks-empty-prompt" {
			masked = &report.Results[i]
		}
	}
	if masked == nil {
		t.Fatalf("eval set lost its misattribution case")
	}
	want := []granularity{granularityWorkflow, granularityStage, granularityStep}
	if !reflect.DeepEqual(masked.Correct, want) {
		t.Fatalf("correct = %v, want %v (predicted %+v)", masked.Correct, want, masked.Predicted)
	}
	if masked.Predicted.MAST != mastOutsideAgentSystem || masked.Expected.MAST != mastSpecification {
		t.Fatalf("MAST predicted/expected = %q/%q", masked.Predicted.MAST, masked.Expected.MAST)
	}
}

func TestAccuracyAndCalibration(t *testing.T) {
	label := evalLabel{Source: labelHuman, Domain: creditgraph.FaultDomainWorkflow, Workflow: "wf",
		Stage: "implement", Step: "stage:implement#1", Class: creditgraph.ClassWeakInstructions, MAST: mastSpecification}
	hit := prediction{Attributed: true, Domain: creditgraph.FaultDomainWorkflow, Workflow: "wf", Stage: "implement",
		Step: "stage:implement#1", Class: creditgraph.ClassWeakInstructions, Confidence: 0.9}
	miss := prediction{Attributed: true, Domain: creditgraph.FaultDomainExternal, Workflow: "wf", Stage: "implement",
		Step: "stage:implement#1", Class: creditgraph.ClassEnvironment, Confidence: 0.85}
	results := []caseResult{
		score(evalCase{ID: "a", Expected: label}, hit),
		score(evalCase{ID: "b", Expected: label}, miss),
		score(evalCase{ID: "c", Expected: label}, prediction{}),
	}
	accuracy := map[granularity]accuracyEntry{}
	for _, entry := range accuracyOf(results) {
		accuracy[entry.Granularity] = entry
	}
	for level, want := range map[granularity]int{granularityDomain: 1, granularityWorkflow: 2, granularityStage: 2, granularityStep: 2, granularityClass: 1} {
		if got := accuracy[level]; got.Correct != want || got.Total != 3 {
			t.Fatalf("%s = %+v, want %d/3", level, got, want)
		}
	}
	summary := calibrate(results, granularityClass)
	// Class hits: a (0.9) only. Brier = (0.1^2 + 0.85^2 + 0^2)/3.
	if summary.BrierScore != round4((0.01+0.7225)/3) {
		t.Fatalf("brier = %v", summary.BrierScore)
	}
	// Bin [0,0.2): c at confidence 0, accuracy 0. Bin [0.8,1]: a and b, mean
	// 0.875, accuracy 0.5.
	if summary.ExpectedCalibrationError != round4(2.0/3*0.375) {
		t.Fatalf("ece = %v", summary.ExpectedCalibrationError)
	}
	if top := summary.Bins[calibrationBins-1]; top.Count != 2 || top.MeanConfidence != 0.875 || top.Accuracy != 0.5 {
		t.Fatalf("top bin = %+v", top)
	}
	if bottom := summary.Bins[0]; bottom.Count != 1 || bottom.Accuracy != 0 {
		t.Fatalf("bottom bin = %+v", bottom)
	}
}

// TestScoreGivesNoCreditWithoutACause keeps an empty prediction from matching
// a run-level label's empty workflow and stage.
func TestScoreGivesNoCreditWithoutACause(t *testing.T) {
	runLevel := evalLabel{Source: labelSynthetic, Domain: creditgraph.FaultDomainUnknown, Step: "outcome",
		Class: creditgraph.ClassUnknown, MAST: mastUnknown}
	result := score(evalCase{ID: "none", Expected: runLevel}, prediction{})
	if len(result.Correct) != 0 {
		t.Fatalf("correct = %v, want no credit for an unattributed case", result.Correct)
	}
	if got := unattributed([]caseResult{result}); got != 1 {
		t.Fatalf("unattributed = %d, want 1", got)
	}
}

func TestRegressions(t *testing.T) {
	report := evalReport{EvalSetVersion: "v1", EvalSetDigest: "sha256:set", Accuracy: []accuracyEntry{
		{Granularity: granularityDomain, Accuracy: 0.5}, {Granularity: granularityStep, Accuracy: 0.75},
	}}
	baseline := report.baseline()
	if got := report.regressions(baseline); len(got) != 0 {
		t.Fatalf("report regresses against its own baseline: %v", got)
	}
	dropped := report
	dropped.Accuracy = []accuracyEntry{{Granularity: granularityDomain, Accuracy: 0.5}, {Granularity: granularityStep, Accuracy: 0.5}}
	if got := dropped.regressions(baseline); len(got) != 1 || !strings.Contains(got[0], "step") {
		t.Fatalf("regressions = %v, want only the step drop", got)
	}
	improved := report
	improved.Accuracy = []accuracyEntry{{Granularity: granularityDomain, Accuracy: 1}, {Granularity: granularityStep, Accuracy: 1}}
	if got := improved.regressions(baseline); len(got) != 0 {
		t.Fatalf("improvement reported as regression: %v", got)
	}
	partial := baseline
	partial.Accuracy = baseline.Accuracy[:1]
	if got := report.regressions(partial); len(got) != 1 || !strings.Contains(got[0], "no recorded baseline") {
		t.Fatalf("regressions = %v, want the unrecorded granularity flagged", got)
	}
	relabeled := baseline
	relabeled.EvalSetDigest = "sha256:relabeled"
	if got := report.regressions(relabeled); len(got) != 1 || !strings.Contains(got[0], "relabeling") {
		t.Fatalf("regressions = %v, want an eval set content mismatch", got)
	}
}

func TestValidateRejectsMalformedSets(t *testing.T) {
	valid := evalLabel{Source: labelFixMarker, Domain: creditgraph.FaultDomainWorkflow, Workflow: "wf",
		Stage: "implement", Step: "stage:implement#1", Class: creditgraph.ClassTopology, MAST: mastInterAgentMisalignment}
	set := func(labels ...evalLabel) evalSet {
		result := evalSet{Schema: evalSetSchemaVersion, Version: "test"}
		for i, label := range labels {
			result.Cases = append(result.Cases, evalCase{ID: string(rune('a' + i)), Expected: label})
		}
		return result
	}
	if err := set(valid).validate(); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}
	mutate := func(edit func(*evalLabel)) evalLabel {
		label := valid
		edit(&label)
		return label
	}
	tests := map[string]evalSet{
		"mast mismatch":          set(mutate(func(l *evalLabel) { l.MAST = mastSpecification })),
		"unknown class":          set(mutate(func(l *evalLabel) { l.Class = "guess" })),
		"unknown domain":         set(mutate(func(l *evalLabel) { l.Domain = "elsewhere" })),
		"unknown source":         set(mutate(func(l *evalLabel) { l.Source = "rumor" })),
		"missing step":           set(mutate(func(l *evalLabel) { l.Step = "" })),
		"stage without workflow": set(mutate(func(l *evalLabel) { l.Workflow = "" })),
		"no cases":               set(),
	}
	duplicate := set(valid, valid)
	duplicate.Cases[1].ID = duplicate.Cases[0].ID
	tests["duplicate id"] = duplicate
	wrongSchema := set(valid)
	wrongSchema.Schema = "v0"
	tests["wrong schema"] = wrongSchema
	for name, candidate := range tests {
		if err := candidate.validate(); err == nil {
			t.Errorf("%s: validate accepted a malformed set", name)
		}
	}
}
