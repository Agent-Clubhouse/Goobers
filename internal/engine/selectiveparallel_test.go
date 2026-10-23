package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestStaticSchedulerSelectiveParallelUsesFrozenSelection(t *testing.T) {
	in, acts, calls := selectiveParallelFixture(t, `{
		"schemaVersion": 1,
		"branches": [
			{"name": "gamma", "reason": "owns the affected package"}
		]
	}`)
	out := executeScheduler(t, in, acts)
	if out.Error != "" || out.Result.Status != "completed" {
		t.Fatalf("selective parallel: %+v", out)
	}
	gotCalls := calls.snapshot()
	if gotCalls["alpha"] != 1 || gotCalls["beta"] != 0 || gotCalls["gamma"] != 1 {
		t.Fatalf("branch calls = %v, want required alpha and selected gamma only", gotCalls)
	}

	var started *journal.Event
	branchStarts := map[string]bool{}
	for _, op := range out.Projection.Ops {
		event := op.Event
		if event == nil {
			continue
		}
		if event.Type == journal.EventParallelStarted {
			started = event
		}
		if event.Type == journal.EventBranchStarted {
			branchStarts[event.Name] = true
		}
	}
	if started == nil || started.Selection == nil {
		t.Fatalf("parallel.started selection = %+v", started)
	}
	selection := started.Selection
	if len(selection.Selected) != 1 || selection.Selected[0] != "gamma" ||
		selection.Reasons["gamma"] != "owns the affected package" {
		t.Fatalf("frozen selection = %+v", selection)
	}
	if branchStarts["beta"] {
		t.Fatal("unselected optional beta emitted branch.started")
	}
	if len(started.Completeness) != 3 ||
		started.Completeness[1].Status != journal.BranchSkipped ||
		started.Completeness[1].Reason != "not selected by seed.selection" {
		t.Fatalf("parallel.started completeness = %+v", started.Completeness)
	}
}

func TestStaticSchedulerSelectiveParallelAllOptionalSkipped(t *testing.T) {
	in, acts, calls := selectiveParallelFixture(t, `{"schemaVersion":1,"branches":[]}`)
	out := executeScheduler(t, in, acts)
	if out.Error != "" || out.Result.Status != "completed" {
		t.Fatalf("selective parallel: %+v", out)
	}
	gotCalls := calls.snapshot()
	if gotCalls["alpha"] != 1 || gotCalls["beta"] != 0 || gotCalls["gamma"] != 0 {
		t.Fatalf("branch calls = %v, want only required alpha", gotCalls)
	}
	for _, op := range out.Projection.Ops {
		event := op.Event
		if event != nil && event.Type == journal.EventParallelStarted {
			if event.Selection == nil || len(event.Selection.Selected) != 0 ||
				event.Completeness[1].Status != journal.BranchSkipped ||
				event.Completeness[2].Status != journal.BranchSkipped {
				t.Fatalf("parallel.started = %+v", event)
			}
			return
		}
	}
	t.Fatal("parallel.started not recorded")
}

func TestStaticSchedulerSelectiveParallelSourceFailureRoutesOnFailure(t *testing.T) {
	in, acts, calls := selectiveParallelFixture(t, `{"schemaVersion":1,"branches":[]}`)
	in.Run.Spec.Tasks[0].ArtifactSlots[0].MaxSize = 1
	in.Run.Spec.Parallels[0].FailurePolicy = apiv1.BranchFailFast
	in.Run.Spec.Parallels[0].OnFailure = "join"
	out := executeScheduler(t, in, acts)
	if out.Error != "" || out.Result.Status != "completed" {
		t.Fatalf("selection source failure: %+v", out)
	}
	gotCalls := calls.snapshot()
	if gotCalls["alpha"] != 0 || gotCalls["beta"] != 0 || gotCalls["gamma"] != 0 ||
		gotCalls["join"] != 1 {
		t.Fatalf("calls after source failure = %v, want failure route to join", gotCalls)
	}
	for _, op := range out.Projection.Ops {
		event := op.Event
		if event != nil && event.Type == journal.EventError && event.Parallel == "parallel" &&
			event.Error != nil && event.Error.Code == "fanout_source_invalid" {
			return
		}
	}
	t.Fatal("selection source failure did not record fanout_source_invalid")
}

func TestStaticSchedulerSelectiveParallelLegacyVersionRefusesBeforeFreeze(t *testing.T) {
	in, acts, _ := selectiveParallelFixture(t, `{"schemaVersion":1,"branches":[]}`)
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.OnGetVersion(staticSelectionVersion, workflow.DefaultVersion, 1).
		Return(workflow.DefaultVersion)
	env.RegisterActivity(acts)
	env.ExecuteWorkflow(staticScheduler, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var out staticSchedulerResult
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Error, "selective parallel execution is refused") {
		t.Fatalf("legacy selection version = %+v", out)
	}
	for _, op := range out.Projection.Ops {
		if op.Event != nil && op.Event.Type == journal.EventParallelStarted {
			t.Fatalf("legacy selection recorded parallel.started: %+v", op.Event)
		}
	}
}

type selectiveCalls struct {
	mu     sync.Mutex
	stages map[string]int
}

func (c *selectiveCalls) add(stage string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stages[stage]++
}

func (c *selectiveCalls) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.stages))
	for stage, count := range c.stages {
		out[stage] = count
	}
	return out
}

func selectiveParallelFixture(
	t *testing.T,
	selection string,
) (staticSchedulerInput, *Activities, *selectiveCalls) {
	t.Helper()
	in := schedulerInput(t.Name(), 2)
	in.Run.DSLVersion = "3.1"
	parallel := &in.Run.Spec.Parallels[0]
	parallel.Branches[1].Optional = true
	parallel.Branches[2].Optional = true
	parallel.Selection = &apiv1.ParallelSelection{
		Source: apiv1.FanOutSource{Task: "seed", Slot: "selection"},
	}
	source := []byte(selection)
	seed := &in.Run.Spec.Tasks[0]
	seed.ArtifactSlots = []apiv1.ArtifactSlot{{
		Name: "selection", MediaType: fanout.SourceMediaType, MaxSize: fanout.MaxSourceBytes,
	}}
	pointer := apiv1.ArtifactPointer{
		Path: "artifacts/selection.json", Digest: journal.Digest(source),
		MediaType: fanout.SourceMediaType, Size: int64(len(source)),
		Integrity: apiv1.IntegrityUnapproved,
	}
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), pointer.Digest, source); err != nil {
		t.Fatal(err)
	}
	writer, _ := newLiveWriter(t)
	calls := &selectiveCalls{stages: map[string]int{}}
	acts := &Activities{
		Journal: writer, Workspaces: testWorkspaces(t), FanOutBlobs: store,
		Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			stage := env.TaskID[len(env.RunID)+1:]
			calls.add(stage)
			result := apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Outputs: map[string]any{"value": stage},
			}
			if stage == "seed" {
				result.Artifacts = []apiv1.ArtifactPointer{pointer}
			}
			return result, nil
		}},
	}
	return in, acts, calls
}
