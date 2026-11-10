package workflowcluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"maps"
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
		TaskCohort: "cohort-a",
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
		ToolCalls: []string{"functions.apply_patch", "functions.powershell"},
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
		"tools.filesystem": 1, "tools.shell": 1,
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
	in.ChildObservation = &ChildBehaviorObservation{
		Disposition: "replace", ParentWait: 12 * time.Second,
		PublicationState: map[string]string{"branch": "confirmed", "pr": "effect_pending"},
		CompletionSeen:   true,
	}
	d, err := ExtractBehavior(in)
	if err != nil {
		t.Fatal(err)
	}
	if d.Lineage == nil || d.Lineage.ParentRunID != "parent-1" || d.Lineage.SourceDigest != "sha256:proposal" || d.Numeric["child.generated"] != 1 {
		t.Fatalf("child lineage = %+v, generated = %v", d.Lineage, d.Numeric["child.generated"])
	}
	if d.Numeric["child.stages.generated"] != 2 || d.Numeric["child.disposition.replace"] != 1 ||
		d.Numeric["child.publication.branch.confirmed"] != 1 || d.Numeric["child.publication.pr.effect_pending"] != 1 ||
		d.Numeric["child.parentWait.log1pSeconds"] != math.Log1p(12) {
		t.Fatalf("child features = %+v", d.Numeric)
	}
}

