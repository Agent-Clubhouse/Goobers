//go:build cloudintegration

package childengine

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
)

// Scenario 6 changes both the workflow goal and child publication policy while
// a real child is started but has not yet loaded its frozen manifest.
func TestChildQualificationConfigReload(t *testing.T) {
	h := newQualificationHarness(t)
	loaded, release := qualificationGate(t, h, apicontract.EngineChildLoadPath)
	run := h.StartParent()
	qualificationWait(t, h, loaded)
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
	release()
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
	candidate, _ := h.applied.Applied()
	_, machine, err := childworkflow.StageCatalog(candidate.Config, candidate.Set, childworkflow.ParentSelection{Gaggle: "example", Workflow: "default-implement", Stage: "plan"}, childworkflow.BackendEngine, childworkflow.OfflineGooberAdmission)
	if err != nil {
		t.Fatal(err)
	}
	next := h.input
	next.RunID = ""
	next.ConfigGeneration, next.Spec = generation, machine.Def.Spec
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
	qualificationCompleted(t, h, newRun)
	newInput := qualificationRunInput(t, h, newRun)
	newPins, err := h.epoch.store.RunPinsOf(h.ctx, started.RunID)
	if err != nil || newPins.ConfigGeneration != generation || newPins.WorkflowDigest == oldPins.WorkflowDigest || newInput.ConfigGeneration != generation || newInput.Spec.Tasks[0].ChildWorkflows.AllowPRPublication || newInput.Spec.Tasks[0].Goal != "Plan using the reloaded configuration" {
		t.Fatalf("new run did not use reloaded policy: pins=%+v input=%+v: %v", newPins, newInput, err)
	}
	t.Log("qualification scenario 6: in-flight parent and frozen child kept their generation and permission; a new completed run used the reloaded goal and child policy")
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

// Gate only transport timing; the original authenticated production handler
// still performs every operation. Cleanup releases blocked requests first.
func qualificationGate(t *testing.T, h *qualificationHarness, path string) (<-chan struct{}, func()) {
	t.Helper()
	reached, proceed := make(chan struct{}), make(chan struct{})
	var seen, released sync.Once
	release := func() { released.Do(func() { close(proceed) }) }
	t.Cleanup(release)
	h.routerMu.Lock()
	inner := h.handler
	h.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path {
			seen.Do(func() { close(reached) })
			select {
			case <-proceed:
			case <-r.Context().Done():
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
	h.routerMu.Unlock()
	return reached, release
}

func qualificationWait(t *testing.T, h *qualificationHarness, reached <-chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-h.ctx.Done():
		t.Fatal("durable qualification boundary: ", h.ctx.Err())
	}
}
