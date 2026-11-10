package workspaceauthority

import (
	"bytes"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// ChildWriterPolicyInput names the immutable writer policy journal input.
const ChildWriterPolicyInput = "child-writer-policy"

// ChildWriterStarted marks a durable workspace writer scope.
const ChildWriterStarted = "child.workspace.writer.started"

// ChildWriterJoined records acknowledgement that a writer has stopped.
const ChildWriterJoined = "child.workspace.writer.joined"

// ChildWriterPolicy pins the stop-and-wait requirement for managed child writers.
const ChildWriterPolicy = `{"version":1,"scope":"managed-child-repository","writers":"stop-and-wait"}`

// PendingChildWorkspaceWriters validates writer receipts and returns scopes without matching joins.
func PendingChildWorkspaceWriters(reader *journal.Reader, id journal.RunIdentity, events []journal.Event) (map[string]journal.Event, error) {
	if err := VerifyChildWriterPolicy(reader, id); err != nil {
		return nil, err
	}
	pending := map[string]journal.Event{}
	seen := map[string]bool{}
	for _, event := range events {
		kind, _ := event.Runner["kind"].(string)
		if event.Type != journal.EventRunnerAnnotation || (kind != ChildWriterStarted && kind != ChildWriterJoined) {
			continue
		}
		scope, ok := event.Runner["writerScope"].(string)
		if !ok || len(scope) != 32 || !apiv1.ValidRunID(scope) || event.Stage == "" || event.Attempt < 1 {
			return nil, invoke.ErrWorkspaceNotQuiescent
		}
		if kind == ChildWriterStarted {
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

// VerifyChildWriterPolicy checks the trusted, bounded policy bytes in the child journal.
func VerifyChildWriterPolicy(reader *journal.Reader, id journal.RunIdentity) error {
	var ref *journal.InputRef
	for i := range id.Inputs {
		if id.Inputs[i].Name != ChildWriterPolicyInput {
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
	if err != nil || !bytes.Equal(data, []byte(ChildWriterPolicy)) {
		return errors.Join(fmt.Errorf("runner: child workspace writer policy custody missing"), err)
	}
	return nil
}
