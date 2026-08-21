package main

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

// agentProbeWorkflows is a two-workflow fixture covering every stage
// classification the probe must distinguish: an agentic task, an agentic gate,
// and a deterministic task.
func agentProbeWorkflows() []apiv1.Workflow {
	return []apiv1.Workflow{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "default-implement"},
			Spec: apiv1.WorkflowSpec{
				Gaggle: "example",
				Start:  "query-backlog",
				Tasks: []apiv1.Task{
					{Name: "query-backlog", Type: apiv1.TaskDeterministic, Goal: "claim"},
					{Name: "implement", Type: apiv1.TaskAgentic, Goal: "implement", Goober: "coder"},
				},
				Gates: []apiv1.Gate{
					{
						Name:      "review",
						Evaluator: apiv1.EvaluatorAgentic,
						Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
						Branches:  map[string]string{"pass": "@complete"},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "nightly-sweep"},
			Spec: apiv1.WorkflowSpec{
				Gaggle: "ops",
				Start:  "sweep",
				Tasks: []apiv1.Task{
					{Name: "sweep", Type: apiv1.TaskDeterministic, Goal: "sweep"},
				},
			},
		},
	}
}

func agentProbeRun(runID, gaggle, workflow, stage string, phase journal.RunPhase) runSummary {
	return runSummary{
		RunID:    runID,
		Gaggle:   gaggle,
		Workflow: workflow,
		Phase:    phase,
		Operator: readservice.OperatorRunSummary{CurrentStage: stage, Liveness: "recent"},
	}
}

func TestBuildAgentProbeListsLiveAgenticStagesByRole(t *testing.T) {
	runs := []runSummary{
		agentProbeRun("run-implement", "example", "default-implement", "implement", journal.PhaseRunning),
		agentProbeRun("run-review", "example", "default-implement", "review", journal.PhaseRunning),
		agentProbeRun("run-deterministic", "example", "default-implement", "query-backlog", journal.PhaseRunning),
		agentProbeRun("run-between-stages", "ops", "nightly-sweep", "", journal.PhaseRunning),
		agentProbeRun("run-done", "example", "default-implement", "implement", journal.PhaseCompleted),
	}

	probe := buildAgentProbe(runs, agentProbeWorkflows(), "", "", "")

	if probe.Source != agentProbeSourceJournal {
		t.Fatalf("probe.Source = %q, want %q", probe.Source, agentProbeSourceJournal)
	}
	if len(probe.Live) != 2 {
		t.Fatalf("probe.Live = %+v, want exactly the two agentic stages", probe.Live)
	}
	// Ordered by role: coder before reviewer.
	if probe.Live[0].Role != "coder" || probe.Live[0].RunID != "run-implement" ||
		probe.Live[0].Stage != "implement" || probe.Live[0].StageKind != agentStageKindTask {
		t.Fatalf("probe.Live[0] = %+v", probe.Live[0])
	}
	if probe.Live[1].Role != "reviewer" || probe.Live[1].RunID != "run-review" ||
		probe.Live[1].Stage != "review" || probe.Live[1].StageKind != agentStageKindGate {
		t.Fatalf("probe.Live[1] = %+v", probe.Live[1])
	}
	// A terminal run is not live work; a deterministic stage and a run between
	// stages are live but not agentic, and must be counted rather than dropped
	// — an operator sizing a quiet window needs to know the instance is busy.
	if probe.OtherLiveRuns != 2 {
		t.Fatalf("probe.OtherLiveRuns = %d, want 2", probe.OtherLiveRuns)
	}
	if probe.SelfExcluded != nil {
		t.Fatalf("probe.SelfExcluded = %+v, want nil when the caller is not a stage", probe.SelfExcluded)
	}
}

// TestBuildAgentProbeExcludesTheInvokingRun is the regression test for the
// shape #3362 reports: a probe that reports busy=1 when the only "match" is
// the asker itself. Here the sole live agentic stage IS the caller's own run,
// and the probe must answer zero — while still disclosing what it removed.
func TestBuildAgentProbeExcludesTheInvokingRun(t *testing.T) {
	runs := []runSummary{
		agentProbeRun("run-self", "example", "default-implement", "implement", journal.PhaseRunning),
	}

	probe := buildAgentProbe(runs, agentProbeWorkflows(), "run-self", "", "")

	if len(probe.Live) != 0 {
		t.Fatalf("probe.Live = %+v, want empty: the only live stage is the asker", probe.Live)
	}
	if probe.OtherLiveRuns != 0 {
		t.Fatalf("probe.OtherLiveRuns = %d, want 0: the self-excluded run is not an 'other' run", probe.OtherLiveRuns)
	}
	if probe.SelfExcluded == nil {
		t.Fatal("probe.SelfExcluded = nil, want the invoking run disclosed rather than silently dropped")
	}
	if probe.SelfExcluded.RunID != "run-self" || probe.SelfExcluded.Stage != "implement" ||
		probe.SelfExcluded.Role != "coder" || probe.SelfExcluded.Reason == "" {
		t.Fatalf("probe.SelfExcluded = %+v", probe.SelfExcluded)
	}
}

