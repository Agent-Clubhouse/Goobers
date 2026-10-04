package main

import (
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestHumanChildRuntimePolicyLeaseWaitsForNestedPodJoin(t *testing.T) {
	service, pinned, gaggle := humanChildCredentialFixture(t)
	ctx, closeRuntime, err := service.beginChildRestartAuthority(t.Context(), pinned.identity, pinned.defs)
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := invoke.WithWorkspaceQuiescence(ctx)
	ack := invoke.RegisterWorkspaceWriter(inner)
	gaggle.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	done := make(chan error, 1)
	go func() { done <- service.interactive.Apply([]apiv1.Gaggle{gaggle}, nil) }()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("reload did not cancel entire child epoch")
	}
	select {
	case err := <-done:
		t.Fatal("policy published before worker joined", err)
	default:
	}
	ack(nil)
	if err := closeRuntime(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("policy waited after acknowledged teardown")
	}
}

func TestHumanChildRuntimeUnknownPodPinsRevokedLease(t *testing.T) {
	service, pinned, _ := humanChildCredentialFixture(t)
	ctx, closeRuntime, err := service.beginChildRestartAuthority(t.Context(), pinned.identity, pinned.defs)
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := invoke.WithWorkspaceQuiescence(ctx)
	ack := invoke.RegisterWorkspaceWriter(inner)
	if err := closeRuntime(); !errors.Is(err, interactiveaccess.ErrExecutionNotJoined) {
		t.Fatal("unjoined worker released lease", err)
	}
	ack(nil)
	if err := closeRuntime(); !errors.Is(err, interactiveaccess.ErrExecutionNotJoined) {
		t.Fatal("late unverified acknowledgement cleared failed custody", err)
	}
	if ctx.Err() == nil {
		t.Fatal("failed lease kept credential authority")
	}
}

func TestHumanChildEpochBlobAndFactoryUseExactExecutionJournal(t *testing.T) {
	service, pinned, _ := humanChildCredentialFixture(t)
	id := pinned.identity
	ref, err := retainedChildExecutionRef(t.Context(), service.childQueue, id, true)
	if err != nil || ref.runID() != id.RunID || ref.Execution == nil || ref.executionIdentity(id.GooberDigest).ValidateChildLineage() != nil {
		t.Fatal(ref, err)
	}
	if err := service.verifyChildBlobOwner(t.Context(), ref.Child, id.RunID); err != nil {
		t.Fatal(err)
	}
	dir, err := service.layout.FindRunDir(id.RunID)
	if err != nil {
		t.Fatal(err)
	}
	writer, _, err := journal.TryRecover(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	factory := childPodFactory{start: childExecutionStart{childExecutionRef: ref}, runtime: preparedChildRuntime{executionGenerationRuntime: executionGenerationRuntime{gooberDigest: id.GooberDigest}}}
	if executor, err := factory.executor(writer, "coder"); err != nil || executor.identity.RunID != id.RunID {
		t.Fatal(executor, err)
	}
	factory.start.Execution = nil
	if _, err := factory.executor(writer, "coder"); err == nil {
		t.Fatal("original factory adopted human execution")
	}
}
