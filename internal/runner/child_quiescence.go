package runner

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/workspaceauthority"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

const childWriterPolicyInput = workspaceauthority.ChildWriterPolicyInput

const childWriterStarted = workspaceauthority.ChildWriterStarted

const childWriterJoined = workspaceauthority.ChildWriterJoined

const childWriterPolicy = workspaceauthority.ChildWriterPolicy

// Every invocation that can write the retained child repository gets an
// independent durable scope. A returned executor call alone is not evidence;
// only its actual process owner can acknowledge stop/join. Unknown adapters
// therefore fail closed rather than supplying an empty successful proof.
func invokeChildWriter[T any](ctx context.Context, required bool, jr journalAppender, env apiv1.InvocationEnvelope, call func(context.Context) (T, error)) (T, error) {
	if !required {
		return call(ctx)
	}
	var zero T
	if err := childWriterCustodyReady(jr); err != nil {
		return zero, err
	}
	scope, err := telemetry.NewRunID()
	if err != nil {
		return zero, err
	}
	event := journal.Event{Type: journal.EventRunnerAnnotation, Stage: env.TaskID, Attempt: int(env.Attempt), Runner: map[string]any{"kind": childWriterStarted, "writerScope": scope}}
	if err := jr.Append(event); err != nil {
		return zero, err
	}
	owned, proof := invoke.WithWorkspaceQuiescence(ctx)
	result, callErr := call(owned)
	if err := proof.Verify(); err != nil {
		return result, errors.Join(callErr, err)
	}
	event.Runner = map[string]any{"kind": childWriterJoined, "writerScope": scope}
	return result, errors.Join(callErr, jr.Append(event))
}

func invokeChildDeterministic(ctx context.Context, tf taskFrame, det invoke.Deterministic, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	ctx, err := childCredentialContext(ctx, tf.in)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return invokeChildWriter(ctx, childWorkspaceWriterRequired(tf.in, tf.t.EffectiveWorkspace()), tf.jr, env, func(owned context.Context) (apiv1.ResultEnvelope, error) {
		return det.Run(owned, env, *tf.t.Run)
	})
}

func invokeChildAgent(ctx context.Context, tf taskFrame, invocation *gooberInvocation, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	ctx, err := childCredentialContext(ctx, tf.in)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return invokeChildWriter(ctx, childWorkspaceWriterRequired(tf.in, tf.t.EffectiveWorkspace()), tf.jr, env, func(owned context.Context) (apiv1.ResultEnvelope, error) {
		return invocation.Invoke(owned, env)
	})
}

// VerifyChildWorkspaceQuiescence is required before adopting a crashed child
// repository or capturing its terminal result. No new attempt, stage.finished,
// or run.finished closes an earlier unacknowledged scope. A crashed writer
// requires independently verified owner cleanup before recovery can continue.
func VerifyChildWorkspaceQuiescence(reader *journal.Reader, id journal.RunIdentity, events []journal.Event) error {
	return verifyChildWorkspaceQuiescence(reader, id, events, nil)
}

func verifyChildWorkspaceQuiescence(reader *journal.Reader, id journal.RunIdentity, events []journal.Event, branch *int) error {
	pending, err := pendingChildWorkspaceWriters(reader, id, events)
	if err != nil {
		return err
	}
	for _, scope := range pending {
		if childWriterBlocksBranch(scope.Branch, branch) {
			return invoke.ErrWorkspaceNotQuiescent
		}
	}
	return nil
}

func pendingChildWorkspaceWriters(reader *journal.Reader, id journal.RunIdentity, events []journal.Event) (map[string]journal.Event, error) {
	return workspaceauthority.PendingChildWorkspaceWriters(reader, id, events)
}

func verifyChildWriterPolicy(reader *journal.Reader, id journal.RunIdentity) error {
	return workspaceauthority.VerifyChildWriterPolicy(reader, id)
}

func childWorkspaceWriterRequired(in StartInput, mode apiv1.WorkspaceMode) bool {
	return in.ChildWorkspace != nil && mode != apiv1.WorkspaceScratch
}

// A retry cannot start another writer while an older scope is unresolved.
// Reading the same durable evidence also makes operator reruns and restarts
// follow the identical rule, rather than clearing an in-memory failure flag.
func childWriterCustodyReady(jr journalAppender) error {
	owner, ok := jr.(interface{ Dir() string })
	if !ok {
		return invoke.ErrWorkspaceNotQuiescent
	}
	reader, err := journal.OpenReadOnly(owner.Dir())
	if err != nil {
		return err
	}
	id, err := reader.Identity()
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	recorder, ok := jr.(ArtifactRecorder)
	if !ok {
		return invoke.ErrWorkspaceNotQuiescent
	}
	_, branch, err := OwnedJournalScope(recorder)
	if err != nil {
		return err
	}
	return verifyChildWorkspaceQuiescence(reader, id, events, &branch)
}

func childWriterBlocksBranch(owner int, branch *int) bool {
	return branch == nil || *branch == 0 || owner == 0 || owner == *branch
}