func TestBuildAgentProbeReportsUnclassifiableLiveRun(t *testing.T) {
	runs := []runSummary{
		agentProbeRun("run-orphan", "retired", "removed-workflow", "implement", journal.PhaseRunning),
	}

	probe := buildAgentProbe(runs, agentProbeWorkflows(), "", "", "")

	if len(probe.Live) != 1 {
		t.Fatalf("probe.Live = %+v, want the unclassifiable run reported, never silently dropped", probe.Live)
	}
	if probe.Live[0].Role != agentRoleUnknown || probe.Live[0].StageKind != agentStageKindUnknown {
		t.Fatalf("probe.Live[0] = %+v, want the unknown-role classification", probe.Live[0])
	}
}

func TestBuildAgentProbeHonorsWorkflowAndGaggleFilters(t *testing.T) {
	runs := []runSummary{
		agentProbeRun("run-implement", "example", "default-implement", "implement", journal.PhaseRunning),
		agentProbeRun("run-other", "other", "default-implement", "implement", journal.PhaseRunning),
	}
	workflows := append(agentProbeWorkflows(), apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "default-implement"},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "other",
			Start:  "implement",
			Tasks:  []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goal: "implement", Goober: "coder"}},
		},
	})

	probe := buildAgentProbe(runs, workflows, "", "", "other")
	if len(probe.Live) != 1 || probe.Live[0].RunID != "run-other" {
		t.Fatalf("gaggle-filtered probe.Live = %+v", probe.Live)
	}
	if probe.OtherLiveRuns != 0 {
		t.Fatalf("a filtered-out run must not be counted as an other live run, got %d", probe.OtherLiveRuns)
	}

	probe = buildAgentProbe(runs, workflows, "", "nightly-sweep", "")
	if len(probe.Live) != 0 {
		t.Fatalf("workflow-filtered probe.Live = %+v, want empty", probe.Live)
	}
}

// writeAgentProbeRun writes a run journal parked at stage, with a heartbeat, so
// the probe reads it the way it reads a real in-flight stage.
func writeAgentProbeRun(t *testing.T, root, runID, workflow, gaggle, stage string) {
	t.Helper()
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID:     runID,
		Workflow:  workflow,
		Gaggle:    gaggle,
		StartedAt: time.Now().Add(-3 * time.Minute),
	}, nil)
	if err != nil {
		t.Fatalf("create agent probe fixture run: %v", err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: stage, Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: stage, Attempt: 1}); err != nil {
		t.Fatalf("append stage.heartbeat: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close agent probe fixture run: %v", err)
	}
}

func TestStatusAgentsListsLiveStagesByRole(t *testing.T) {
	unsetRunContext(t)
	root := initDemo(t)
	writeAgentProbeRun(t, root, "run-implement", "default-implement", "example", "implement")

	code, stdout, stderr := runArgs(t, "status", "--agents", root)
	if code != 0 {
		t.Fatalf("status --agents code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "agentic stages live: 1") {
		t.Fatalf("status --agents stdout = %q", stdout)
	}
	// "recent" is the heartbeat liveness the fixture's stage.heartbeat earns:
	// the probe reports a stage the runner believes is live AND how fresh that
	// belief is, so a wedged stage is not silently indistinguishable from a
	// working one.
	for _, want := range []string{"coder", "run-implement", "implement", "example/default-implement", "recent", agentProbeSourceJournal} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("status --agents stdout missing %q: %q", want, stdout)
		}
	}
	if strings.Contains(stdout, "self-excluded") {
		t.Fatalf("no invoking run, so nothing should be self-excluded: %q", stdout)
	}
}

