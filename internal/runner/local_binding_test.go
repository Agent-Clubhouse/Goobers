package runner

import (
	"context"
	"errors"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
)

func TestConcurrentBranchLaunchBindingsSurviveJournalRecovery(t *testing.T) {
	id := journal.RunIdentity{RunID: "parallel-launch", Workflow: "parallel", WorkflowVersion: 1, WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goobers")), Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerManual}}
	run, err := journal.Create(t.TempDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := launchreceipt.LocalRecorder{Root: t.TempDir()}
	type result struct {
		binding launchreceipt.Binding
		err     error
	}
	results := make(chan result, 8)
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	for branch := 1; branch <= 8; branch++ {
		wg.Add(1)
		go func(branch int) {
			defer wg.Done()
			<-barrier
			source := &branchJournal{run: run, branch: branch}
			event := journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1}
			seq, err := source.AppendWithSeq(event)
			if err != nil {
				results <- result{err: err}
				return
			}
			ctx := launchreceipt.WithJournalStart(context.Background(), source, event, seq)
			binding, _ := launchreceipt.ContextBinding(ctx)
			err = launchreceipt.RecordLocalInvocation(ctx, recorder, apiv1.InvocationEnvelope{RunID: id.RunID, TaskID: "build", Attempt: 1}, false, launchreceipt.PreparedLocal("deterministic"))
			results <- result{binding: binding, err: err}
		}(branch)
	}
	close(barrier)
	wg.Wait()
	close(results)
	bindings := make(map[uint64]launchreceipt.Binding)
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if _, ok := bindings[result.binding.StartedSeq]; ok {
			t.Fatal("branches shared a start sequence")
		}
		bindings[result.binding.StartedSeq] = result.binding
	}
	directory := run.Dir()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := journal.Recover(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recovered.Close() }()
	reader, err := journal.OpenRead(directory)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type != journal.EventStageStarted {
			continue
		}
		binding, ok := bindings[event.Seq]
		if !ok || binding.Branch != event.Branch || binding.AttemptID != journal.StageAttemptID(id.RunID, event.Branch, event.Stage, event.Seq) || binding.WorkflowDigest != id.WorkflowDigest || binding.GooberDigest != id.GooberDigest {
			t.Fatalf("binding diverged: %+v / %+v", binding, event)
		}
		count++
	}
	if count != 8 {
		t.Fatalf("durable starts=%d", count)
	}
	event := journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 2, Branch: 1}
	seq, err := recovered.AppendWithSeq(event)
	if err != nil {
		t.Fatal(err)
	}
	ctx := launchreceipt.WithJournalStart(t.Context(), recovered, event, seq)
	binding, _ := launchreceipt.ContextBinding(ctx)
	if _, ok := bindings[binding.StartedSeq]; ok {
		t.Fatal("continuation reused old start")
	}
	if err := launchreceipt.RecordLocalInvocation(ctx, recorder, apiv1.InvocationEnvelope{RunID: id.RunID, TaskID: "build", Attempt: 2}, false, launchreceipt.PreparedLocal("deterministic")); err != nil {
		t.Fatal(err)
	}
	for _, old := range bindings {
		facts := launchreceipt.PreparedLocal("deterministic")
		if err := recorder.Record(t.Context(), launchreceipt.Receipt{Version: 1, Binding: old, Local: &facts}); !errors.Is(err, launchreceipt.ErrUsed) {
			t.Fatalf("recovered consumed attempt: %v", err)
		}
	}
}
