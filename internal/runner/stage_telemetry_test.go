package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func TestShellStageTelemetryRoundTripsToRollup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test telemetry producer uses POSIX shell syntax")
	}
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	client, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "runner-stage-telemetry-test",
		SpanExporter: telemetry.NewJournalSpanExporter(runsDir, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	wtMgr, err = worktree.NewManager(wtMgr.Root, worktree.WithUsageObserver("acme-web", client.RecordWorkcopyUsage))
	if err != nil {
		t.Fatal(err)
	}

	// These stages declare no capabilities, so no ref is ever resolved.
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}

	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, reg SecretRegistrar) (invoke.Deterministic, error) {
			injector, err := credentials.NewInjector(resolver, nil, reg)
			if err != nil {
				return nil, err
			}
			return executor.NewShellExecutor(injector, rec)
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
		Telemetry: client,
	})
	if err != nil {
		t.Fatal(err)
	}

	const eventTime = "2026-07-18T18:00:00Z"
	canary := "ghp_" + strings.Repeat("x", 36)
	script := `printf '%s\n' \
'{"name":"exitCode","value":99}' \
'{"name":"build.items","value":7,"unit":"count","attrs":{"source":"shell"}}' \
'malformed' > "$GOOBERS_TELEMETRY_DIR/metrics.jsonl"
printf '%s\n' \
'{"ts":"` + eventTime + `","name":"scan.complete","attrs":{"authorization":"` + canary + `"}}' \
> "$GOOBERS_TELEMETRY_DIR/events.jsonl"
i=0
while [ "$i" -lt 130 ]; do
  printf '{"ts":"` + eventTime + `","name":"batch.%03d","attrs":{"index":%d}}\n' "$i" "$i"
  i=$((i + 1))
done >> "$GOOBERS_TELEMETRY_DIR/events.jsonl"`
	machine, err := workflow.Compile(workflow.Definition{
		Name:    "stage-telemetry",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web",
			Start:  "emit",
			Tasks: []apiv1.Task{{
				Name: "emit", Type: apiv1.TaskDeterministic, Goal: "emit telemetry",
				Run:  &apiv1.DeterministicRun{Command: []string{"sh", "-c", script}},
				Next: workflow.TerminalComplete,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := telemetry.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}

	rawSpans, err := os.ReadFile(filepath.Join(runsDir, runID, "spans", "spans.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawSpans), canary) {
		t.Fatalf("stage telemetry secret reached spans at rest:\n%s", rawSpans)
	}

	db, err := rollup.Open(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.IngestRun(filepath.Join(runsDir, runID)); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}
	spans, err := db.Spans(runID)
	if err != nil {
		t.Fatal(err)
	}
	var taskSpanID string
	for _, span := range spans {
		if span.Name == "task/emit" {
			taskSpanID = span.SpanID
		}
	}

	if taskSpanID == "" {
		t.Fatalf("task span missing from rollup: %#v", spans)
	}
	events, err := db.SpanEvents(runID, taskSpanID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 138 {
		t.Fatalf("span events = %d, want 138", len(events))
	}
	if events[0].Name != telemetry.EventWorktreeDiskUsage ||
		events[0].Attributes[telemetry.AttrRunID] != runID ||
		events[0].Attributes[telemetry.AttrGaggle] != "acme-web" ||
		events[0].Attributes[telemetry.AttrStorageOperation] != string(worktree.UsageOperationCreate) {
		t.Fatalf("worktree create metric event = %#v", events[0])
	}
	if events[1].Name != telemetry.EventWorkcopyDiskUsage {
		t.Fatalf("workcopy create metric event = %#v", events[1])
	}
	if events[2].Name != "exitCode" || events[2].Attributes["goobers.metric.value"] != "99" {
		t.Fatalf("exitCode metric event = %#v", events[2])
	}
	if events[3].Name != "build.items" || events[3].Attributes["goobers.metric.unit"] != "count" {
		t.Fatalf("build.items metric event = %#v", events[3])
	}
	if events[4].Name != "scan.complete" || events[4].Attributes["authorization"] != journal.Redacted {
		t.Fatalf("custom event = %#v", events[4])
	}
	if events[5].Name != "batch.000" || events[5].Attributes["index"] != "0" {
		t.Fatalf("first batch event = %#v", events[5])
	}
	if events[134].Name != "batch.129" || events[134].Attributes["index"] != "129" {
		t.Fatalf("last batch event = %#v", events[134])
	}
	if events[135].Name != "goobers.telemetry.warning" ||
		events[135].Attributes["goobers.telemetry.file"] != "metrics.jsonl" ||
		events[135].Attributes["goobers.telemetry.dropped_lines"] != "1" {
		t.Fatalf("malformed-line warning = %#v", events[135])
	}
	if events[136].Name != telemetry.EventWorktreeDiskUsage ||
		events[136].Attributes[telemetry.AttrStorageOperation] != string(worktree.UsageOperationTeardown) ||
		events[137].Name != telemetry.EventWorkcopyDiskUsage {
		t.Fatalf("teardown metric events = %#v, %#v", events[136], events[137])
	}
}

type telemetryEmittingReviewer struct{}

func (*telemetryEmittingReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (*telemetryEmittingReviewer) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	dir := telemetry.StageTelemetryDir(env.Workspace)
	if err := os.WriteFile(filepath.Join(dir, "metrics.jsonl"), []byte(
		"{\"name\":\"review.score\",\"value\":1,\"unit\":\"ratio\"}\n",
	), 0o600); err != nil {
		return apiv1.Verdict{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(
		"{\"ts\":\"2026-07-18T18:00:00Z\",\"name\":\"review.complete\",\"attrs\":{\"findingCount\":0}}\n",
	), 0o600); err != nil {
		return apiv1.Verdict{}, err
	}
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestAgenticGateTelemetryRoundTripsToRollup(t *testing.T) {
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	client, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "runner-gate-telemetry-test",
		SpanExporter: telemetry.NewJournalSpanExporter(runsDir, nil),
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	// These stages declare no capabilities, so no ref is ever resolved.
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}

	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, reg SecretRegistrar) (invoke.Deterministic, error) {
			injector, err := credentials.NewInjector(resolver, nil, reg)
			if err != nil {
				return nil, err
			}
			return executor.NewShellExecutor(injector, rec)
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return &telemetryEmittingReviewer{}, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
		Telemetry: client,
	})
	if err != nil {
		t.Fatal(err)
	}

	machine, err := workflow.Compile(workflow.Definition{
		Name:    "gate-telemetry",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web",
			Start:  "prepare",
			Tasks: []apiv1.Task{{
				Name: "prepare", Type: apiv1.TaskDeterministic, Goal: "prepare review",
				Run:  &apiv1.DeterministicRun{Command: []string{testExecutable, "-test.run=^$"}},
				Next: "review",
			}},
			Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					string(apiv1.VerdictPass):         workflow.TerminalComplete,
					string(apiv1.VerdictNeedsChanges): "prepare",
					string(apiv1.VerdictFail):         workflow.TargetAbort,
				},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := telemetry.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}

	db, err := rollup.Open(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.IngestRun(filepath.Join(runsDir, runID)); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}
	spans, err := db.Spans(runID)
	if err != nil {
		t.Fatal(err)
	}
	var gateSpanID string
	for _, span := range spans {
		if span.Name == "gate/review" {
			gateSpanID = span.SpanID
		}
	}
	if gateSpanID == "" {
		t.Fatalf("gate span missing from rollup: %#v", spans)
	}
	events, err := db.SpanEvents(runID, gateSpanID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("gate span events = %d, want 2: %#v", len(events), events)
	}
	if events[0].Name != "review.score" ||
		events[0].Attributes["goobers.metric.value"] != "1" ||
		events[0].Attributes["goobers.metric.unit"] != "ratio" {
		t.Fatalf("gate metric event = %#v", events[0])
	}
	if events[1].Name != "review.complete" || events[1].Attributes["findingCount"] != "0" {
		t.Fatalf("gate custom event = %#v", events[1])
	}
}

func TestResumeFailedRunSpanMatchesJournalOutcome(t *testing.T) {
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	exporter := telemetry.NewMemoryExporter()
	client, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "runner-resume-telemetry-test",
		SpanExporter: exporter,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	machine, err := workflow.Compile(workflow.Definition{
		Name:    "resume-failure",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web",
			Start:  "fail",
			Tasks: []apiv1.Task{{
				Name: "fail", Type: apiv1.TaskDeterministic, Goal: "fail",
				Run:  &apiv1.DeterministicRun{Command: []string{"false"}},
				Next: workflow.TerminalComplete,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := telemetry.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{
				runID + ":fail": {
					status:    apiv1.ResultFailure,
					errorInfo: &apiv1.ErrorInfo{Code: "build_failed", Message: "build failed"},
				},
			}}, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
		Telemetry: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Resume(context.Background(), ResumeInput{
		RunID:   runID,
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", result.Phase)
	}

	var found bool
	for _, span := range exporter.Spans() {
		if span.Name() != "run/resume-failure" {
			continue
		}
		found = true
		attrs := map[string]string{}
		for _, attr := range span.Attributes() {
			attrs[string(attr.Key)] = attr.Value.Emit()
		}
		if got := attrs[telemetry.AttrOutcome]; got != telemetry.OutcomeFailure {
			t.Errorf("run outcome = %q, want failure", got)
		}
		if got := attrs[telemetry.AttrErrorCode]; got != "build_failed" {
			t.Errorf("run error code = %q, want build_failed", got)
		}
		if span.Status().Code != codes.Error {
			t.Errorf("run status = %s, want Error", span.Status().Code)
		}
	}
	if !found {
		t.Fatal("resumed run span not exported")
	}
}
