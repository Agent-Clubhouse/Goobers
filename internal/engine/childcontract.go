package engine

// Generated-child wire contracts (HA-P4-08). The protocol that uses them is
// reachable for parents with a pinned enabled posture and their admitted
// children.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
	wf "github.com/goobers/goobers/internal/workflow"
)

const (
	childCleanupTimeout = 5 * time.Minute
	// ChildActivityStartToClose bounds one child custody activity attempt. A
	// custody client's per-call credential must outlive it.
	ChildActivityStartToClose = time.Minute
	// childCustodyRetryCeiling caps backoff so retries keep probing through a
	// control-plane failover (lease TTL plus self-fence plus readiness) inside
	// childCleanupTimeout. Unclassified failures still stop after
	// childUnclassifiedAttempts; refusals never retry.
	childCustodyRetryCeiling = 10 * time.Second
	// maxChildBindingBytes bounds the receipt carried in every child command.
	maxChildBindingBytes = 16 << 10
	// maxChildManifestBytes admits the largest retained proposal plus its pins.
	maxChildManifestBytes = triggerqueue.MaxChildProposalBytes + 1<<20
	// maxChildResolvedInputBytes keeps the recorded Load result well below the
	// Temporal payload limit after encoding overhead. Proposal bytes never enter
	// history; only the normalized execution input does.
	maxChildResolvedInputBytes = 512 << 10
	// frozenChildManifestVersion is the only manifest encoding this worker reads.
	frozenChildManifestVersion = 1
)

type childProtocolContextKey struct{}

func childProtocolEnabled(ctx workflow.Context) bool {
	return ctx.Value(childProtocolContextKey{}) == true
}

// childWorkflowsSwitchChange gates the production child-workflow switch
// (#8085 d). The marker is recorded only for a run that opted in and whose
// definition declares task.childWorkflows, so every other history is
// byte-identical, and a history recorded before the switch replays refused.
const childWorkflowsSwitchChange = "engine-child-workflows-switch-v1"

// childWorkflowsEnabled lets a run that pinned the instance's opt-in use the
// generated-child protocol.
func childWorkflowsEnabled(ctx workflow.Context, in RunInput) bool {
	return in.ChildWorkflowsEnabled && workflow.GetVersion(ctx, childWorkflowsSwitchChange, workflow.DefaultVersion, 1) != workflow.DefaultVersion
}

// ChildRunBinding is an immutable admission receipt, never agent-provided
// authority. The service fences every operation by acceptance and parent epoch.
// ManifestDigest addresses the entire frozen manifest, not a mutable catalog name.
type ChildRunBinding struct {
	Request             journal.ChildHandoffRequest `json:"request"`
	Child               triggerqueue.ChildRecord    `json:"child"`
	ManifestDigest      string                      `json:"manifestDigest"`
	ParentWorkflowID    string                      `json:"parentWorkflowId"`
	ParentWorkflow      string                      `json:"parentWorkflow"`
	ParentTemporalRunID string                      `json:"parentTemporalRunId"`
	Deadline            time.Time                   `json:"deadline"`
}

// ChildAdmissionInput is built from the committed origin and owning execution.
// Request is a dispatch adapter's verified handoff; the service rechecks authority.
type ChildAdmissionInput struct {
	// ChildWorkflowsEnabled is copied from the parent's immutable RunInput,
	// never from current configuration. Admission freezes it into the child's
	// digest-addressed manifest before binding the start.
	ChildWorkflowsEnabled bool                        `json:"childWorkflowsEnabled,omitempty"`
	Request               journal.ChildHandoffRequest `json:"request"`
	ParentWorkflowID      string                      `json:"parentWorkflowId"`
	ParentWorkflow        string                      `json:"parentWorkflow"`
	ParentTemporalRunID   string                      `json:"parentTemporalRunId"`
	Stage                 string                      `json:"stage"`
	// OriginSeq is the committed journal sequence of the bound stage start.
	OriginSeq uint64 `json:"originSeq"`
}

