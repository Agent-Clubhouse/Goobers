package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Exercise the actual queue, retained generation builder, launcher, runner,
// authenticated worker plane and terminal capture together. The Temporal worker
// client remains the transport fixture; this does not qualify a real pod/PID 1.
func testQueuedChildFactory(t *testing.T, f childKitFixture, worker *factoryWorkerClient) {
	t.Helper()
	s := f.writer.service
	keepChildLaunchSnapshotFixture(t, s.childQueue, f.child)
	store, err := executionGenerationStore(s.layout)
	if err != nil {
		t.Fatal(err)
	}
	retainer := &configgeneration.Retainer{Store: store}
	t.Cleanup(func() { _ = retainer.Close() })
	registry := newDaemonRunnerRegistry()
	dispatch := newDaemonTriggerService()
	dispatch.AttachDispatchContext(t.Context())
	dispatch.AttachScheduler(localscheduler.New([]localscheduler.WorkflowEntry{{
		Gaggle: f.child.Identity.Gaggle, Workflow: f.writer.identity.Child.ParentWorkflow, RepoRef: f.parent.applied.Gaggles[0].Spec.Project,
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 3},
	}}, s.log))
	triggers := &durableTriggerService{queue: s.childQueue, dispatch: dispatch, observeChild: acceptedChildObserver(s.layout)}
	build := func(layout instance.Layout, generation string, set *instance.ConfigSet, report *validate.Report) (*schedulerDefinitions, error) {
		return buildSchedulerDefinitions(schedulerDefinitionsInput{Layout: layout, Config: s.config, Definitions: set, Validation: report,
			RunnerRegistry: registry, ProviderQuota: localscheduler.NewProviderQuotaState(), Generations: []*configgeneration.Retainer{retainer}, PinnedGeneration: generation})
	}
	var wg sync.WaitGroup
	setup := &schedulerSetup{RunnerRegistry: registry, ChildRuntime: childRuntimeBuilderFor(s.layout, retainer, s.config, build)}
	if err := s.installQueuedChildren(setup, triggers, &wg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wg.Wait)
	before, err := s.childQueue.ChildStart(t.Context(), f.child.Identity)
	if err != nil || before.State != triggerqueue.Accepted {
		t.Fatal("fixture did not preserve an accepted, unstarted request", before, err)
	}
	if dir, err := acceptedTriggerJournalDir(t.Context(), s.layout, f.child.RunID); err != nil || dir != "" {
		t.Fatal("child journal existed before durable dispatch", dir, err)
	}
	if err := triggers.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if worker.starts != 1 {
		receipt, _ := s.childQueue.ChildStart(t.Context(), f.child.Identity)
		dir, _ := acceptedTriggerJournalDir(t.Context(), s.layout, f.child.RunID)
		t.Fatal("queued child did not reach its contained worker exactly once", worker.starts, receipt.State, receipt.Reason, dir)
	}
	wantStarts := 1
	if worker.lostReply {
		recoverQueuedFactoryWorker(t, triggers, worker, &wg, f.child.Identity)
		wantStarts = 2
	}
	// One bounded cursor cycle first reaches the end of the active-child list,
	// then revisits this child to capture joined terminal custody. Neither
	// sweep may dispatch the accepted source a second time.
	for i := 0; i < 2; i++ {
		if err := triggers.Drain(t.Context()); err != nil {
			logQueuedFactoryJournal(t, s, f.child.RunID)
			t.Fatal("terminal reconciliation", err, "starts", worker.starts, "gets", worker.gets, "report", worker.result, "worker error", worker.executeErr)
		}
	}
	child, err := s.childQueue.GetChild(t.Context(), f.child.Identity)
	if err != nil || child.State != triggerqueue.ChildCompleted || child.ResultRef == "" || worker.starts != wantStarts {
		t.Fatal("queue did not retain the real worker result", child, err, worker.starts)
	}
	dir, err := s.layout.FindRunDir(child.RunID)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil || journal.PhaseFromEvents(events) != journal.PhaseCompleted {
		t.Fatal("queue completion lacks a completed journal", err)
	}
	if err := triggers.Drain(context.Background()); err != nil || worker.starts != wantStarts {
		t.Fatal("completed receipt replayed execution", err, worker.starts)
	}
}

func recoverQueuedFactoryWorker(t *testing.T, triggers *durableTriggerService, worker *factoryWorkerClient, wg *sync.WaitGroup, id triggerqueue.ChildIdentity) {
	t.Helper()
	worker.getErr = errors.New("original worker outcome unavailable")
	var observedErr error
	for range 2 {
		observedErr = errors.Join(observedErr, triggers.Drain(t.Context()))
		wg.Wait()
	}
	child, err := triggers.queue.GetChild(t.Context(), id)
	if err != nil || observedErr == nil || worker.starts != 1 || worker.gets != 1 || child.State != triggerqueue.ChildRunning || child.ResultRef != "" {
		t.Fatal("unknown worker was replaced or reported complete", child.State, child.ResultRef, err, observedErr, worker.starts, worker.gets)
	}
	worker.getErr, worker.lostReply = nil, false
	for range 2 {
		if err := triggers.Drain(t.Context()); err != nil {
			t.Fatal("queue could not rejoin original worker", err)
		}
		wg.Wait()
	}
	if worker.starts != 2 || worker.gets != 2 {
		t.Fatal("resume did not follow exact original-worker recovery", worker.starts, worker.gets)
	}
}

func logQueuedFactoryJournal(t *testing.T, s *daemonCredentialService, runID string) {
	t.Helper()
	dir, err := s.layout.FindRunDir(runID)
	if err != nil {
		t.Log(err)
		return
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Log(err)
		return
	}
	events, err := rd.Events()
	if err != nil {
		t.Log(err)
		return
	}
	for _, event := range events {
		t.Logf("%d %s %s attempt%d status%s reason%s runner%v", event.Seq, event.Type, event.Stage, event.Attempt, event.Status, event.Reason, event.Runner)
	}
}