func TestToolCategoryUsesFixedStructuralBuckets(t *testing.T) {
	tests := map[string]string{
		"functions.delegate":         "agent",
		"functions.rg":               "filesystem",
		"functions.fetch":            "network",
		"goobers-io-read_input":      "orchestration",
		"github.search_issues":       "provider",
		"functions.powershell":       "shell",
		"unrecognized-custom-method": "other",
		// Copilot built-ins.
		"edit": "filesystem", "create": "filesystem", "view": "filesystem", "task": "agent", "bash": "shell",
		// Claude built-ins.
		"Read": "filesystem", "Edit": "filesystem", "Write": "filesystem", "MultiEdit": "filesystem",
		"LS": "filesystem", "NotebookEdit": "filesystem", "Glob": "filesystem", "Grep": "filesystem",
		"Task": "agent", "Bash": "shell", "WebFetch": "network",
		// Codex canonical names.
		"command_execution": "shell", "apply_patch": "filesystem", "web_search": "network",
	}
	for tool, want := range tests {
		if got := toolCategory(tool); got != want {
			t.Errorf("%q category = %q, want %q", tool, got, want)
		}
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
			TaskCohort: "review", Numeric: map[string]float64{"visits.total": visits}}
	}
	ds := []BehaviorDescriptor{
		descriptor("r4", "a", 4), descriptor("r1", "a", 1), descriptor("r3", "a", 3), descriptor("r2", "a", 2),
		descriptor("r5", "b", 9),
	}
	got := SummarizeBehavior(ds, 2)
	if len(got) != 1 || !strings.HasPrefix(got[0].ID, "g/a@0123456789abcdef~") ||
		got[0].TaskCohort != "review" || got[0].SampleCount != 4 {
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

func TestSummarizeBehaviorSeparatesTaskCohorts(t *testing.T) {
	base := BehaviorDescriptor{
		Gaggle: "g", Workflow: "wf", EffectiveVersion: "sha256:0123456789abcdef",
		Numeric: map[string]float64{"visits.total": 1},
	}
	a, b := base, base
	a.RunID, a.TaskCohort = "a", "review"
	b.RunID, b.TaskCohort = "b", "implementation"
	got := SummarizeBehavior([]BehaviorDescriptor{a, b}, 1)
	if len(got) != 2 || got[0].ID == got[1].ID || got[0].TaskCohort == got[1].TaskCohort {
		t.Fatalf("task cohorts were pooled: %+v", got)
	}
}

func TestReadChildObservationsJoinsWaitDispositionAndPublication(t *testing.T) {
	at := func(seconds int) time.Time { return behaviorEpoch.Add(time.Duration(seconds) * time.Second) }
	wait := func(requestID, action string, started int) journal.Event {
		return journal.Event{
			Type: journal.EventRunnerAnnotation, Time: at(started),
			Runner: map[string]any{
				"kind": journal.ChildWaitKind,
				"childWait": map[string]any{"request": map[string]any{
					"parentRunId": "parent-1", "requestId": requestID, "childRunId": "child-1", "action": action,
				}},
			},
		}
	}
	events := []journal.Event{
		wait("request-1", "wait", 2),
		{Type: journal.EventRunnerAnnotation, Time: at(7), Runner: map[string]any{"kind": journal.ChildContinuedKind, "requestId": "request-1"}},
		wait("request-2", "merge", 10),
		{Type: journal.EventRunnerAnnotation, Time: at(13), Runner: map[string]any{"kind": journal.ChildContinuedKind, "requestId": "request-2"}},
	}
	artifact := behaviorArtifact{data: []byte(`{
		"request":{"parentRunId":"parent-1","requestId":"request-2","childRunId":"child-1","action":"merge"},
		"completion":{"publications":[
			{"action":"branch","state":"confirmed"},
			{"action":"pr","state":"prepared"}
		]}}`)}
	key := childObservationKey{parentRunID: "parent-1", childRunID: "child-1"}
	got := readChildObservations("parent-1", events, []behaviorArtifact{artifact})[key]
	if got.Disposition != "merge" || got.ParentWait != 8*time.Second ||
		got.PublicationState["branch"] != "confirmed" || got.PublicationState["pr"] != "prepared" || !got.CompletionSeen {
		t.Fatalf("observation = %+v", got)
	}
}

func TestScanBehaviorJoinsChildObservationToMatchingParent(t *testing.T) {
	root := t.TempDir()
	runsDir := instance.NewLayout(root).ForGaggle("g").RunsDir()
	now := behaviorEpoch
	parent, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "parent-1", Gaggle: "g", Workflow: "parent", WorkflowDigest: "sha256:parent",
	}, nil, journal.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	now = behaviorEpoch.Add(2 * time.Second)
	wait := map[string]any{"request": map[string]any{
		"parentRunId": "parent-1", "requestId": "request-1", "childRunId": "child-1", "action": "merge",
	}}
	if err := parent.Append(journal.Event{
		Type:   journal.EventRunnerAnnotation,
		Runner: map[string]any{"kind": journal.ChildWaitKind, "childWait": wait},
	}); err != nil {
		t.Fatal(err)
	}
	now = behaviorEpoch.Add(7 * time.Second)
	if err := parent.Append(journal.Event{
		Type:   journal.EventRunnerAnnotation,
		Runner: map[string]any{"kind": journal.ChildContinuedKind, "requestId": "request-1"},
	}); err != nil {
		t.Fatal(err)
	}
	completion := []byte(`{
		"request":{"parentRunId":"parent-1","requestId":"request-1","childRunId":"child-1","action":"merge"},
		"completion":{"publications":[{"action":"branch","state":"confirmed"}]}}`)
	if _, err := parent.RecordStageArtifact("parent", 1, "", "child-1-completion.json", completion); err != nil {
		t.Fatal(err)
	}
	now = behaviorEpoch.Add(8 * time.Second)
	if err := parent.Append(journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}

	child, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "child-1", Gaggle: "g", Workflow: "child",
		WorkflowDigest:   "sha256:" + strings.Repeat("1", 64),
		GooberDigest:     "sha256:" + strings.Repeat("2", 64),
		ConfigGeneration: "sha256:" + strings.Repeat("3", 64),
		Child: &journal.ChildLineage{
			Gaggle: "g", ParentRunID: "parent-1", ParentWorkflow: "parent",
			StageOccurrence: "parent/branch0/visit1", InvocationKey: "parent",
			AcceptanceID: "trigger-child-1", SourceDigest: "sha256:" + strings.Repeat("4", 64),
			EnvelopeDigest: "sha256:" + strings.Repeat("5", 64),
		},
	}, nil, journal.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	now = behaviorEpoch
	if err := child.Append(journal.Event{Type: journal.EventStageStarted, Stage: "generated", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	now = behaviorEpoch.Add(time.Second)
	if err := child.Append(journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}

	scan, err := ScanBehavior(root, BehaviorScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range scan.Descriptors {
		if descriptor.RunID != "child-1" {
			continue
		}
		if descriptor.Numeric["child.parentWait.log1pSeconds"] != math.Log1p(5) ||
			descriptor.Numeric["child.disposition.merge"] != 1 ||
			descriptor.Numeric["child.publication.branch.confirmed"] != 1 ||
			descriptor.Numeric["child.publication.pr.none"] != 1 {
			t.Fatalf("child descriptor = %+v", descriptor)
		}
		return
	}
	t.Fatalf("child descriptor missing from %+v", scan)
}

func TestScanBehaviorKeepsPinnedExplicitTaskClass(t *testing.T) {
	root := t.TempDir()
	runsDir := instance.NewLayout(root).ForGaggle("g").RunsDir()
	definition := []byte(`{"Spec":{"triggers":[{"type":"manual"}]}}`)
	create := func(runID string, labels []byte) {
		inputs := map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition}
		if labels != nil {
			inputs[journal.PinnedWorkflowLabelsInputName] = labels
		}
		run, err := journal.Create(runsDir, journal.RunIdentity{
			RunID: runID, Gaggle: "g", Workflow: "w", WorkflowVersion: 1, WorkflowDigest: "sha256:w",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
		}, inputs, journal.WithClock(func() time.Time { return behaviorEpoch }))
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	create("run-auto", nil)
	create("run-labeled", []byte(`{"goobers.dev/task-class":"review","team":"x"}`))

	scan, err := ScanBehavior(root, BehaviorScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cohorts := map[string]string{}
	for _, d := range scan.Descriptors {
		cohorts[d.RunID] = d.TaskCohort
	}
	if cohorts["run-labeled"] != "review" || !strings.HasPrefix(cohorts["run-auto"], "auto:") {
		t.Fatalf("cohorts = %v", cohorts)
	}
	summaries := SummarizeBehavior(scan.Descriptors, 1)
	if len(summaries) != 2 {
		t.Fatalf("explicit and auto cohorts were pooled: %+v", summaries)
	}
	again, err := ScanBehavior(root, BehaviorScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(summaries)
	second, _ := json.Marshal(SummarizeBehavior(again.Descriptors, 1))
	if !bytes.Equal(first, second) {
		t.Fatal("behavior summaries are not byte-identical across scans")
	}
}

// writeToolRun records a terminal run whose stages carry the given harness
// transcripts and one runner span with the given span events.
func writeToolRun(t *testing.T, root, runID string, transcripts [][]byte, spanEvents []telemetry.SpanEventRecord) {
	t.Helper()
	run, err := journal.Create(instance.NewLayout(root).ForGaggle("g").RunsDir(), journal.RunIdentity{
		RunID: runID, Gaggle: "g", Workflow: "w", WorkflowVersion: 1, WorkflowDigest: "sha256:w",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, map[string][]byte{
		journal.PinnedWorkflowDefinitionInputName: []byte(`{"Spec":{"triggers":[{"type":"manual"}]}}`),
	}, journal.WithClock(func() time.Time { return behaviorEpoch }))
	if err != nil {
		t.Fatal(err)
	}
	span, err := json.Marshal(telemetry.SpanRecord{Kind: telemetry.SpanKindTask, Events: spanEvents, Attributes: map[string]string{
		telemetry.AttrModel: "m", telemetry.AttrHarnessVersion: "copilot-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpanWithSchema("implement", "implement", telemetry.SpanSchema, span); err != nil {
		t.Fatal(err)
	}
	for _, transcript := range transcripts {
		if _, err := run.RecordSpanWithSchema("implement", "copilot.transcript", telemetry.GenAIEventSchema, transcript); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func scanToolHistogram(t *testing.T, root string) map[string]float64 {
	t.Helper()
	scan, err := ScanBehavior(root, BehaviorScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Descriptors) != 1 {
		t.Fatalf("scan = %+v", scan)
	}
	tools := map[string]float64{}
	for _, category := range toolCategories {
		tools[category] = scan.Descriptors[0].Numeric["tools."+category]
	}
	return tools
}

func TestScanBehaviorCountsToolsFromHarnessTranscripts(t *testing.T) {
	// The canonical transcript the Copilot adapter produces from its native
	// session events (pinned by the harness transcript tests).
	fixture, err := os.ReadFile(filepath.Join("..", "harness", "testdata", "copilot-transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	shell := []byte(`{"role":"assistant","tool_call":{"id":"c1","name":"bash","arguments":{"command":"go test ./..."}}}
{"role":"tool","content":"ok","tool_call":{"id":"c1","success":true}}
{"role":"assistant","content":"{\"tool_call\":{\"name\":\"fetch\"}}"}
`)
	root := t.TempDir()
	writeToolRun(t, root, "run-1", [][]byte{fixture, shell}, nil)

	got := scanToolHistogram(t, root)
	want := map[string]float64{"agent": 0, "filesystem": 1, "network": 0, "orchestration": 0, "provider": 0, "shell": 1, "other": 0}
	if !maps.Equal(got, want) {
		t.Fatalf("tool histogram = %v, want %v", got, want)
	}
}

func TestScanBehaviorIgnoresStageEmittedToolEvents(t *testing.T) {
	spoofed := make([]telemetry.SpanEventRecord, 0, 3)
	for _, tool := range []string{"functions.powershell", "functions.fetch", "github.search_issues"} {
		spoofed = append(spoofed, telemetry.SpanEventRecord{Name: "harness.tool_call", Attributes: map[string]string{
			"tool": tool, telemetry.EmissionKindAttribute: "event",
		}})
	}
	root := t.TempDir()
	writeToolRun(t, root, "run-1", nil, spoofed)

	for category, count := range scanToolHistogram(t, root) {
		if count != 0 {
			t.Errorf("tools.%s = %v from agent-emitted events, want 0", category, count)
		}
	}
	in, err := readBehaviorInput(filepath.Join(instance.NewLayout(root).ForGaggle("g").RunsDir(), "run-1"), BehaviorScanOptions{}.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	for _, span := range in.Spans {
		if len(span.Events) != 0 {
			t.Fatalf("stage-emitted span events reached the extractor: %+v", span.Events)
		}
	}
}

// writeJournal records a synthetic terminal run under the instance root.
func writeJournal(t *testing.T, root, gaggle, runID, workflow, digest string, visits int, status journal.RunPhase) string {
	t.Helper()
	runsDir := instance.NewLayout(root).ForGaggle(gaggle).RunsDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Gaggle: gaggle, Workflow: workflow, WorkflowVersion: 1, WorkflowDigest: digest,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, map[string][]byte{
		journal.PinnedWorkflowDefinitionInputName: []byte(`{"Spec":{"triggers":[{"type":"manual"}]}}`),
	})
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
	if scan.Descriptors[0].RunID != "run-a1" || scan.Descriptors[0].TaskCohort == "" ||
		scan.Descriptors[2].Numeric["visits.total"] != 3 {
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
