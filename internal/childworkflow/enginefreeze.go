package childworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ErrFrozenChildDrift refuses an accepted child whose pinned inputs no longer
// reproduce the identities recorded at acceptance. Changed policy may block a
// child; it never rewrites what was accepted.
var ErrFrozenChildDrift = errors.New("childworkflow: accepted child no longer reproduces its admitted identities")

// EngineFreezeContext is what the trusted control plane resolves for an
// accepted envelope from its pinned configuration generation, never from the
// live catalog: the admission context the proposal was validated against and
// the parent's execution bindings the child inherits.
type EngineFreezeContext struct {
	Admission       AdmissionContext
	InstanceID      string
	RepoRef         apiv1.RepoRef
	BranchNamespace string
}

// EngineFreezeResolver loads the pinned context for one accepted envelope.
type EngineFreezeResolver func(context.Context, ChildStartEnvelope) (EngineFreezeContext, error)

// EngineFreezer compiles accepted child custody into the engine's frozen
// manifest. Freeze is a pure function of its arguments and the resolver's
// pinned context, so retries and lease successors produce identical bytes.
type EngineFreezer struct {
	Resolve EngineFreezeResolver
}

// Freeze recompiles the exact accepted source under the pinned admission
// context, verifies every identity recorded in the start envelope, and
// returns the manifest the engine loads by digest.
func (f EngineFreezer) Freeze(ctx context.Context, request journal.ChildHandoffRequest, child triggerqueue.ChildRecord, payload, source []byte) (engine.FrozenChildManifest, error) {
	if f.Resolve == nil {
		return engine.FrozenChildManifest{}, errors.New("childworkflow: engine freezer requires a pinned context resolver")
	}
	e, err := DecodeStartEnvelope(payload)
	if err != nil {
		return engine.FrozenChildManifest{}, err
	}
	if e.Backend != BackendEngine || e.Identity() != child.Identity || e.InvocationKey != request.InvocationKey || e.StageOccurrence != request.Origin.StageOccurrence ||
		e.SourceDigest != child.ProposalDigest || e.SourceDigest != request.SourceDigest || e.SourceDigest != digest(source) {
		return engine.FrozenChildManifest{}, fmt.Errorf("%w: envelope, lineage and source differ", ErrFrozenChildDrift)
	}
	pinned, err := f.Resolve(ctx, e)
	if err != nil {
		return engine.FrozenChildManifest{}, err
	}
	admission := pinned.Admission
	if admission.Backend != BackendEngine || admission.Gaggle.Name != e.Gaggle || admission.ParentTask.Name != e.ParentStage ||
		admission.ConfigDigest != e.ConfigDigest || AuthorityPolicyDigest(admission) != e.PolicyDigest {
		return engine.FrozenChildManifest{}, fmt.Errorf("%w: pinned admission context", ErrFrozenChildDrift)
	}
	validator, err := NewValidator(admission)
	if err != nil {
		return engine.FrozenChildManifest{}, err
	}
	proposal, err := validator.Validate(source)
	if err != nil {
		return engine.FrozenChildManifest{}, err
	}
	placements, err := json.Marshal(proposal.Placements)
	if err != nil {
		return engine.FrozenChildManifest{}, err
	}
	def := proposal.Machine.Def
	if proposal.CanonicalDigest != e.CanonicalDigest || proposal.Machine.Digest() != e.WorkflowDigest || def.Name != e.Workflow || digest(placements) != e.PlacementsDigest {
		return engine.FrozenChildManifest{}, fmt.Errorf("%w: compiled proposal", ErrFrozenChildDrift)
	}
	preview := true
	input := engine.RunInput{
		InstanceID: pinned.InstanceID, RunID: child.RunID, Gaggle: e.Gaggle,
		WorkflowName: def.Name, Version: def.Version, DSLVersion: def.DSLVersion, WorkflowDigest: proposal.Machine.Digest(), Spec: def.Spec,
		// task.childWorkflows is itself preview-gated: a parent that may
		// request children already carries the acknowledgement its child needs.
		PreviewFeaturesEnabled: &preview, LiveJournal: true,
		RepoRef: pinned.RepoRef, BranchNamespace: pinned.BranchNamespace, TriggerKind: string(journal.TriggerManual),
		GooberDigest: e.ParentGooberDigest, ConfigGeneration: e.ConfigGeneration, Placements: proposal.Placements,
	}
	return engine.FrozenChildManifest{Version: 1, Source: append([]byte(nil), source...), SourceDigest: e.SourceDigest, CanonicalDigest: e.CanonicalDigest, Input: input}, nil
}
