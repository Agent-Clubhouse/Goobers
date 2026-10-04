package main

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func TestChildGenerationRecoveryUsesRetainedProposalAndActualRunnerResume(t *testing.T) {
	f := actualChildLaunchFixture(t)
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