// FrozenChildManifest is stored once by admission. Source is the exact accepted
// proposal; SourceDigest, CanonicalDigest and Input.WorkflowDigest are distinct
// identities. Repository/configuration, grants, kit and placement pins travel
// inside Input. No workspace bytes, credentials or signed URLs do.
type FrozenChildManifest struct {
	Version         int      `json:"version"`
	Source          []byte   `json:"source"`
	SourceDigest    string   `json:"sourceDigest"`
	CanonicalDigest string   `json:"canonicalDigest"`
	Input           RunInput `json:"input"`
}

// ChildAdmissions is the activity-side admission boundary. Implementations
// MUST use durable, tenant-scoped compare-and-swap receipts; process-local
// locks are insufficient. Every method is idempotent after lost acknowledgement.
//
// Admit verifies the active origin, policy/preview, exact source bytes and
// pinned placements, and atomically retains admission plus a unique start
// intent owned by the Temporal parent. Load reads only the digest-addressed
// manifest. Cancel durably fences later starts and retains custody; it never
// restores capacity or publishes a usable snapshot on uncertain termination.
type ChildAdmissions interface {
	Admit(context.Context, ChildAdmissionInput) (ChildRunBinding, error)
	Load(context.Context, ChildRunBinding) ([]byte, error)
	Cancel(context.Context, ChildRunBinding) error
}

func (b ChildRunBinding) validate() error {
	r, c := b.Request, b.Child
	encoded, err := json.Marshal(b)
	if err != nil || len(encoded) > maxChildBindingBytes || r.Validate(&r.Origin) != nil || r.Action != "wait" ||
		c.Identity.Gaggle != r.Gaggle || c.Identity.ParentRunID != r.ParentRunID || c.Identity.StageOccurrence != r.Origin.StageOccurrence || c.Identity.InvocationKey != r.InvocationKey ||
		c.AcceptanceID != r.AcceptanceID || c.RunID != r.ChildRunID || c.ProposalDigest != r.SourceDigest || c.AcceptedAt.IsZero() ||
		!blobstore.ValidDigest(b.ManifestDigest) || b.ParentWorkflow == "" || len(b.ParentWorkflow) > 256 || b.ParentWorkflowID == "" || b.ParentTemporalRunID == "" ||
		!b.Deadline.After(c.AcceptedAt) || b.Deadline.Sub(c.AcceptedAt) > 24*time.Hour {
		return errors.New("engine: invalid frozen child admission")
	}
	return nil
}

// sameParent reports whether a receipt was issued for exactly this intent.
func (b ChildRunBinding) sameParent(in ChildAdmissionInput) bool {
	return b.Request == in.Request && b.ParentWorkflow == in.ParentWorkflow && b.ParentWorkflowID == in.ParentWorkflowID && b.ParentTemporalRunID == in.ParentTemporalRunID
}

// restartedParent reports a receipt for this exact request that a different
// execution of the same parent workflow already owns, as after a reset.
func (b ChildRunBinding) restartedParent(in ChildAdmissionInput) bool {
	return b.Request == in.Request && b.ParentWorkflow == in.ParentWorkflow && b.ParentWorkflowID == in.ParentWorkflowID && b.ParentTemporalRunID != in.ParentTemporalRunID
}

// decodeFrozenChildManifest verifies exact manifest and proposal bytes, then
// the serial phase-1 subset. The result is the only part recorded in history.
func decodeFrozenChildManifest(b ChildRunBinding, data []byte) (RunInput, error) {
	if len(data) == 0 || len(data) > maxChildManifestBytes || journal.Digest(data) != b.ManifestDigest {
		return RunInput{}, errors.New("engine: frozen child manifest digest mismatch")
	}
	var manifest FrozenChildManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return RunInput{}, errors.New("engine: undecodable frozen child manifest")
	}
	if manifest.Version != frozenChildManifestVersion || len(manifest.Source) == 0 || len(manifest.Source) > triggerqueue.MaxChildProposalBytes ||
		journal.Digest(manifest.Source) != manifest.SourceDigest || manifest.SourceDigest != b.Request.SourceDigest || !blobstore.ValidDigest(manifest.CanonicalDigest) {
		return RunInput{}, errors.New("engine: frozen child source differs from its accepted digest")
	}
	child := manifest.Input
	if child.RunID != b.Child.RunID || child.Gaggle != b.Child.Identity.Gaggle || child.HITL != nil || !child.LiveJournal ||
		child.PreviewFeaturesEnabled == nil || !*child.PreviewFeaturesEnabled || !blobstore.ValidDigest(child.GooberDigest) || !blobstore.ValidDigest(child.ConfigGeneration) {
		return RunInput{}, errors.New("engine: invalid frozen child pins")
	}
	if err := refuseGeneratedChildShape(child); err != nil {
		return RunInput{}, err
	}
	digest, err := wf.ComputeDigest(child.definition())
	if err != nil || digest != child.WorkflowDigest {
		return RunInput{}, errors.New("engine: frozen workflow digest mismatch")
	}
	if encoded, err := json.Marshal(child); err != nil || len(encoded) > maxChildResolvedInputBytes {
		return RunInput{}, errors.New("engine: frozen child input exceeds the history bound")
	}
	return child, nil
}

