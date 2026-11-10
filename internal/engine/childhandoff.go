package engine

import (
	"context"
	"encoding/json"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/journal"
)

// ChildHandoffObservation names only authority lent to the worker. Finish
// atomically revokes it with the final custody check, closing acceptance
// versus invocation exit.
// Neither child identity nor source is accepted from the stage.
type ChildHandoffObservation struct {
	ChildAuthorityRelease
	Finish bool `json:"finish,omitempty"`
	// Stopped is worker-observed proof, never stage output. Control seals the
	// accepted handoff atomically with revocation only when this is true.
	Stopped bool `json:"stopped,omitempty"`
}

// ChildHandoffObserver reads accepted custody over the worker-only plane.
// A nil request means this occurrence has no outstanding child.
type ChildHandoffObserver interface {
	ObserveChildHandoff(context.Context, ChildHandoffObservation) (*journal.ChildHandoffRequest, error)
}

// ChildHandoffDigest is the stable identity of a host-authored handoff.
func ChildHandoffDigest(request journal.ChildHandoffRequest) string {
	request.RequestID = ""
	data, _ := json.Marshal(request)
	return journal.Digest(data)
}

// Recorded dispatch payloads without this marker retain their absent pin.
func childHandoffDispatchEnabled(ctx workflow.Context, in RunInput, env apiv1.InvocationEnvelope) bool {
	return env.ChildWorkflowOrigin != nil && childProtocolEnabled(ctx) && in.ChildWorkflowsEnabled && workflow.GetVersion(ctx, "engine-child-handoff-dispatch-v1", workflow.DefaultVersion, 1) != workflow.DefaultVersion
}
