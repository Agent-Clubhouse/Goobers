package workflowcluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

var behaviorEpoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// syntheticRun is a parent run that loops implement→review→implement, retries
// review, escalates once at a gate, and merges one child workflow result.
func syntheticRun(names [2]string) BehaviorInput {
	implement, review := names[0], names[1]
	at := func(s int) time.Time { return behaviorEpoch.Add(time.Duration(s) * time.Second) }
	childWait := map[string]any{"version": 1, "request": map[string]any{"childRunId": "child-1", "action": "merge"}}
	return BehaviorInput{
		Identity: journal.RunIdentity{
			RunID: "run-1", Gaggle: "g", Workflow: "wf", WorkflowDigest: "sha256:wf", GooberDigest: "sha256:gb",
			RunControls: &apiv1.RunControls{MaxRunDuration: "100s", MaxRepasses: 2},
		},
		Events: []journal.Event{
			{Seq: 1, Type: journal.EventRunStarted, Time: at(0)},
			{Seq: 2, Type: journal.EventStageStarted, Stage: implement, Attempt: 1, Time: at(1)},
			{Seq: 3, Type: journal.EventStageStarted, Stage: review, Attempt: 1, Time: at(2)},
			{Seq: 4, Type: journal.EventStageStarted, Stage: review, Attempt: 2, AttemptClass: journal.AttemptInfra, Time: at(3)},
			{Seq: 5, Type: journal.EventGateEvaluated, Gate: "verdict", Escalated: true, Runner: map[string]any{"repassAttempt": 2}, Time: at(4)},
			{Seq: 6, Type: journal.EventStageStarted, Stage: implement, Attempt: 1, Time: at(5)},
			{Seq: 7, Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": journal.ChildWaitKind, "childWait": childWait}, Time: at(6)},
			{Seq: 8, Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted), Time: at(90)},
		},
		Spans: []telemetry.SpanRecord{
			{Kind: telemetry.SpanKindTask, Attributes: map[string]string{
				telemetry.AttrModel: "m", telemetry.AttrHarnessVersion: "copilot-1", telemetry.AttrGenAIUsageInputTokens: "100",
				telemetry.AttrGenAIUsageOutputTokens: "10", telemetry.AttrUsageCostUSD: "0.5",
			}},
			{Kind: telemetry.SpanKindRun, Attributes: map[string]string{telemetry.AttrGenAIUsageInputTokens: "100"}},
		},
	}
}

func TestExtractBehaviorIsDeterministicAndNameFree(t *testing.T) {
	first, err := ExtractBehavior(syntheticRun([2]string{"implement", "review"}))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := ExtractBehavior(syntheticRun([2]string{"implement", "review"}))
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(again)
	if !bytes.Equal(a, b) {
		t.Fatalf("descriptor is not byte-identical:\n%s\n%s", a, b)
	}
	renamed, _ := ExtractBehavior(syntheticRun([2]string{"build", "inspect"}))
	r, _ := json.Marshal(renamed.Numeric)
	n, _ := json.Marshal(first.Numeric)
	if !bytes.Equal(r, n) {
		t.Fatalf("stage names changed the descriptor:\n%s\n%s", n, r)
	}
	if strings.Contains(string(a), "implement") || strings.Contains(string(a), "verdict") {
		t.Fatalf("descriptor leaks stage or gate names: %s", a)
	}

	want := map[string]float64{
		"outcome.completed": 1, "outcome.failed": 0, "visits.total": 4, "stages.distinct": 2,
		"visits.ngram.start>new": 1, "visits.ngram.new>new": 1, "visits.ngram.new>retry": 1,
		"visits.ngram.retry>loop": 1, "visits.loop": 1, "retries.infra": 1, "retries.policy": 0,
		"attempts.max": 2, "gates.evaluated": 1, "gates.escalated": 1, "repass.max": 2,
		"nearMiss.repass": 1, "nearMiss.timeout": 1, "children.spawned": 1, "children.merge": 1,
		"child.generated": 0, "duration.log1pSeconds": math.Log1p(90),
		"usage.log1pInputTokens": math.Log1p(100), "usage.log1pCostCents": math.Log1p(50),
	}
	for name, value := range want {
		if got, ok := first.Numeric[name]; !ok || math.Abs(got-value) > 1e-9 {
			t.Errorf("%s = %v (present %t), want %v", name, got, ok, value)
		}
	}
	if first.Version != BehaviorFeatureVersion || first.Outcome != journal.PhaseCompleted || first.EffectiveVersion == "" {
		t.Fatalf("descriptor header = %+v", first)
	}
	if first.Provenance[StratifyModel] != "m" || first.Provenance[StratifyHarness] != "copilot-1" ||
		first.Provenance[StratifySkills] != "" || first.Provenance[StratifyTools] != "" {
		t.Fatalf("provenance = %+v", first.Provenance)
	}
}

