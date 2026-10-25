//go:build cloudintegration

package childengine

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// Scenario 6 changes both the workflow goal and child publication policy while
// a real child awaits its frozen manifest response, then rolls back the
// instance opt-in. Existing runs retain permission; a new run is refused.
func TestChildQualificationConfigReload(t *testing.T) {
	h := newQualificationHarness(t)
	loaded := h.InstallHTTPBarrier(func(r *http.Request) bool { return r.URL.Path == apicontract.EngineChildLoadPath })
	run := h.StartParent()
	if err := loaded.Wait(h.ctx); err != nil {
		t.Fatal(err)
	}
	oldPins, err := h.epoch.store.RunPinsOf(h.ctx, run.GetID())
	if err != nil {
		t.Fatal(err)
	}
	var manifestDigest string
	if err := h.pool.QueryRow(h.ctx, `SELECT manifest_digest FROM control.child_engine_start WHERE instance_id=$1 AND parent_workflow_id=$2`, testInstance, run.GetID()).Scan(&manifestDigest); err != nil {
		t.Fatal(err)
	}
	before, err := h.blobs.Get(h.ctx, manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	var frozen engine.FrozenChildManifest
	if err := json.Unmarshal(before, &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.Input.ConfigGeneration != oldPins.ConfigGeneration || !frozen.Input.ChildWorkflowsEnabled {
		t.Fatalf("child did not freeze parent's generation and posture: %+v", frozen.Input)
	}
	generation := h.ReloadConfig(func(directory string) {
		edited := strings.Replace(childEnabledParent, "goal: Plan a child workflow", "goal: Plan using the reloaded configuration", 1)
		edited = strings.Replace(edited, "allowPRPublication: true", "allowPRPublication: false", 1)
		if err := os.WriteFile(filepath.Join(directory, "gaggles", "example", "workflows", "default-implement.yaml"), []byte(edited), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if generation == oldPins.ConfigGeneration {
		t.Fatal("reload did not change generation")
	}
	previous, _ := h.applied.Applied()
	reloadedGeneration := h.ReloadInstanceConfig(func(filename string) {
		raw, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "selfIdentity:") {
			t.Fatal("fixture unexpectedly defines selfIdentity")
		}
		raw = append(raw, []byte("\nselfIdentity: qualification-reloaded\n")...)
		if err := os.WriteFile(filename, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}, false)
	candidate, _ := h.applied.Applied()
	if candidate.Config == previous.Config || candidate.Config.SelfIdentity != "qualification-reloaded" || candidate.Config.EngineChildWorkflowsEnabled() || !previous.Config.EngineChildWorkflowsEnabled() {
		t.Fatal("instance reload did not replace policy and roll back the opt-in")
	}
	// instance.yaml is outside the retained definition archive.
	if reloadedGeneration != generation {
		t.Fatal("instance-only reload changed definition generation")
	}
	loaded.Release()
	qualificationCompleted(t, h, run)
	pins, err := h.epoch.store.RunPinsOf(h.ctx, run.GetID())
	if err != nil || pins != oldPins {
		t.Fatalf("reload changed existing run pins: %+v: %v", pins, err)
	}
	after, err := h.blobs.Get(h.ctx, manifestDigest)
	if err != nil || string(after) != string(before) || journal.Digest(after) != manifestDigest {
		t.Fatalf("reload changed frozen child manifest: %v", err)
	}
	original := qualificationRunInput(t, h, run)
	if original.ConfigGeneration != oldPins.ConfigGeneration || !original.ChildWorkflowsEnabled || !original.Spec.Tasks[0].ChildWorkflows.AllowPRPublication {
		t.Fatalf("in-flight parent lost pinned posture: %+v", original)
	}
	h.epoch.release()
	_, machine, err := childworkflow.StageCatalog(candidate.Config, candidate.Set, childworkflow.ParentSelection{Gaggle: "example", Workflow: "default-implement", Stage: "plan"}, childworkflow.BackendEngine, childworkflow.OfflineGooberAdmission)
	if err != nil {
		t.Fatal(err)
	}
	next := h.input
	next.RunID = ""
	next.ChildWorkflowsEnabled = candidate.Config.EngineChildWorkflowsEnabled()
	next.ConfigGeneration, next.Spec, next.Annotations = generation, machine.Def.Spec, machine.Def.Annotations
	next.WorkflowDigest, next.GooberDigest, _ = parentPins(t, candidate.Config, h.configDir, candidate.Set)
	record, _, err := h.epoch.store.Accept(h.ctx, "qualification-reloaded-parent", "qualification", []byte(`{"workflow":"default-implement"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	next.AcceptanceID = record.ID
	// Confirm durable pins before allowing the separate worker to execute.
	h.KillWorker()
	starter := startfence.New(h.temporal.Client(), h.queue, acceptance.NewStartAdmission(h.epoch.store), &h.applied)
	started, err := starter.Start(h.ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	releaseNext, ok, reason := h.epoch.scheduler.ReserveContinuation(started.RunID, next.Gaggle, next.WorkflowName)
	if !ok {
		t.Fatal(reason)
	}
	defer releaseNext()
	h.StartWorker()
	newRun := h.temporal.Client().GetWorkflow(h.ctx, started.RunID, "")
	if err := newRun.Get(h.ctx, nil); err == nil || !strings.Contains(err.Error(), workflow.ErrChildWorkflowExecutionUnsupported.Error()) {
		t.Fatalf("new run did not honor reloaded disabled child posture: %v", err)
	}
	newInput := qualificationRunInput(t, h, newRun)
	newPins, err := h.epoch.store.RunPinsOf(h.ctx, started.RunID)
	if err != nil || newPins.ConfigGeneration != generation || newPins.WorkflowDigest == oldPins.WorkflowDigest || newInput.ConfigGeneration != generation || newInput.ChildWorkflowsEnabled || newInput.Spec.Tasks[0].ChildWorkflows.AllowPRPublication || newInput.Spec.Tasks[0].Goal != "Plan using the reloaded configuration" {
		t.Fatalf("new run did not use reloaded policy: pins=%+v input=%+v: %v", newPins, newInput, err)
	}
	var children int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_lineage WHERE instance_id=$1`, testInstance).Scan(&children); err != nil || children != 1 {
		t.Fatalf("disabled new run created another child: %d: %v", children, err)
	}
	t.Log("qualification scenario 6: in-flight parent and frozen child kept their generation and permission; a new run pinned the reloaded definition and was refused by the disabled instance posture")
}

func qualificationRunInput(t *testing.T, h *qualificationHarness, run client.WorkflowRun) engine.RunInput {
	t.Helper()
	history := h.temporal.Client().GetWorkflowHistory(h.ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	event, err := history.Next()
	if err != nil {
		t.Fatal(err)
	}
	var input engine.RunInput
	if err := converter.GetDefaultDataConverter().FromPayloads(event.GetWorkflowExecutionStartedEventAttributes().GetInput(), &input); err != nil {
		t.Fatal(err)
	}
	return input
}
