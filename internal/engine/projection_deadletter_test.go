package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/sdk/converter"
)

type deadLetterClient struct {
	completedRunFake
	queryErr     error
	executionIDs []string
}

func (f *deadLetterClient) QueryWorkflow(ctx context.Context, wid, rid, query string, args ...interface{}) (converter.EncodedValue, error) {
	f.executionIDs = append(f.executionIDs, rid)
	if f.queryErr != nil {
		f.queries++
		return nil, f.queryErr
	}
	return f.completedRunFake.QueryWorkflow(ctx, wid, rid, query, args...)
}
func deadLetterFixture(t *testing.T) (*deadLetterClient, string) {
	t.Helper()
	spec := retrySpec(nil)
	proj := executeForProjection(t, runInput("dead-letter", spec), &Activities{Det: &scriptedDeterministic{}, Workspaces: testWorkspaces(t)}, false)
	// Model a worker that closed after recording the final task but without
	// recording run.finished. The remaining projection is otherwise valid.
	proj.Ops = proj.Ops[:len(proj.Ops)-1]
	memo, err := converter.GetDefaultDataConverter().ToPayload(proj.Identity.Gaggle)
	if err != nil {
		t.Fatal(err)
	}
	return &deadLetterClient{completedRunFake: completedRunFake{projection: proj, executions: []*workflowpb.WorkflowExecutionInfo{{
		Execution: &commonpb.WorkflowExecution{WorkflowId: proj.Identity.RunID, RunId: "execution-one"},
		Memo:      &commonpb.Memo{Fields: map[string]*commonpb.Payload{RunGaggleMemoKey: memo}},
		Status:    enumspb.WORKFLOW_EXECUTION_STATUS_FAILED,
	}}}}, t.TempDir()
}
func newDeadLetterReconciler(t *testing.T, f *deadLetterClient, runsDir string) *CompletedRunReconciler {
	t.Helper()
	r, err := NewCompletedRunReconciler(f, "default", map[string]string{f.projection.Identity.Gaggle: runsDir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestProjectionDeadLetterPersistsAndScopesToExecution(t *testing.T) {
	f, dir := deadLetterFixture(t)
	r := newDeadLetterReconciler(t, f, dir)
	if _, err := r.Reconcile(context.Background()); !errors.Is(err, ErrUnprojectable) || !strings.Contains(err.Error(), "dead-lettered") {
		t.Fatalf("first failure=%v", err)
	}
	calls := f.queries
	for i := 0; i < 2; i++ {
		r = newDeadLetterReconciler(t, f, dir) // restart must retain suppression
		if n, err := r.Reconcile(context.Background()); err != nil || n != 0 {
			t.Fatalf("repeat=(%d,%v)", n, err)
		}
	}
	if f.queries != calls {
		t.Fatal("dead-lettered execution was queried again")
	}
	files, err := filepath.Glob(filepath.Join(dir, projectionDeadLetterDirectory, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("dead-letter records=%v err=%v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var record ProjectionDeadLetter
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.ExecutionID != "execution-one" || record.RecordedAt.IsZero() || !strings.Contains(record.Reason, "no terminal run.finished") {
		t.Fatalf("operator record=%+v", record)
	}
	// Reusing a workflow ID does not suppress its distinct Temporal execution.
	f.executions[0].Execution.RunId = "execution-two"
	if _, err := r.Reconcile(context.Background()); !errors.Is(err, ErrUnprojectable) {
		t.Fatalf("new execution failure=%v", err)
	}
	if f.queries <= calls {
		t.Fatal("new execution was suppressed")
	}
	for i, id := range f.executionIDs {
		if id != "execution-one" && id != "execution-two" {
			t.Fatalf("query %d targeted %q", i, id)
		}
	}
	// Removing the operator record deliberately re-enables the old execution.
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	f.executions[0].Execution.RunId = "execution-one"
	if _, err := r.Reconcile(context.Background()); !errors.Is(err, ErrUnprojectable) {
		t.Fatalf("manual retry=%v", err)
	}
}
func TestProjectionDeadLetterKeepsTransientAndUnconfirmedFailuresRetryable(t *testing.T) {
	for _, mode := range []string{"query unavailable", "status unknown", "execution unknown", "persistence failed"} {
		t.Run(mode, func(t *testing.T) {
			f, dir := deadLetterFixture(t)
			switch mode {
			case "query unavailable":
				f.queryErr = errors.New("frontend unavailable")
			case "status unknown":
				f.executions[0].Status = enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED
			case "execution unknown":
				f.executions[0].Execution.RunId = ""
			case "persistence failed":
				if err := os.WriteFile(filepath.Join(dir, projectionDeadLetterDirectory), []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			r := newDeadLetterReconciler(t, f, dir)
			for i := 0; i < 2; i++ {
				if _, err := r.Reconcile(context.Background()); err == nil {
					t.Fatal("failure silently skipped")
				}
			}
			if f.queries < 2 && mode != "persistence failed" {
				t.Fatalf("failure was suppressed: queries=%d", f.queries)
			}
		})
	}
}
func TestProjectionDeadLetterSkipsRunningExecutions(t *testing.T) {
	f, dir := deadLetterFixture(t)
	f.executions[0].Status = enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
	r := newDeadLetterReconciler(t, f, dir)
	if n, err := r.Reconcile(context.Background()); n != 0 || err != nil {
		t.Fatalf("running=(%d,%v)", n, err)
	}
	if f.queries != 0 {
		t.Fatal("running execution queried")
	}
}
func TestProjectionDeadLetterDoesNotSuppressValidClosedRun(t *testing.T) {
	f, dir := deadLetterFixture(t)
	f.projection = executeForProjection(t, runInput("dead-letter", retrySpec(nil)), &Activities{Det: &scriptedDeterministic{}, Workspaces: testWorkspaces(t)}, false)
	r := newDeadLetterReconciler(t, f, dir)
	if n, err := r.Reconcile(context.Background()); n != 1 || err != nil {
		t.Fatalf("valid=(%d,%v)", n, err)
	}
	for _, id := range f.executionIDs {
		if id != "execution-one" {
			t.Fatalf("query targeted %q", id)
		}
	}
}