// TestStatusAgentsJSONExcludesTheInvokingRunEndToEnd drives the whole command
// the way a stage would invoke it — GOOBERS_RUN_ID pointing at its own run —
// and asserts the probe answers "no agentic stages live" instead of counting
// the asker.
func TestStatusAgentsJSONExcludesTheInvokingRunEndToEnd(t *testing.T) {
	unsetRunContext(t)
	root := initDemo(t)
	writeAgentProbeRun(t, root, "run-self", "default-implement", "example", "implement")

	previous := selfProbeRunID
	selfProbeRunID = func() string { return "run-self" }
	t.Cleanup(func() { selfProbeRunID = previous })

	code, stdout, stderr := runArgs(t, "status", "--agents", "--json", root)
	if code != 0 {
		t.Fatalf("status --agents --json code = %d, stderr = %q", code, stderr)
	}
	var probe agentProbe
	if err := json.Unmarshal([]byte(stdout), &probe); err != nil {
		t.Fatalf("status --agents --json = %q: %v", stdout, err)
	}
	if probe.Source != agentProbeSourceJournal {
		t.Fatalf("probe.Source = %q", probe.Source)
	}
	if len(probe.Live) != 0 {
		t.Fatalf("probe.Live = %+v, want empty: the only live stage is this invocation's own run", probe.Live)
	}
	if probe.SelfExcluded == nil || probe.SelfExcluded.RunID != "run-self" {
		t.Fatalf("probe.SelfExcluded = %+v, want run-self disclosed", probe.SelfExcluded)
	}
}

func TestStatusAgentsRejectsHistoricalRunTableFlags(t *testing.T) {
	unsetRunContext(t)
	root := initDemo(t)

	for _, args := range [][]string{
		{"status", "--agents", "--phase=running", root},
		{"status", "--agents", "--limit=5", root},
		{"status", "--agents", "--watch", root},
		{"status", "--agents", "--daemon", root},
	} {
		code, _, stderr := runArgs(t, args...)
		if code != 2 {
			t.Fatalf("%v code = %d, want 2 (usage error); stderr = %q", args, code, stderr)
		}
		if !strings.Contains(stderr, "cannot be combined") {
			t.Fatalf("%v stderr = %q, want a combination diagnostic", args, stderr)
		}
	}
}

// TestStatusAgentsRunsWithoutProviderCredentials pins the #3346 read: the probe
// makes no provider call, so a credential-less invocation (an operator's
// `kubectl exec` into a pod) still gets a clean answer rather than a
// verification-unavailable line dressed up as a blocker.
func TestStatusAgentsRunsWithoutProviderCredentials(t *testing.T) {
	unsetRunContext(t)
	root := initDemo(t)
	writeAgentProbeRun(t, root, "run-implement", "default-implement", "example", "implement")

	code, stdout, stderr := runArgs(t, "status", "--agents", root)
	if code != 0 {
		t.Fatalf("status --agents code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, "blockers:") || strings.Contains(stdout, "unavailable") {
		t.Fatalf("agents probe must not surface diagnostic-credential lines: %q", stdout)
	}
}

func TestStatusAgentsReportsAnIdleInstance(t *testing.T) {
	unsetRunContext(t)
	root := initDemo(t)

	code, stdout, stderr := runArgs(t, "status", "--agents", root)
	if code != 0 {
		t.Fatalf("status --agents code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "agentic stages live: 0") ||
		!strings.Contains(stdout, "no agentic stages in flight") {
		t.Fatalf("idle status --agents stdout = %q", stdout)
	}
}

// TestAgentProbeNeverScansProcesses pins the structural property the whole
// feature exists for (#3362): the answer is built from the runner's own
// bookkeeping, so the probe cannot match the process asking. A later change
// that reintroduces process matching here — os/exec, a ps/pgrep shell-out, a
// /proc walk — reopens exactly the defect this replaced, so it is guarded in
// code rather than left to a note someone has to remember having read.
func TestAgentProbeNeverScansProcesses(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve this test file's path")
	}
	path := filepath.Join(filepath.Dir(thisFile), "statusagents.go")
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	forbidden := map[string]string{
		"os/exec":                    "shelling out (ps/pgrep/tasklist) is how the self-matching probe is written",
		"github.com/shirou/gopsutil": "a process-table library answers the question from the wrong source",
		"github.com/mitchellh/go-ps": "a process-table library answers the question from the wrong source",
	}
	for _, spec := range parsed.Imports {
		imported := strings.Trim(spec.Path.Value, `"`)
		for prefix, why := range forbidden {
			if imported == prefix || strings.HasPrefix(imported, prefix+"/") {
				t.Errorf("statusagents.go imports %q — %s; the agents probe must answer from the runner's journals only", imported, why)
			}
		}
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "/proc/") {
		t.Error("statusagents.go walks /proc — the agents probe must answer from the runner's journals only, never from a process table")
	}
}
