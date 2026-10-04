package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
)

type launchAuthority struct {
	authority childworkflow.Authority
	held      atomic.Int32
	revoked   atomic.Bool
}

func (a *launchAuthority) AcquirePinnedStage(context.Context, journal.RunIdentity, string) (childworkflow.PinnedStageAdmission, func(), error) {
	if a.revoked.Load() {
		return childworkflow.PinnedStageAdmission{}, nil, childworkflow.ErrAuthorityUnavailable
	}
	a.held.Add(1)
	var once sync.Once
	return childworkflow.PinnedStageAdmission{Admission: a.authority.Admission, ConfigGeneration: a.authority.ConfigGeneration, WorkflowDigest: a.authority.ParentWorkflowDigest, GooberDigest: a.authority.ParentGooberDigest}, func() { once.Do(func() { a.held.Add(-1) }) }, nil
}

type launchDeterministic struct {
	calls   atomic.Int32
	entered chan struct{}
	proceed chan struct{}
}

func (d *launchDeterministic) Run(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.calls.Add(1)
	if d.entered != nil {
		close(d.entered)
		select {
		case <-d.proceed:
		case <-ctx.Done():
			return apiv1.ResultEnvelope{}, ctx.Err()
		}
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

type actualChildFixture struct {
	childDrainFixture
	launcher  *queuedChildLauncher
	authority *launchAuthority
	executor  *launchDeterministic
	wg        sync.WaitGroup
	builds    atomic.Int32
	releases  atomic.Int32
}

func actualChildLaunchFixture(t *testing.T, customSource ...string) *actualChildFixture {
	t.Helper()
	source := strings.Replace(dispatchChildSource, "      type: deterministic", "      type: deterministic\n      workspace: scratch", 1)
	if len(customSource) > 0 {
		source = customSource[0]
	}
	f := &actualChildFixture{childDrainFixture: newChildDrainFixture(t, source)}
	a := f.childDrainFixture.launcher.authority
	f.authority = &launchAuthority{authority: a}
	layout := f.childDrainFixture.launcher.layout
	parent, err := journal.Create(layout.ForGaggle(a.Origin.Gaggle).RunsDir(), journal.RunIdentity{RunID: a.Origin.RunID, Gaggle: a.Origin.Gaggle, Workflow: a.ParentWorkflow, WorkflowDigest: a.ParentWorkflowDigest, GooberDigest: a.ParentGooberDigest, ConfigGeneration: a.ConfigGeneration}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = parent.Close(); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(filepath.Join(layout.Root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	sched := localscheduler.New([]localscheduler.WorkflowEntry{{Gaggle: a.Origin.Gaggle, Workflow: a.ParentWorkflow, RepoRef: a.Admission.Gaggle.Spec.Project, Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 3}}}, log)
	f.service.dispatch.AttachScheduler(sched)
	manager, err := worktree.NewManager(filepath.Join(layout.Root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	f.executor = &launchDeterministic{}
	rn, err := runner.New(runner.Config{RunsDir: layout.ForGaggle(a.Origin.Gaggle).RunsDir(), ScratchDir: filepath.Join(layout.Root, "scratch"), Worktrees: manager, ConfigGeneration: a.ConfigGeneration,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			t.Error("scratch accessed repository")
			return "", errors.New("scratch accessed repository")
		},
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return f.executor, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.launcher = &queuedChildLauncher{layout: layout, queue: f.service.queue, authority: f.authority, dispatch: f.service.dispatch, runners: newDaemonRunnerRegistry(), wg: &f.wg,
		build: func(_ context.Context, start childExecutionStart) (preparedChildRuntime, error) {
			f.builds.Add(1)
			return preparedChildRuntime{executionGenerationRuntime: executionGenerationRuntime{runner: rn, machine: start.Proposal.Machine, gooberDigest: a.ParentGooberDigest, repoRef: a.Admission.Gaggle.Spec.Project},
				entry: localscheduler.WorkflowEntry{Gaggle: a.Origin.Gaggle, Workflow: start.Envelope.Workflow, RepoRef: a.Admission.Gaggle.Spec.Project}, worktrees: manager, release: func() { f.releases.Add(1) }}, nil
		}}
	f.service.children = f.launcher
	t.Cleanup(func() { f.wg.Wait() })
	return f
}

func TestChildLauncherDrainPublishesRealRunnerOnceAcrossQueueReopen(t *testing.T) {
	f := actualChildLaunchFixture(t)
	f.executor.entered = make(chan struct{})
	f.executor.proceed = make(chan struct{})
	defer close(f.executor.proceed)
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.executor.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("runner never invoked")
	}
	if f.authority.held.Load() != 0 {
		t.Fatal("applied-policy lease held through execution")
	}
	if f.releases.Load() != 0 {
		t.Fatal("archive released before executor joined")
	}
	_, record := f.state(t)
	if record.State != triggerqueue.Dispatching {
		t.Fatalf("receipt state=%s", record.State)
	}
	if err := f.service.queue.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = acceptedService(t, f.path, newDaemonTriggerService())
	f.launcher.queue = f.service.queue
	f.service.children = f.launcher
	f.service.observeChild = acceptedChildObserver(f.launcher.layout)
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.executor.calls.Load() != 1 || f.builds.Load() != 1 {
		t.Fatal("replayed observed real start")
	}
	child, _ := f.state(t)
	if child.State != triggerqueue.ChildRunning {
		t.Fatal(child.State)
	}
}

func TestChildLauncherRechecksCurrentPolicyAndDoesNotSpendCapacityWhenRefused(t *testing.T) {
	f := actualChildLaunchFixture(t)
	f.authority.revoked.Store(true)
	if err := f.service.Drain(t.Context()); !errors.Is(err, childworkflow.ErrAuthorityUnavailable) {
		t.Fatal(err)
	}
	_, record := f.state(t)
	if record.State != triggerqueue.Accepted || f.builds.Load() != 0 {
		t.Fatal("revoked execution advanced")
	}
	f.authority.revoked.Store(false)
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wg.Wait()
	if f.executor.calls.Load() != 1 || f.releases.Load() != 1 {
		t.Fatal("runner or retained lease ownership incorrect")
	}
}

func TestChildLauncherParentCapacityDefersBeforeRunner(t *testing.T) {
	f := actualChildLaunchFixture(t)
	e := f.submission.Envelope
	sched := f.launcher.dispatch.sched.Load()
	release, ok, reason := sched.ReserveContinuation(e.ParentRunID, e.Gaggle, e.ParentWorkflow)
	if !ok {
		t.Fatal(reason)
	}
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, record := f.state(t)
	if record.State != triggerqueue.Accepted || f.executor.calls.Load() != 0 {
		t.Fatal("child bypassed parent capacity")
	}
	wait, err := sched.SuspendChildParent(t.Context(), e.ParentRunID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wg.Wait()
	if err = wait.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	release()
	if f.executor.calls.Load() != 1 {
		t.Fatal("child failed after parent suspended")
	}
}

func TestChildLauncherCancellationBetweenPreparationAndPublicationFencesEffects(t *testing.T) {
	f := actualChildLaunchFixture(t)
	build := f.launcher.build
	f.launcher.build = func(ctx context.Context, start childExecutionStart) (preparedChildRuntime, error) {
		if err := f.service.queue.FenceChildParent(ctx, start.Child.Identity.ChildParent, "human", time.Now()); err != nil {
			return preparedChildRuntime{}, err
		}
		return build(ctx, start)
	}
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.executor.calls.Load() != 0 || f.releases.Load() != 1 {
		t.Fatal("fenced child reached executor or leaked archive")
	}
	dir, err := acceptedTriggerJournalDir(t.Context(), f.launcher.layout, f.submission.Child.RunID)
	if err != nil || dir != "" {
		t.Fatalf("cancelled child published: %s %v", dir, err)
	}
}

func TestChildJournalBarrierFailureKeepsIdentityWithoutStageEffects(t *testing.T) {
	f := actualChildLaunchFixture(t)
	_, record := f.state(t)
	ref, err := f.service.childReference(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	source, err := f.service.queue.ChildProposal(t.Context(), ref.Child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := childworkflow.ValidateRetainedStart(f.authority.authority, ref.Envelope, source.Source)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := f.launcher.build(t.Context(), childExecutionStart{childExecutionRef: ref, Proposal: proposal})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.release()
	refused := errors.New("publication transaction aborted")
	_, err = runtime.runner.Start(t.Context(), runner.StartInput{RunID: ref.Child.RunID, Gaggle: ref.Envelope.Gaggle, Machine: proposal.Machine, GooberDigest: runtime.gooberDigest, Child: &ref.Lineage,
		OnJournalPublished: func() error {
			observed, observeErr := acceptedChildObserver(f.launcher.layout)(t.Context(), ref)
			if observeErr != nil || !observed {
				t.Errorf("barrier preceded identity: %v %v", observed, observeErr)
			}
			return refused
		},
	})
	if !errors.Is(err, refused) || f.executor.calls.Load() != 0 {
		t.Fatalf("barrier executed stage: %d %v", f.executor.calls.Load(), err)
	}
}
