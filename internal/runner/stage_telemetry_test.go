package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
)

func TestShellStageTelemetryRoundTripsToRollup(t *testing.T) {
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	client, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "runner-stage-telemetry-test",
		SpanExporter: telemetry.NewJournalSpanExporter(runsDir, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, reg SecretRegistrar) (invoke.Deterministic, error) {
			injector, err := credentials.NewInjector(&credentials.Resolver{}, nil, reg)
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
> "$GOOBERS_TELEMETRY_DIR/events.jsonl"`
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
	if len(events) != 4 {
		t.Fatalf("span events = %d, want 4: %#v", len(events), events)
	}
	if events[0].Name != "exitCode" || events[0].Attributes["goobers.metric.value"] != "99" {
		t.Fatalf("exitCode metric event = %#v", events[0])
	}
	if events[1].Name != "build.items" || events[1].Attributes["goobers.metric.unit"] != "count" {
		t.Fatalf("build.items metric event = %#v", events[1])
	}
	if events[2].Name != "scan.complete" || events[2].Attributes["authorization"] != journal.Redacted {
		t.Fatalf("custom event = %#v", events[2])
	}
	if events[3].Name != "goobers.telemetry.warning" ||
		events[3].Attributes["goobers.telemetry.file"] != "metrics.jsonl" ||
		events[3].Attributes["goobers.telemetry.dropped_lines"] != "1" {
		t.Fatalf("malformed-line warning = %#v", events[3])
	}
}