func TestExtractBehaviorLinksChildRunLineage(t *testing.T) {
	in := syntheticRun([2]string{"a", "b"})
	in.Identity.Child = &journal.ChildLineage{ParentRunID: "parent-1", ParentWorkflow: "wf", SourceDigest: "sha256:proposal"}
	d, err := ExtractBehavior(in)
	if err != nil {
		t.Fatal(err)
	}
	if d.Lineage == nil || d.Lineage.ParentRunID != "parent-1" || d.Lineage.SourceDigest != "sha256:proposal" || d.Numeric["child.generated"] != 1 {
		t.Fatalf("child lineage = %+v, generated = %v", d.Lineage, d.Numeric["child.generated"])
	}
}

func TestExtractBehaviorExcludesUndefinedAndNonTerminalRuns(t *testing.T) {
	noDigest := syntheticRun([2]string{"a", "b"})
	noDigest.Identity.WorkflowDigest = ""
	mixed := syntheticRun([2]string{"a", "b"})
	mixed.Spans = append(mixed.Spans, telemetry.SpanRecord{Kind: telemetry.SpanKindTask, Attributes: map[string]string{telemetry.AttrModel: "other"}})
	running := syntheticRun([2]string{"a", "b"})
	running.Events = running.Events[:len(running.Events)-1]

	for name, tc := range map[string]struct {
		in   BehaviorInput
		want error
	}{
		"no workflow digest": {noDigest, ErrUndefinedProvenance},
		"mixed models":       {mixed, ErrUndefinedProvenance},
		"running":            {running, ErrRunNotTerminal},
	} {
		if _, err := ExtractBehavior(tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestSummarizeBehaviorDistributionsAndMinSamples(t *testing.T) {
	descriptor := func(run, workflow string, visits float64) BehaviorDescriptor {
		return BehaviorDescriptor{RunID: run, Gaggle: "g", Workflow: workflow, EffectiveVersion: "sha256:0123456789abcdef0123",
			Numeric: map[string]float64{"visits.total": visits}}
	}
	ds := []BehaviorDescriptor{
		descriptor("r4", "a", 4), descriptor("r1", "a", 1), descriptor("r3", "a", 3), descriptor("r2", "a", 2),
		descriptor("r5", "b", 9),
	}
	got := SummarizeBehavior(ds, 2)
	if len(got) != 1 || got[0].ID != "g/a@0123456789abcdef" || got[0].SampleCount != 4 {
		t.Fatalf("summaries = %+v", got)
	}
	if d := got[0].Features["visits.total"]; d != (Distribution{Mean: 2.5, P50: 2, P90: 4, Variance: 1.25}) {
		t.Fatalf("distribution = %+v", d)
	}
	reversed := SummarizeBehavior([]BehaviorDescriptor{ds[4], ds[3], ds[2], ds[1], ds[0]}, 2)
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(reversed)
	if !bytes.Equal(a, b) {
		t.Fatal("summary depends on input order")
	}
	if p := BehaviorPoint(got[0]); p.Numeric["visits.total.mean"] != 2.5 || p.Numeric["visits.total.p90"] != 4 {
		t.Fatalf("point = %+v", p)
	}
}

// writeJournal records a synthetic terminal run under the instance root.
func writeJournal(t *testing.T, root, gaggle, runID, workflow, digest string, visits int, status journal.RunPhase) string {
	t.Helper()
	runsDir := instance.NewLayout(root).ForGaggle(gaggle).RunsDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Gaggle: gaggle, Workflow: workflow, WorkflowVersion: 1, WorkflowDigest: digest,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range visits {
		if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "s", Attempt: i + 1}); err != nil {
			t.Fatal(err)
		}
	}
	if status != journal.PhaseRunning {
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(status), Disposition: "produced"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(runsDir, runID)
}

func TestScanBehaviorReadsJournalsReadOnlyAndAccountsExclusions(t *testing.T) {
	root := t.TempDir()
	dir := writeJournal(t, root, "g", "run-a1", "a", "sha256:a", 1, journal.PhaseCompleted)
	writeJournal(t, root, "g", "run-a2", "a", "sha256:a", 2, journal.PhaseFailed)
	writeJournal(t, root, "h", "run-b1", "b", "sha256:b", 3, journal.PhaseCompleted)
	writeJournal(t, root, "h", "run-legacy", "b", "", 1, journal.PhaseCompleted)
	writeJournal(t, root, "h", "run-live", "b", "sha256:b", 1, journal.PhaseRunning)
	before, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	scan, err := ScanBehavior(root, BehaviorScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if scan.Candidates != 5 || len(scan.Descriptors) != 3 || scan.UndefinedProvenance != 1 || scan.NonTerminal != 1 {
		t.Fatalf("scan = %+v", scan)
	}
	if scan.Descriptors[0].RunID != "run-a1" || scan.Descriptors[2].Numeric["visits.total"] != 3 {
		t.Fatalf("descriptors = %+v", scan.Descriptors)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if !bytes.Equal(before, after) {
		t.Fatal("scan modified a journal")
	}

	capped, err := ScanBehavior(root, BehaviorScanOptions{MaxRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if capped.OverLimit != 3 || len(capped.Descriptors)+capped.NonTerminal+capped.UndefinedProvenance != 2 {
		t.Fatalf("capped scan = %+v", capped)
	}
	future, _ := ScanBehavior(root, BehaviorScanOptions{Since: time.Now().Add(time.Hour)})
	if future.BeforeSince != 5 || len(future.Descriptors) != 0 {
		t.Fatalf("since scan = %+v", future)
	}
	tiny, _ := ScanBehavior(root, BehaviorScanOptions{MaxEventBytes: 1})
	if tiny.Oversized != 5 {
		t.Fatalf("bounded scan = %+v", tiny)
	}
}

func TestRunCLIClustersBehaviorCohorts(t *testing.T) {
	root := t.TempDir()
	writeJournal(t, root, "g", "run-a1", "a", "sha256:a", 1, journal.PhaseCompleted)
	writeJournal(t, root, "g", "run-b1", "b", "sha256:b", 1, journal.PhaseCompleted)
	writeJournal(t, root, "h", "run-c1", "c", "sha256:c", 6, journal.PhaseFailed)
	writeJournal(t, root, "h", "run-d1", "d", "sha256:d", 6, journal.PhaseFailed)

	var stdout, stderr bytes.Buffer
	args := []string{"--features", "behavior", "--min-samples", "1", "--min-cluster-size", "2", "--stratify", "gaggle", "--json", root}
	if code := RunCLI(args, flag.NewFlagSet("clusters", flag.ContinueOnError), &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var out struct {
		Model     Model
		Confounds *ConfoundReport
		Behavior  struct {
			Cohorts []BehaviorSummary
			Scan    BehaviorScan
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Model.FeatureVersion != BehaviorFeatureVersion || out.Model.Population != 4 || len(out.Behavior.Cohorts) != 4 ||
		out.Behavior.Scan.Candidates != 4 || out.Confounds == nil || out.Behavior.Scan.Descriptors != nil {
		t.Fatalf("output = %s", stdout.String())
	}

	stdout.Reset()
	if code := RunCLI([]string{"--features", "behavior", "--min-samples", "1", "--min-cluster-size", "2", root},
		flag.NewFlagSet("clusters", flag.ContinueOnError), &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "run behavior") || !strings.Contains(stdout.String(), "Behavior scan: 4 journals") {
		t.Fatalf("text output = %q", stdout.String())
	}
	for _, bad := range [][]string{
		{"--since", "1h", root},
		{"--features", "genome", root},
		{"--features", "behavior", "--max-runs", "0", root},
	} {
		if code := RunCLI(bad, flag.NewFlagSet("clusters", flag.ContinueOnError), &stdout, &stderr); code != 2 {
			t.Errorf("%v: code = %d, want 2", bad, code)
		}
	}
}
