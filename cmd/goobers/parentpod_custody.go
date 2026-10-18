package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// parentInvocationBlobs reserves private recovery input before the host grants
// a physical worker access to its contract. Retained input is never pod-readable.
type parentInvocationBlobs struct {
	childpod.ParentBlobs
	recorder       runner.OwnedJournalRecorder
	contract       childpod.Contract
	contractDigest string
	retainedRef    journal.Ref
	started        bool
}

func (b *parentInvocationBlobs) keepAttempt(ctx context.Context, retained childpod.RetainedAttempt) error {
	if b.started || b.retainedRef.Digest != "" {
		return errors.New("parent dispatch custody already retained")
	}
	if err := retained.Input.Validate(); err != nil {
		return err
	}
	data, err := b.Get(ctx, retained.Input.Attempt.ChildExecutionDigest)
	if err != nil {
		return err
	}
	contract, err := childpod.DecodeContract(data, retained.Input.Attempt.ChildExecutionDigest)
	if err != nil {
		return err
	}
	if err = verifyRetainedParent(contract, retained, retained.Input.Attempt.ChildExecutionDigest); err != nil {
		return err
	}
	if err = b.verifyOwner(ctx, contract); err != nil {
		return err
	}
	raw, err := json.Marshal(retained)
	if err != nil || len(raw) > childpod.MaxRetainedAttemptBytes {
		return errors.New("retained parent dispatch exceeds bound")
	}
	if err = b.Put(ctx, journal.Digest(raw), raw); err != nil {
		return err
	}
	b.retainedRef, err = childpod.RecordRetainedAttempt(b.recorder, retained)
	return err
}

func (b *parentInvocationBlobs) BindContract(ctx context.Context, digest string) error {
	if b.started || b.retainedRef.Digest == "" {
		return errors.New("parent dispatch retention unavailable or already bound")
	}
	data, err := b.Get(ctx, digest)
	if err != nil {
		return err
	}
	contract, err := childpod.DecodeContract(data, digest)
	if err != nil {
		return err
	}
	if err = b.verifyOwner(ctx, contract); err != nil {
		return err
	}
	reader, err := journal.OpenReadOnly(b.recorder.Dir())
	if err != nil {
		return err
	}
	if _, err = readParentRetainedAttempt(ctx, reader, b.ParentBlobs, b.retainedRef, contract, digest); err != nil {
		return err
	}
	if err = childpod.VerifyParentBranchCustody(ctx, reader, contract.ParentBranch); err != nil {
		return err
	}
	if err = b.ParentBlobs.BindContract(ctx, digest); err != nil {
		return err
	}
	b.contract, b.contractDigest = contract, digest
	if err = b.record(childpod.ParentWriterStarted); err != nil {
		return err
	}
	b.started = true
	return nil
}

func (b *parentInvocationBlobs) verifyOwner(ctx context.Context, contract childpod.Contract) error {
	owned, branch, err := runner.OwnedJournalScope(b.recorder)
	if err != nil {
		return err
	}
	if contract.ParentOrigin == nil || branch != contract.ParentBranch || owned.Dir() != b.RunDir || !reflect.DeepEqual(contract.Identity, b.Identity) {
		return errors.New("parent dispatch differs from journal owner")
	}
	reader, err := journal.OpenReadOnly(owned.Dir())
	if err != nil {
		return err
	}
	id, started, err := childworkflow.VerifyActiveStage(ctx, reader, b.Identity.RunID, *contract.ParentOrigin)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(id, b.Identity) || started.Seq != uint64(contract.PodAttempt) || started.Stage != contract.Stage || started.Attempt != contract.Attempt || started.Branch != branch || !started.Time.Equal(contract.StartedAt) {
		return errors.New("parent dispatch differs from active physical attempt")
	}
	return nil
}

func (b *parentInvocationBlobs) record(kind string) error {
	return b.recorder.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: b.contract.Stage, Attempt: b.contract.Attempt, Branch: b.contract.ParentBranch, Runner: map[string]any{"kind": kind, "contractDigest": b.contractDigest, "retainedAttempt": b.retainedRef}})
}

func readParentRetainedAttempt(ctx context.Context, reader *journal.Reader, store childpod.ParentBlobs, ref journal.Ref, contract childpod.Contract, digest string) (childpod.RetainedAttempt, error) {
	if ref.Integrity != apiv1.IntegrityTrusted {
		return childpod.RetainedAttempt{}, errors.New("parent recovery reference is not trusted")
	}
	retained, err := childpod.ReadRetainedAttempt(reader, ref)
	if err != nil {
		return retained, err
	}
	exact, err := store.GetBounded(ctx, ref.Digest, childpod.MaxRetainedAttemptBytes)
	if err != nil {
		return retained, err
	}
	canonical, err := json.Marshal(retained)
	if err != nil || !bytes.Equal(canonical, exact) {
		return retained, errors.New("parent recovery custody changed")
	}
	return retained, verifyRetainedParent(contract, retained, digest)
}

func verifyRetainedParent(contract childpod.Contract, retained childpod.RetainedAttempt, digest string) error {
	a := retained.Input.Attempt
	e := a.Envelope
	id := contract.Identity
	if contract.ParentOrigin == nil || !a.WorkflowParent || !a.Agentic || a.Review || a.InstanceID != id.InstanceID || a.RunID != id.RunID || a.Gaggle != id.Gaggle || a.Workflow != id.Workflow || a.Stage != contract.Stage || a.Number != contract.Attempt || a.PodAttempt != contract.PodAttempt || a.ChildExecutionDigest != digest || a.KitDigest != contract.KitDigest || e == nil || e.Workspace != "" || e.InstanceID != id.InstanceID || e.RunID != id.RunID || e.Gaggle != id.Gaggle || e.WorkflowID != id.Workflow || e.ConfigGeneration != id.ConfigGeneration || e.GooberDigest != id.GooberDigest || !reflect.DeepEqual(e.ChildWorkflowOrigin, contract.ParentOrigin) || (retained.HostSnapshot == nil) != (contract.Workspace == nil) {
		return errors.New("parent recovery input differs from physical contract")
	}
	return nil
}
