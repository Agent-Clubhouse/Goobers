package main

import (
	"bytes"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/retention"
)

func TestHumanChildContextCopiesOnlyVerifiedPriorExecution(t *testing.T) {
	service, pinned, _ := humanChildCredentialFixture(t)
	ref, err := retainedChildExecutionRef(t.Context(), service.childQueue, pinned.identity, true)
	if err != nil {
		t.Fatal(err)
	}
	currentDir, err := service.layout.FindRunDir(pinned.identity.RunID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := journal.OpenReadOnly(currentDir)
	if err != nil {
		t.Fatal(err)
	}
	sourceDir, err := service.layout.FindRunDir(ref.Execution.SourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := journal.OpenReadOnly(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, err := source.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceID.Inputs) == 0 {
		t.Fatal("source has no retained context")
	}
	artifact := sourceID.Inputs[0].Ref
	pointer := apiv1.ContextPointer{RunID: sourceID.RunID, Artifact: &apiv1.ArtifactPointer{Path: artifact.Path, Digest: artifact.Digest, Size: artifact.Size}}
	blobs := childpod.ScopedBlobs{Queue: service.childQueue, Identity: ref.Child.Identity}
	pod := childStagePod{childPodFactory: childPodFactory{service: service, start: childExecutionStart{childExecutionRef: ref}}, identity: pinned.identity}
	if err = pod.copyContext(t.Context(), current, blobs, []apiv1.ContextPointer{pointer}); err != nil {
		t.Fatal(err)
	}
	want, err := source.ArtifactBytesBounded(artifact, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got, err := blobs.Get(t.Context(), artifact.Digest)
	if err != nil || !bytes.Equal(want, got) {
		t.Fatal("prior context missing", err)
	}
	pointer.RunID = ref.Child.Identity.ParentRunID
	if err = pod.copyContext(t.Context(), current, blobs, []apiv1.ContextPointer{pointer}); err == nil {
		t.Fatal("parent context bypassed lineage")
	}
	pointer.RunID = sourceID.RunID
	pointer.Artifact.Path = "artifacts/forged"
	if err = pod.copyContext(t.Context(), current, blobs, []apiv1.ContextPointer{pointer}); err == nil {
		t.Fatal("unverified context copied")
	}
	pod.identity.Child = nil
	if err = pod.copyContext(t.Context(), current, blobs, []apiv1.ContextPointer{pointer}); err == nil {
		t.Fatal("ordinary run read prior child context")
	}
}

func TestHumanChildSourceAndEpochJournalsBlockProductionPruneGuard(t *testing.T) {
	service, pinned, _ := humanChildCredentialFixture(t)
	ref, err := retainedChildExecutionRef(t.Context(), service.childQueue, pinned.identity, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{ref.Child.RunID, ref.runID(), ref.Child.Identity.ParentRunID} {
		dir, err := service.layout.FindRunDir(run)
		if err != nil {
			t.Fatal(err)
		}
		candidate := retention.Result{RunDir: dir, RunID: run}
		if err = acknowledgeTriggerBeforePrune(t.Context(), service.childQueue, candidate, time.Now()); err == nil {
			t.Fatal("active family journal could be pruned", run)
		}
		if _, err = journal.OpenReadOnly(dir); err != nil {
			t.Fatal(err)
		}
	}
}
