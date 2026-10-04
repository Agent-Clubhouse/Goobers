package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func TestChildGenerationRecoveryUsesRetainedProposalAndActualRunnerResume(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	registry := newDaemonRunnerRegistry()
	registry.setGenerationResolver(func(context.Context, journal.RunIdentity) (executionGenerationRuntime, error) {
		t.Fatal("child selected mutable named catalog")
		return executionGenerationRuntime{}, nil
	})
	registry.setChildGenerationResolver(f.launcher.resolveGeneration)
	recovered, err := registry.executionGeneration(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := recovered.runner.Resume(t.Context(), runner.ResumeInput{RunID: id.RunID, Machine: recovered.machine, GooberDigest: recovered.gooberDigest, RepoRef: recovered.repoRef})
	if err != nil || result.Phase != journal.PhaseCompleted || f.executor.calls.Load() != 1 {
		t.Fatalf("recovered child=%+v calls=%d err=%v", result, f.executor.calls.Load(), err)
	}
	f.authority.revoked.Store(true)
	if _, err = registry.executionGeneration(t.Context(), id); !errors.Is(err, childworkflow.ErrAuthorityUnavailable) {
		t.Fatalf("revoked child resumed: %v", err)
	}
	id.Child.InvocationKey = "foreign"
	if _, err = registry.executionGeneration(t.Context(), id); err == nil {
		t.Fatal("foreign retained source selected")
	}
}

func publishInterruptedChild(t *testing.T, f *actualChildFixture) journal.RunIdentity {
	t.Helper()
	_, receipt := f.state(t)
	if err := f.service.queue.BeginDispatch(t.Context(), receipt.ID); err != nil {
		t.Fatal(err)
	}
	ref, err := f.service.childReference(t.Context(), receipt)
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
	// Simulate a crash at the exact durable journal handoff boundary. No stage
	// ran; resume must recover this identity, not allocate another child RunID.
	_, err = runtime.runner.Start(t.Context(), runner.StartInput{RunID: ref.Child.RunID, Gaggle: ref.Envelope.Gaggle, Machine: proposal.Machine, GooberDigest: runtime.gooberDigest, Child: &ref.Lineage, OnJournalPublished: func() error { return errors.New("simulated handoff crash") }})
	if err == nil {
		t.Fatal("expected interrupted publication")
	}
	dir, err := f.launcher.layout.FindRunDir(ref.Child.RunID)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestChildGenerationRecoveryRefusalDefersOnlyChildAtActualStartup(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	f.authority.revoked.Store(true)
	registry := newDaemonRunnerRegistry()
	registry.setChildGenerationResolver(f.launcher.resolveGeneration)
	dir, err := f.launcher.layout.FindRunDir(id.RunID)
	if err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(logDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	var wg sync.WaitGroup
	var released []string
	release := func(runID, _ string) { released = append(released, runID) }
	outcome, err := resumeInterruptedRunsWithRunners(t.Context(), f.launcher.layout, nil, nil, registry, nil, nil, nil, nil, log, nil, nil, nil, release, &wg, nil, []string{dir})
	if err != nil || len(outcome.Warned) != 1 || outcome.Warned[0] != id.RunID || len(outcome.Resumed) != 0 || len(released) != 1 || released[0] != id.RunID || f.executor.calls.Load() != 0 {
		t.Fatalf("outcome=%+v release=%v calls=%d err=%v", outcome, released, f.executor.calls.Load(), err)
	}
	events, err := journal.ReadInstanceLog(logDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Error == nil || events[0].Error.Code != "child_recovery_deferred" {
		t.Fatalf("missing safe recovery warning: %+v", events)
	}
	// Ordinary archived generations retain the established fatal startup rule.
	ordinary := id
	ordinary.RunID = "ordinary-pinned"
	ordinary.Child = nil
	jr, err := journal.Create(f.launcher.layout.ForGaggle(id.Gaggle).RunsDir(), ordinary, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = jr.Close(); err != nil {
		t.Fatal(err)
	}
	registry.setGenerationResolver(func(context.Context, journal.RunIdentity) (executionGenerationRuntime, error) {
		return executionGenerationRuntime{}, errors.New("missing ordinary archive")
	})
	dir, err = f.launcher.layout.FindRunDir(ordinary.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resumeInterruptedRunsWithRunners(t.Context(), f.launcher.layout, nil, nil, registry, nil, nil, nil, nil, nil, nil, nil, nil, release, &wg, nil, []string{dir}); err == nil {
		t.Fatal("ordinary archive error silently deferred")
	}
}
