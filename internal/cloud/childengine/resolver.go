package childengine

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/childworkflow"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
)

// AppliedConfiguration reports the control plane's running generation.
// *cloudconfig.Configuration implements it.
type AppliedConfiguration interface {
	Applied() (cloudconfig.Candidate, bool)
}

// PinnedResolver resolves an accepted engine child's freeze context from the
// retained generation its parent was accepted under (envelope.ConfigGeneration),
// narrowed by the applied configuration. The parent identity is rebuilt from
// the envelope's own pins and bound to this control plane's instance, so
// Freeze's execution-binding check refuses a child accepted for any other
// instance.
func PinnedResolver(instanceID string, source childworkflow.GenerationSource, applied AppliedConfiguration) (childworkflow.EngineFreezeResolver, error) {
	if instanceID == "" || source == nil || applied == nil {
		return nil, errors.New("childengine: resolver requires an instance, a generation source and the applied configuration")
	}
	return func(ctx context.Context, e childworkflow.ChildStartEnvelope) (childworkflow.EngineFreezeContext, error) {
		current, ok := applied.Applied()
		if !ok || current.Config == nil || current.Set == nil {
			return childworkflow.EngineFreezeContext{}, fmt.Errorf("%w: no configuration generation is applied", engine.ErrChildCustodyUnavailable)
		}
		parent := journal.RunIdentity{InstanceID: instanceID, RunID: e.ParentRunID, Gaggle: e.Gaggle, Workflow: e.ParentWorkflow,
			WorkflowDigest: e.ParentWorkflowDigest, GooberDigest: e.ParentGooberDigest, ConfigGeneration: e.ConfigGeneration, Driver: journal.DriverEngine}
		authority, err := childworkflow.PinnedAuthority(ctx, source, current.Config, current.Set, parent, e.ParentStage, childworkflow.OfflineGooberAdmission)
		if err != nil {
			return childworkflow.EngineFreezeContext{}, err
		}
		return childworkflow.FreezeContextFor(authority)
	}, nil
}

// EngineFreezer adapts the childworkflow engine freezer to the Service's
// Freezer seam.
func EngineFreezer(resolve childworkflow.EngineFreezeResolver) Freezer {
	freezer := childworkflow.EngineFreezer{Resolve: resolve}
	return func(ctx context.Context, a Accepted) (engine.FrozenChildManifest, error) {
		return freezer.Freeze(ctx, a.Request, a.Child, a.Payload, a.Source)
	}
}
