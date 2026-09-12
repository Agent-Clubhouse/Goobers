package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

type statusQueueFake struct{ calls []statusWorkflowKey }

func (f *statusQueueFake) QueueEligibility(_ context.Context, gaggle, workflow string) (readservice.QueueEligibilityView, error) {
	f.calls = append(f.calls, statusWorkflowKey{gaggle: gaggle, workflow: workflow})
	return readservice.QueueEligibilityView{Gaggle: gaggle, Workflow: workflow, Status: "not-observed"}, nil
}

func TestStatusQueueScopesBeforeBound(t *testing.T) {
	var workflows []apiv1.Workflow
	for i := 30; i >= 0; i-- {
		var def apiv1.Workflow
		def.Name = fmt.Sprintf("review-%02d", i)
		def.Spec.Gaggle = "core"
		workflows = append(workflows, def)
	}
	fake := &statusQueueFake{}
	result := collectStatusQueueEvidence(context.Background(), fake, workflows, "core", "review-30")
	if len(fake.calls) != 1 || fake.calls[0].workflow != "review-30" || result.OmittedWorkflows != 0 {
		t.Fatalf("filter applied after bound: %+v %+v", fake.calls, result)
	}
	fake.calls = nil
	result = collectStatusQueueEvidence(context.Background(), fake, workflows, "", "")
	if len(fake.calls) != maxStatusQueueWorkflows || result.OmittedWorkflows != 15 || fake.calls[0].workflow != "review-00" {
		t.Fatalf("unstable/unbounded query: %+v %+v", fake.calls, result)
	}
}

func TestStatusAndExplainReadRealQueueProjection(t *testing.T) {
	root := initScheduledDemo(t)
	layout := instance.NewLayout(root)
	set, validation, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	def := set.Workflows[0]
	view := queueExplainFixture()
	view.Report.Gaggle, view.Report.Workflow = def.Spec.Gaggle, def.Name
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: view.Report.RunID, Workflow: def.Name, Gaggle: def.Spec.Gaggle, StartedAt: view.Report.ObservedAt}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	data, err := json.Marshal(map[string]any{"queueEligibility": view.Report})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := run.RecordArtifact("select/result", data)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "select", Status: "no-work", Outputs: map[string]any{"queueEligibilityVersion": "1"}, Artifacts: []journal.Ref{ref}}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dir := filepath.Join(layout.RunsDir(), view.Report.RunID)
	if err := store.ProjectRunDir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "status", "--json", "--gaggle="+def.Spec.Gaggle, "--workflow="+def.Name, root)
	if code != 0 {
		t.Fatalf("status code=%d: %s", code, stderr)
	}
	var result statusJSONOutput
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.QueueEligibility == nil || len(result.QueueEligibility.Workflows) != 1 || result.QueueEligibility.Workflows[0].Report == nil {
		t.Fatalf("missing status evidence: %s", stdout)
	}
	if result.QueueEligibility.Workflows[0].ReadState == nil {
		t.Fatalf("CLI wrapper dropped projection freshness: %s", stdout)
	}
	code, stdout, stderr = runArgs(t, "status", "--workflow="+def.Name, root)
	if code != 0 || !strings.Contains(stdout, "PR #42") || !strings.Contains(stdout, "historical selection evidence") {
		t.Fatalf("status text code=%d: %s %s", code, stdout, stderr)
	}
	// The explain path and queue collector must still work without event replay.
	if err := os.Rename(filepath.Join(dir, "events.jsonl"), filepath.Join(dir, "events.saved")); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runArgs(t, "queue-explain", "--gaggle="+def.Spec.Gaggle, "--workflow="+def.Name, "--pr=42", root)
	if code != 0 || !strings.Contains(stdout, "PR #42") {
		t.Fatalf("explain code=%d: %s %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "projection completeness=") || !strings.Contains(stdout, "no_sweep_completed") {
		t.Fatalf("explain hides unknown projection freshness: %s", stdout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	queue := loadStatusQueueEvidence(ctx, readservice.LocalSources{Layout: layout, Config: cfg, Definitions: set, Validation: validation}, set.Workflows, def.Spec.Gaggle, def.Name)
	if len(queue.Workflows) != 1 || queue.Workflows[0].Report == nil {
		t.Fatalf("queue collector replayed events: %+v", queue)
	}
}