// refuseGeneratedChildShape keeps phase 1 to serial task/gate graphs with
// engine gates: no recursion, fan-out or human gate. Static parallels are
// engine-supported for roots, so fan-out is refused here explicitly.
func refuseGeneratedChildShape(child RunInput) error {
	if len(child.Spec.Parallels) != 0 || len(child.Spec.Maps) != 0 {
		return errors.New("engine: generated children are serial task/gate graphs")
	}
	if err := wf.RefuseChildWorkflowExecution(child.Spec); err != nil {
		return err
	}
	if err := wf.RefuseUnsupportedFanOut(child.Spec); err != nil {
		return err
	}
	for _, g := range child.Spec.Gates {
		if err := refuseHumanGate(g); err != nil {
			return err
		}
	}
	return nil
}

// AdmitEngineChild verifies and records one idempotent child start intent.
func (a *Activities) AdmitEngineChild(ctx context.Context, in ChildAdmissionInput) (_ ChildRunBinding, err error) {
	defer func() { err = classifyChildCustody(ctx, err) }()
	if a.EngineChildren == nil {
		return ChildRunBinding{}, ErrNotConfigured
	}
	out, err := a.EngineChildren.Admit(ctx, in)
	if err != nil {
		return ChildRunBinding{}, err
	}
	if err = out.validate(); err != nil {
		return ChildRunBinding{}, temporal.NewNonRetryableApplicationError(err.Error(), "child_admission_invalid", nil)
	}
	if out.restartedParent(in) {
		return ChildRunBinding{}, temporal.NewNonRetryableApplicationError(errSealedChildRestart.Error(), "child_restart_unsupported", nil)
	}
	if !out.sameParent(in) {
		return ChildRunBinding{}, temporal.NewNonRetryableApplicationError("engine: admission changed parent binding", "child_admission_invalid", nil)
	}
	return out, nil
}

// LoadEngineChild checks the exact manifest bytes and generated graph pins.
func (a *Activities) LoadEngineChild(ctx context.Context, in ChildRunBinding) (_ RunInput, err error) {
	defer func() { err = classifyChildCustody(ctx, err) }()
	if a.EngineChildren == nil {
		return RunInput{}, ErrNotConfigured
	}
	if err := in.validate(); err != nil {
		return RunInput{}, err
	}
	data, err := a.EngineChildren.Load(ctx, in)
	if err != nil {
		return RunInput{}, err
	}
	child, err := decodeFrozenChildManifest(in, data)
	if err != nil {
		// Corrupt or missing accepted content is a custody error, never a
		// reason to regenerate it, so retrying the same bytes cannot help.
		return RunInput{}, temporal.NewNonRetryableApplicationError(err.Error(), "child_manifest_invalid", nil)
	}
	return child, nil
}

// CancelEngineChild fences the family and retains custody without resuming it.
func (a *Activities) CancelEngineChild(ctx context.Context, in ChildRunBinding) (err error) {
	defer func() { err = classifyChildCustody(ctx, err) }()
	if a.EngineChildren == nil {
		return ErrNotConfigured
	}
	if err := in.validate(); err != nil {
		return err
	}
	return a.EngineChildren.Cancel(ctx, in)
}

func childActivityContext(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: ChildActivityStartToClose, ScheduleToCloseTimeout: childCleanupTimeout, WaitForCancellation: true,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: childCustodyRetryCeiling}})
}
