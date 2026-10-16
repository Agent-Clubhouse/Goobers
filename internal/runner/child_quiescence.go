package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

const childWriterPolicyInput = "child-writer-policy"
const childWriterStarted = "child.workspace.writer.started"
const childWriterJoined = "child.workspace.writer.joined"
const childWriterPolicy = `{"version":1,"scope":"managed-child-repository","writers":"stop-and-wait"}`

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
	if err := verifyChildWriterPolicy(reader, id); err != nil {
		return nil, err
	}
	pending := map[string]journal.Event{}
	seen := map[string]bool{}
	for _, event := range events {
		kind, _ := event.Runner["kind"].(string)
		if event.Type != journal.EventRunnerAnnotation || (kind != childWriterStarted && kind != childWriterJoined) {
			continue
		}
		scope, ok := event.Runner["writerScope"].(string)
		if !ok || len(scope) != 32 || !apiv1.ValidRunID(scope) || event.Stage == "" || event.Attempt < 1 {
			return nil, invoke.ErrWorkspaceNotQuiescent
		}
		if kind == childWriterStarted {
			if seen[scope] {
				return nil, invoke.ErrWorkspaceNotQuiescent
			}
			seen[scope], pending[scope] = true, event
			continue
		}
		started, ok := pending[scope]
		if !ok || started.Stage != event.Stage || started.Attempt != event.Attempt || started.Branch != event.Branch {
			return nil, invoke.ErrWorkspaceNotQuiescent
		}
		delete(pending, scope)
	}
	return pending, nil
}

func verifyChildWriterPolicy(reader *journal.Reader, id journal.RunIdentity) error {
	var ref *journal.InputRef
	for i := range id.Inputs {
		if id.Inputs[i].Name != childWriterPolicyInput {
			continue
		}
		if ref != nil {
			return invoke.ErrWorkspaceNotQuiescent
		}
		ref = &id.Inputs[i]
	}
	if id.Child == nil || ref == nil || ref.Integrity != apiv1.IntegrityTrusted {
		return invoke.ErrWorkspaceNotQuiescent
	}
	data, err := reader.ArtifactBytesBounded(ref.Ref, 256)
	if err != nil || !bytes.Equal(data, []byte(childWriterPolicy)) {
		return errors.Join(fmt.Errorf("runner: child workspace writer policy custody missing"), err)
	}
	return nil
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
