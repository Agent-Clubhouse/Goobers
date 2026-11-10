//go:build cloudintegration

package journalequivalence

import (
	"context"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
)

// drainAtBoundary holds only the response to the committed first-stage finish.
// Worker.Stop stops workflow polling before it closes the activity stop channel;
// the activity drains successfully while the next workflow task stays queued.
// This is a worker drain, not CancelWorkflow (an operator abort).
type drainAtBoundary struct {
	*livejournal.Writer
	reached chan struct{}
	once    sync.Once
}

func (d *drainAtBoundary) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	response, err := d.Writer.Emit(ctx, req)
	if err != nil {
		return response, err
	}
	for _, op := range req.Ops {
		if op.Event != nil && op.Event.Type == journal.EventStageFinished && op.Event.Stage == "first" {
			d.once.Do(func() { close(d.reached) })
			select {
			case <-activity.GetWorkerStopChannel(ctx):
			case <-ctx.Done():
				return response, ctx.Err()
			}
		}
	}
	return response, nil
}

func compareCloudDrain(t *testing.T, scenario corpusScenario) {
	local := runScenario(t, scenario)
	backend, writer, activities, input := cloudScenario(t, scenario)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	c := server.Client()
	const queue = "journal-equivalence-drain"
	boundary := &drainAtBoundary{Writer: writer, reached: make(chan struct{})}
	activities.Journal = boundary
	first := worker.New(c, queue, worker.Options{WorkerStopTimeout: time.Minute})
	first.RegisterWorkflow(engine.Run)
	first.RegisterActivity(activities)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			first.Stop()
		}
	}()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: input.RunID, TaskQueue: queue}, engine.Run, input)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-boundary.reached:
	case <-ctx.Done():
		t.Fatal("first-stage boundary was never reached:", ctx.Err())
	}
	first.Stop()
	stopped = true
	info, err := c.DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
	if err != nil {
		t.Fatal(err)
	}
	if info.WorkflowExecutionInfo.Status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		t.Fatalf("drain terminalized workflow: %v", info.WorkflowExecutionInfo.Status)
	}
	reader, err := backend.OpenRun(ctx, journal.RunRef{ID: input.RunID})
	if err != nil {
		t.Fatal(err)
	}
	before, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 5 {
		t.Fatalf("drained journal events=%d, want 5", len(before))
	}
	if err := validateCloudEmitKeys(before, input.RunID); err != nil {
		t.Fatal(err)
	}
	assertCanonicalEqual(t, local, canonicalReader(t, scenario, reader))

	// A fresh worker consumes the queued task and continues the same run.
	replacementActivities := *activities
	replacementActivities.Journal = writer
	replacement := worker.New(c, queue, worker.Options{})
	replacement.RegisterWorkflow(engine.Run)
	replacement.RegisterActivity(&replacementActivities)
	if err := replacement.Start(); err != nil {
		t.Fatal(err)
	}
	defer replacement.Stop()
	if err := run.Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	after, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before) || !reflect.DeepEqual(before, after[:len(before)]) {
		t.Fatal("restart changed committed prefix or made no progress")
	}
	if err := validateCloudEmitKeys(after, input.RunID); err != nil {
		t.Fatal(err)
	}
	finishes := map[string]int{}
	for _, event := range after {
		if event.Type == journal.EventStageFinished {
			finishes[event.Stage]++
		}
	}
	if finishes["first"] != 1 || finishes["second"] != 1 || after[len(after)-1].Type != journal.EventRunFinished {
		t.Fatalf("restart did not finish each stage exactly once: %v", finishes)
	}
	script := activities.Det.(*cloudDeterministic).script
	script.mu.Lock()
	defer script.mu.Unlock()
	for stage, calls := range script.calls {
		if calls != 1 {
			t.Errorf("stage %s executed %d times", stage, calls)
		}
	}
}
