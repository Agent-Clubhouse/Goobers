package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func replayArtifactReceiptHistory(t *testing.T, history *historypb.History) {
	t.Helper()
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactReceiptsNMinusOneHistoryReplays(t *testing.T) {
	// Real serial Run captured at 2661f0209, before recorder receipts existed.
	history := loadHistoryFixture(t, "testdata/run-fanout-pre-assembly-serial.json")
	completed := 0
	for _, event := range history.Events {
		if event.GetActivityTaskCompletedEventAttributes() != nil {
			completed++
		}
		if marker := event.GetMarkerRecordedEventAttributes(); marker != nil && strings.Contains(marker.String(), artifactReceiptsChange) {
			t.Fatal("pre-change history already contains receipts marker")
		}
	}
	if completed == 0 {
		t.Fatal("N-1 fixture did not execute a stage")
	}
	replayEveryWorkflowTask(t, history, replayArtifactReceiptHistory)
}

func TestArtifactReceiptsRealHistoryAndPrefixesReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt := fixtureArtifactReceipt(t)
	if err := store.Put(ctx, receipt.Ref.Digest, []byte("artifact bytes")); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return root, true }, livejournal.WithArtifactSource(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	input := runInput("receipt-replay", fixtureSpec("work", []apiv1.Task{detTask("work", "")}, nil))
	input.RunID, input.LiveJournal = strings.Repeat("e", 32), true
	w := worker.New(c, "artifact-receipt-replay", worker.Options{})
	w.RegisterWorkflow(Run)
	w.RegisterActivity(&Activities{Journal: writer, Det: receiptExecutor{receipt: receipt}, Workspaces: testWorkspaces(t)})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: input.RunID, TaskQueue: "artifact-receipt-replay"}, Run, input)
	if err != nil {
		t.Fatal(err)
	}
	var result RunResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("result=%+v", result)
	}
	recorded := 0
	for _, event := range liveEvents(t, root, input.RunID) {
		if event.Type == journal.EventArtifactRecorded && event.Name == receipt.Name {
			recorded++
		}
	}
	if recorded != 1 {
		t.Fatalf("recorded artifacts=%d", recorded)
	}
	history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
	marked := false
	for _, event := range history.Events {
		if marker := event.GetMarkerRecordedEventAttributes(); marker != nil && strings.Contains(marker.String(), artifactReceiptsChange) {
			marked = true
		}
	}
	if !marked {
		t.Fatal("missing recorder receipt version marker")
	}
	if count := replayEveryWorkflowTask(t, history, replayArtifactReceiptHistory); count < 3 {
		t.Fatalf("only %d workflow task boundaries", count)
	}
}
