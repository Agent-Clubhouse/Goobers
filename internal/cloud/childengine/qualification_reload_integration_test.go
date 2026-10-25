//go:build cloudintegration

package childengine

import (
	"encoding/json"
	"errors"
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
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/startinput"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Scenario 6 changes both the workflow goal and child publication policy while
// a real child awaits its frozen manifest response, then rolls back the
// instance posture. Existing runs retain permission; a new run is refused.
func TestChildQualificationConfigReload(t *testing.T) {
	h := newQualificationHarness(t)
	h.resolveParentInput = true
	loaded := h.InstallHTTPBarrier(func(r *http.Request) bool { return r.URL.Path == apicontract.EngineChildLoadPath })
	run := h.StartParent()
	oldPins, err := h.epoch.store.RunPinsOf(h.ctx, run.GetID())
	if err != nil {
		t.Fatal(err)
	}
	startedInput := qualificationRunInput(t, h, run)
	if !startedInput.ChildWorkflowsEnabled {
		t.Fatal("production resolver did not pin the enabled child-workflow posture")
	}
	if err := loaded.Wait(h.ctx); err != nil {
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
		t.Fatal("instance reload did not replace policy and disable child workflows")
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
	record, _, err := h.epoch.store.Accept(h.ctx, "qualification-reloaded-parent", "qualification", h.parentStartPayload(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.startResolver().Resolve(h.ctx, record); !errors.Is(err, acceptance.ErrPermanentStart) || !errors.Is(err, startinput.ErrChildWorkflowsDisabled) {
		t.Fatalf("new acceptance was not refused by the reloaded disabled posture: %v", err)
	}
	if err := h.epoch.store.RejectAccepted(h.ctx, record.ID, startinput.ErrChildWorkflowsDisabled.Error(), time.Now()); err != nil {
		t.Fatal(err)
	}
	rejected, err := h.epoch.store.Get(h.ctx, record.ID, "qualification")
	if err != nil || rejected.State != triggerqueue.Rejected {
		t.Fatalf("new acceptance state = %s: %v", rejected.State, err)
	}
	var children int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_lineage WHERE instance_id=$1`, testInstance).Scan(&children); err != nil || children != 1 {
		t.Fatalf("disabled new run created another child: %d: %v", children, err)
	}
	t.Log("qualification scenario 6: in-flight parent and frozen child kept their generation and permission; production start resolution refused a new acceptance under the reloaded disabled posture")
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
