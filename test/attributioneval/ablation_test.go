package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

const ablationTranscript = `{"role":"assistant","model":"m","tool_call":{"id":"c1","name":"grep"}}
{"role":"tool","tool_call":{"id":"c1","success":false}}
{"role":"assistant","model":"m","tool_call":{"id":"c2","name":"bash"}}
{"role":"tool","tool_call":{"id":"c2","success":true}}
`

func transcriptInput(spans map[string]string, names map[string]string) evalInput {
	input := evalInput{RunID: "run", Workflow: "wf", SpanData: spans}
	input.Events = append(input.Events, journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	for _, digest := range sortedKeys(names) {
		input.Events = append(input.Events, journal.Event{
			Type: journal.EventSpanRecorded, Stage: "implement", Attempt: 1, Name: names[digest],
			DataSchema: telemetry.GenAIEventSchema, Ref: &journal.Ref{Digest: digest},
		})
	}
	input.Events = append(input.Events,
		journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "failure"},
		journal.Event{Type: journal.EventRunFinished, Status: "failed"})
	return input
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

func TestAblateLevels(t *testing.T) {
	input := transcriptInput(
		map[string]string{"sha256:a": ablationTranscript, "sha256:b": `{"role":"assistant","model":"m"}` + "\n"},
		map[string]string{"sha256:a": "copilot-cli.transcript", "sha256:b": "claude-code.transcript"})

	full, change := ablate(input, fidelityFull)
	if change.any() || !reflect.DeepEqual(full.SpanData, input.SpanData) {
		t.Fatalf("full = %v, %+v; want the input unchanged", full.SpanData, change)
	}

	outcomes, change := ablate(input, fidelityOutcomeAblated)
	if change != (evidenceChange{TranscriptsAffected: 1, OutcomesRemoved: 2}) {
		t.Fatalf("outcome-ablated change = %+v", change)
	}
	if strings.Contains(outcomes.SpanData["sha256:a"], "success") {
		t.Fatalf("outcome-ablated transcript still records an outcome:\n%s", outcomes.SpanData["sha256:a"])
	}
	if strings.Count(outcomes.SpanData["sha256:a"], `"id":"c`) != 4 {
		t.Fatalf("outcome-ablated transcript lost tool identity:\n%s", outcomes.SpanData["sha256:a"])
	}

	truncated, change := ablate(input, fidelityResultsTruncated)
	want := strings.SplitN(ablationTranscript, "\n", 2)[0] + "\n" + truncationMarker + "\n"
	if truncated.SpanData["sha256:a"] != want || change != (evidenceChange{TranscriptsAffected: 1, RecordsDropped: 3}) {
		t.Fatalf("results-truncated = %q, %+v; want %q", truncated.SpanData["sha256:a"], change, want)
	}
	if truncated.SpanData["sha256:b"] != input.SpanData["sha256:b"] {
		t.Fatalf("a transcript without tool results changed under truncation: %q", truncated.SpanData["sha256:b"])
	}

	missing, change := ablate(input, fidelityTranscriptMissing)
	if len(missing.SpanData) != 0 || change != (evidenceChange{TranscriptsAffected: 2, TranscriptsWithheld: 2}) {
		t.Fatalf("transcript-missing = %v, %+v; want every transcript withheld", missing.SpanData, change)
	}
	if !reflect.DeepEqual(missing.Events, input.Events) || input.SpanData["sha256:a"] != ablationTranscript {
		t.Fatal("ablation modified the journal or the original input")
	}
}

// TestAblationNeverFabricatesObservations holds every level, on every
// fixture case, to removing or explicitly marking evidence: each retained
// record is the recorded one or the recorded one minus its outcome, and the
// only added record is the truncation marker.
func TestAblationNeverFabricatesObservations(t *testing.T) {
	for _, evalCase := range loadFixture(t).Cases {
		for _, level := range fidelityLevels {
			ablated, _ := ablate(evalCase.Input, level)
			if !reflect.DeepEqual(ablated.Events, evalCase.Input.Events) {
				t.Fatalf("%s/%s: journal changed", evalCase.ID, level)
			}
			for digest, content := range ablated.SpanData {
				original, ok := evalCase.Input.SpanData[digest]
				if !ok {
					t.Fatalf("%s/%s: span %s added", evalCase.ID, level, digest)
				}
				assertNoFabricatedRecords(t, evalCase.ID+"/"+string(level), original, content)
			}
		}
	}
}

func assertNoFabricatedRecords(t *testing.T, name, original, ablated string) {
	t.Helper()
	recorded := map[string]bool{}
	for _, line := range strings.Split(original, "\n") {
		recorded[canonicalRecord(t, line, false)] = true
		recorded[canonicalRecord(t, line, true)] = true
	}
	for _, line := range strings.Split(ablated, "\n") {
		if line == truncationMarker || strings.TrimSpace(line) == "" {
			continue
		}
		if !recorded[canonicalRecord(t, line, false)] {
			t.Fatalf("%s: record %s was not recorded", name, line)
		}
	}
}

// canonicalRecord re-encodes a record so key order does not matter;
// withoutOutcome drops a tool result's success field first.
func canonicalRecord(t *testing.T, line string, withoutOutcome bool) string {
	t.Helper()
	if strings.TrimSpace(line) == "" {
		return ""
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		return line
	}
	if call, ok := record["tool_call"].(map[string]any); ok && withoutOutcome {
		delete(call, "success")
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestEvidenceAblationOnTheEvalSet runs the ablation over the versioned eval
// set and holds the predeclared safety criteria: missing or lossy tool
// evidence must lead to abstention or the same cause, never to a different
// committed cause, more confidence, or citations the evidence cannot support.
func TestEvidenceAblationOnTheEvalSet(t *testing.T) {
	set := loadFixture(t)
	report, err := runAblation(set)
	if err != nil {
		t.Fatalf("runAblation: %v", err)
	}
	again, err := runAblation(set)
	if err != nil || !reflect.DeepEqual(report, again) {
		t.Fatalf("runAblation is not deterministic (err %v)", err)
	}
	if len(report.Variants) != len(fidelityLevels) || report.Variants[0].AffectedCases != 0 {
		t.Fatalf("variants = %+v, want one per level with full fidelity unchanged", report.Variants)
	}
	for _, variant := range report.Variants[1:] {
		if variant.AffectedCases == 0 || variant.AffectedAbstention.Count != variant.AffectedCases {
			t.Fatalf("%s: abstained on %d of %d affected cases, want every affected case to abstain",
				variant.Level, variant.AffectedAbstention.Count, variant.AffectedCases)
		}
	}
	verdicts := map[string]criterionVerdict{}
	for _, criterion := range report.Criteria {
		verdicts[criterion.ID] = criterion.Verdict
	}
	want := map[string]criterionVerdict{
		"full-matches-eval":        verdictPass,
		"no-silent-misattribution": verdictPass,
		"no-confidence-gain":       verdictPass,
		"citations-supported":      verdictPass,
		"outcome-evidence-useful":  verdictInsufficientSample,
	}
	if !reflect.DeepEqual(verdicts, want) {
		t.Fatalf("criteria = %v, want %v", verdicts, want)
	}
	coverage := report.Variants[0].Coverage
	if len(coverage) != 1 || coverage[0].Adapter != "copilot-cli" || coverage[0].ToolResultsWithOutcome == 0 {
		t.Fatalf("full-fidelity coverage = %+v, want the copilot-cli transcripts the set records", coverage)
	}
	if outcome, _ := report.variant(fidelityOutcomeAblated); outcome.Coverage[0].ToolResultsWithOutcome != 0 {
		t.Fatalf("outcome-ablated coverage = %+v, want no recorded outcome", outcome.Coverage)
	}
}

func TestRecordShiftFlagsMisattributionAndConfidenceGain(t *testing.T) {
	committed := func(class creditgraph.FailureClass, confidence float64) ablationCaseResult {
		return ablationCaseResult{caseResult: caseResult{Predicted: prediction{Attributed: true, Class: class, Confidence: confidence}}}
	}
	abstained := committed(creditgraph.ClassUnknown, 0.4)
	abstained.Abstained = true

	var variant ablationVariant
	variant.recordShift(committed(creditgraph.ClassModel, 0.5), committed(creditgraph.ClassBadToolResult, 0.3))
	variant.recordShift(committed(creditgraph.ClassModel, 0.5), abstained)
	variant.recordShift(abstained, committed(creditgraph.ClassModel, 0.1))
	variant.recordShift(committed(creditgraph.ClassModel, 0.2), committed(creditgraph.ClassModel, 0.3))
	want := confidenceShift{Lower: 1, Higher: 2, Abstained: 1}
	if variant.Misattributions != 2 || variant.ConfidenceShift != want {
		t.Fatalf("misattributions = %d, shift = %+v; want 2 and %+v", variant.Misattributions, variant.ConfidenceShift, want)
	}
}

// TestCitationOfAToolResultNeedsItsOutcome checks that a cause citing a tool
// result whose outcome the evidence no longer records is not supported.
func TestCitationOfAToolResultNeedsItsOutcome(t *testing.T) {
	input := transcriptInput(map[string]string{"sha256:a": ablationTranscript}, map[string]string{"sha256:a": "copilot-cli.transcript"})
	graph, attribution, err := attribute(input)
	if err != nil {
		t.Fatal(err)
	}
	cause := attribution.CausesOfClass(creditgraph.ClassBadToolResult)
	if len(cause) != 1 || checkCitations(graph, cause[0]) != (citationCheck{Total: 2, Valid: 2}) {
		t.Fatalf("full-fidelity bad-tool-result cause = %+v, want two valid citations", cause)
	}
	ablated, _ := ablate(input, fidelityOutcomeAblated)
	ablatedGraph, _, err := attribute(ablated)
	if err != nil {
		t.Fatal(err)
	}
	if check := checkCitations(ablatedGraph, cause[0]); check != (citationCheck{Total: 2}) {
		t.Fatalf("citation check without outcomes = %+v, want the outcome-less result invalid", check)
	}
}

func TestCoverageNamesAdaptersFromSpanNames(t *testing.T) {
	input := transcriptInput(
		map[string]string{"sha256:a": ablationTranscript},
		map[string]string{"sha256:a": "claude-code.transcript", "sha256:b": "implement.transcript"})
	collector := newCoverageCollector()
	if adapters := collector.add(input); !reflect.DeepEqual(adapters, []string{"claude-code", unidentifiedAdapter}) {
		t.Fatalf("adapters = %v", adapters)
	}
	collector.add(evalInput{})
	want := []adapterCoverage{
		{Adapter: "claude-code", Cases: 1, Transcripts: 1, TranscriptsWithContent: 1, ToolCalls: 2, ToolResults: 2, ToolResultsWithOutcome: 2},
		{Adapter: unidentifiedAdapter, Cases: 1, Transcripts: 1},
	}
	if got := collector.coverage(); !reflect.DeepEqual(got, want) || collector.casesWithoutTranscript != 1 {
		t.Fatalf("coverage = %+v (%d without transcript), want %+v", got, collector.casesWithoutTranscript, want)
	}
}

func TestRateAndMcNemar(t *testing.T) {
	if got := rateOf(5, 10); got != (rate{Count: 5, Total: 10, Rate: 0.5, Lower: 0.2366, Upper: 0.7634}) {
		t.Fatalf("rateOf(5,10) = %+v", got)
	}
	if got := rateOf(0, 0); got != (rate{}) {
		t.Fatalf("rateOf(0,0) = %+v, want zero", got)
	}
	for _, test := range []struct {
		lost, gained int
		want         float64
	}{{0, 0, 1}, {2, 0, 0.5}, {3, 3, 1}, {10, 0, 0.002}} {
		if got := round4(mcnemarExactP(test.lost, test.gained)); got != test.want {
			t.Fatalf("mcnemarExactP(%d,%d) = %v, want %v", test.lost, test.gained, got, test.want)
		}
	}
}

func TestRunAblationFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-ablation", "-update"}, &stdout, &stderr); code != 2 {
		t.Fatalf("-ablation -update exit = %d, want 2", code)
	}
	path := filepath.Join(t.TempDir(), "ablation.json")
	stdout.Reset()
	if code := run([]string{"-ablation", "-set", fixtureSetPath, "-report", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("-ablation exit = %d:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "criterion no-silent-misattribution") {
		t.Fatalf("stdout = %q, want the criteria", stdout.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report ablationReport
	if err := json.Unmarshal(data, &report); err != nil || report.Schema != ablationReportSchemaVersion {
		t.Fatalf("report = %+v, %v", report.Schema, err)
	}
	if bytes.Contains(data, []byte(`"tool_call"`)) || bytes.Contains(data, []byte(`"role"`)) {
		t.Fatal("ablation report carries transcript content")
	}
}
