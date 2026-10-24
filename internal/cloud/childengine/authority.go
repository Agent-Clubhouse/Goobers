package childengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

// ParentAuthority prepares and resolves an engine parent stage's child-workflow
// authority from control-held state only (docs/design/engine-parent-child-authority.md
// §4 and §5). The parent identity comes from the start fence's run pins and
// this control plane's instance; the worker-written journal is read for the
// active attempt and must agree with the pins; the live engine execution must
// be running the exact pinned start.
type ParentAuthority struct {
	InstanceID string
	// Pins reads the start fence's pins for a run (acceptance.Store.RunPinsOf).
	Pins func(ctx context.Context, runID string) (acceptance.RunPins, error)
	// Journal opens a run's journal read-only from the control-side backend.
	Journal func(ctx context.Context, runID string) (journal.RunReader, error)
	// Execution describes the run's live engine execution
	// (startfence.DescribeStarted over the control Temporal client).
	Execution   func(ctx context.Context, runID string) (startfence.StartedExecution, error)
	Generations childworkflow.GenerationSource
	Applied     AppliedConfiguration
}

func (p ParentAuthority) validate() error {
	if p.InstanceID == "" || p.Pins == nil || p.Journal == nil || p.Execution == nil || p.Generations == nil || p.Applied == nil {
		return errors.New("childengine: parent authority requires an instance, run pins, a journal, the engine execution, a generation source and the applied configuration")
	}
	return nil
}

// PrepareStage is the GrantIssuer seam. Only the run and the committed origin
// are read from env; nothing else the caller asserts is used.
func (p ParentAuthority) PrepareStage(ctx context.Context, env apiv1.InvocationEnvelope) (childworkflow.Authority, error) {
	if env.ChildWorkflowOrigin == nil {
		return childworkflow.Authority{}, childworkflow.ErrAuthorityUnavailable
	}
	return p.PrepareOrigin(ctx, env.RunID, *env.ChildWorkflowOrigin)
}

// PrepareOrigin prepares the authority of the run's committed, active stage
// attempt named by origin. The returned Origin has no GrantID.
func (p ParentAuthority) PrepareOrigin(ctx context.Context, runID string, origin apiv1.ChildWorkflowOrigin) (childworkflow.Authority, error) {
	resolver, pinned, err := p.resolver(ctx, runID)
	if err != nil {
		return childworkflow.Authority{}, err
	}
	authority, err := resolver.PrepareOrigin(ctx, runID, origin)
	if err != nil {
		return childworkflow.Authority{}, err
	}
	authority.Execution = pinned.execution
	return authority, nil
}

// Resolve rechecks an authenticated origin against the same control-held
// state, as every child-workflow call must (§5).
func (p ParentAuthority) Resolve(ctx context.Context, origin childworkflow.Origin) (childworkflow.Authority, error) {
	resolver, pinned, err := p.resolver(ctx, origin.RunID)
	if err != nil {
		return childworkflow.Authority{}, err
	}
	authority, err := resolver.Resolve(ctx, origin)
	if err != nil {
		return childworkflow.Authority{}, err
	}
	authority.Execution = pinned.execution
	return authority, nil
}

// pinnedParent carries what one preparation derived from the pins, for the
// caller to attach after the journal resolver has run.
type pinnedParent struct {
	execution *childworkflow.ExecutionBinding
}

func (p ParentAuthority) resolver(ctx context.Context, runID string) (*childworkflow.JournalAuthorityResolver, *pinnedParent, error) {
	if err := p.validate(); err != nil {
		return nil, nil, err
	}
	if !apiv1.ValidRunID(runID) {
		return nil, nil, childworkflow.ErrAuthorityUnavailable
	}
	pins, err := p.Pins(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		// No start-fence row: not a top-level run this control plane started
		// (the I2 recursion guard), or started before the pins existed.
		return nil, nil, fmt.Errorf("%w: run %s has no start-fence pins", childworkflow.ErrAuthorityUnavailable, runID)
	}
	if err != nil {
		return nil, nil, err
	}
	if missing := missingPin(pins); missing != "" {
		return nil, nil, fmt.Errorf("%w: run %s has no pinned %s", childworkflow.ErrAuthorityUnavailable, runID, missing)
	}
	repo := pins.RepoRef
	parent := journal.RunIdentity{InstanceID: p.InstanceID, RunID: pins.RunID, Gaggle: pins.Gaggle, Workflow: pins.Workflow,
		WorkflowVersion: pins.WorkflowVersion, WorkflowDigest: pins.WorkflowDigest, GooberDigest: pins.GooberDigest,
		ConfigGeneration: pins.ConfigGeneration, Driver: journal.DriverEngine, WorkspaceRepository: &repo}
	pinned := &pinnedParent{}
	return &childworkflow.JournalAuthorityResolver{
		OpenJournal: p.Journal,
		LoadPinnedStage: func(ctx context.Context, journaled journal.RunIdentity, stage string) (childworkflow.PinnedStageAdmission, error) {
			if err := journalAgrees(journaled, parent); err != nil {
				return childworkflow.PinnedStageAdmission{}, err
			}
			if err := p.boundExecution(ctx, pins); err != nil {
				return childworkflow.PinnedStageAdmission{}, err
			}
			admission, err := p.pinnedStage(ctx, parent, pins, stage)
			if err != nil {
				return childworkflow.PinnedStageAdmission{}, err
			}
			pinned.execution = admission.execution
			return admission.PinnedStageAdmission, nil
		},
	}, pinned, nil
}

// missingPin names the first pin the bind needs that the start left empty.
// Pins are evidence, never a start gate: the bind refuses its own operation.
func missingPin(pins acceptance.RunPins) string {
	switch {
	case pins.Gaggle == "":
		return "gaggle"
	case pins.Workflow == "":
		return "workflow"
	case pins.ConfigGeneration == "":
		return "config generation"
	case pins.WorkflowDigest == "":
		return "workflow digest"
	case pins.GooberDigest == "":
		return "goober digest"
	case pins.RepoRef.Provider == "" || pins.RepoRef.Owner == "" || pins.RepoRef.Name == "":
		return "repository"
	case pins.InputDigest == "":
		return "input digest"
	}
	return ""
}

// journalAgrees cross-checks the worker-written journal identity against the
// control-built parent. The journal is never an input to authority; any
// disagreement refuses.
func journalAgrees(journaled, parent journal.RunIdentity) error {
	agrees := journaled.InstanceID == parent.InstanceID && journaled.RunID == parent.RunID && journaled.Gaggle == parent.Gaggle &&
		journaled.Workflow == parent.Workflow && journaled.WorkflowVersion == parent.WorkflowVersion &&
		journaled.WorkflowDigest == parent.WorkflowDigest && journaled.GooberDigest == parent.GooberDigest &&
		journaled.ConfigGeneration == parent.ConfigGeneration && journaled.Driver == journal.DriverEngine && journaled.Child == nil &&
		(journaled.WorkspaceRepository == nil || reflect.DeepEqual(*journaled.WorkspaceRepository, *parent.WorkspaceRepository))
	if !agrees {
		return fmt.Errorf("%w: journal identity of run %s disagrees with its start-fence pins", childworkflow.ErrAuthorityUnavailable, parent.RunID)
	}
	return nil
}

// boundExecution requires the run's live engine execution to be running the
// exact start the pins record: the start-owner memo control's own start fence
// wrote carries the same input digest. A worker that stops writing, or
// journals past its parent's end, cannot keep authority alive.
func (p ParentAuthority) boundExecution(ctx context.Context, pins acceptance.RunPins) error {
	started, err := p.Execution(ctx, pins.RunID)
	var notFound *serviceerror.NotFound
	switch {
	case errors.As(err, &notFound):
		return fmt.Errorf("%w: run %s has no engine execution", childworkflow.ErrAuthorityUnavailable, pins.RunID)
	case err != nil:
		return fmt.Errorf("%w: describe run %s: %w", engine.ErrChildCustodyUnavailable, pins.RunID, err)
	case !started.Engine || started.Status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
		return fmt.Errorf("%w: run %s engine execution is not running", childworkflow.ErrAuthorityUnavailable, pins.RunID)
	case started.Owner == nil || started.Owner.InputDigest != pins.InputDigest || started.Owner.AcceptanceID != pins.AcceptanceID:
		// The digest covers the whole started input, the instance included. A
		// start-owner memo older than the digest cannot match: such a parent's
		// grants refuse, by design.
		return fmt.Errorf("%w: run %s engine execution is not the pinned start", childworkflow.ErrAuthorityUnavailable, pins.RunID)
	}
	return nil
}

type pinnedStageAdmission struct {
	childworkflow.PinnedStageAdmission
	execution *childworkflow.ExecutionBinding
}

// pinnedStage recompiles the parent stage from the pinned generation, narrowed
// by the applied configuration, and checks the execution binding it derives
// against the pins.
func (p ParentAuthority) pinnedStage(ctx context.Context, parent journal.RunIdentity, pins acceptance.RunPins, stage string) (pinnedStageAdmission, error) {
	current, ok := p.Applied.Applied()
	if !ok || current.Config == nil || current.Set == nil {
		return pinnedStageAdmission{}, fmt.Errorf("%w: no configuration generation is applied", engine.ErrChildCustodyUnavailable)
	}
	authority, err := childworkflow.PinnedAuthority(ctx, p.Generations, current.Config, current.Set, parent, stage, childworkflow.OfflineGooberAdmission)
	if err != nil {
		return pinnedStageAdmission{}, err
	}
	// The start resolver pins the gaggle's project from the same generation.
	// Requiring that keeps a future start path that pins another repository
	// failing closed instead of redirecting children.
	if !reflect.DeepEqual(pins.RepoRef, authority.Admission.Gaggle.Spec.Project) {
		return pinnedStageAdmission{}, fmt.Errorf("%w: run %s pinned repository is not its generation's gaggle project", childworkflow.ErrAuthorityUnavailable, pins.RunID)
	}
	execution := authority.Execution
	if execution == nil || !reflect.DeepEqual(execution.RepoRef, pins.RepoRef) {
		return pinnedStageAdmission{}, fmt.Errorf("%w: run %s execution binding is not its pinned repository", childworkflow.ErrAuthorityUnavailable, pins.RunID)
	}
	if execution.BranchNamespace != providers.NormalizeBranchNamespace(pins.BranchNamespace) {
		return pinnedStageAdmission{}, fmt.Errorf("%w: run %s pinned branch namespace differs from its generation's", childworkflow.ErrAuthorityUnavailable, pins.RunID)
	}
	policy, err := childworkflow.CurrentCustodyPolicy(current.Config, current.Set,
		childworkflow.ParentSelection{Gaggle: parent.Gaggle, Workflow: parent.Workflow, Stage: stage}, childworkflow.OfflineGooberAdmission)
	if err != nil {
		return pinnedStageAdmission{}, err
	}
	// The parent's own execution ceiling, as the local runtime derives it:
	// child delegation limits do not define it.
	capabilities := childworkflow.PermissionIntersection(authority.Admission.ParentTask.Capabilities, policy.GrantedCapabilities)
	if policy.ParentTask.Goober != authority.Admission.ParentTask.Goober {
		capabilities = nil
	}
	return pinnedStageAdmission{
		PinnedStageAdmission: childworkflow.PinnedStageAdmission{ParentExecutionCapabilities: capabilities, Admission: authority.Admission,
			ConfigGeneration: authority.ConfigGeneration, WorkflowDigest: authority.ParentWorkflowDigest, GooberDigest: authority.ParentGooberDigest},
		execution: execution,
	}, nil
}

// AuthorityQueue is the child custody an Authorities binds grants in.
// *acceptance.Store implements it.
type AuthorityQueue interface {
	childworkflow.ChildQueue
	RunPinsOf(ctx context.Context, runID string) (acceptance.RunPins, error)
}

// Authorities binds and releases engine parents' child-workflow grants under
// the controller lease: the control half of the engine worker's acquire and
// release at stage dispatch (§2 steps 2 to 6).
type Authorities struct {
	Queue   AuthorityQueue
	Parents ParentAuthority
	// Grants mints child-workflow grants in the credential-grant key domain.
	Grants *podauth.SignedKey
	Now    func() time.Time
}

var _ engine.ChildWorkflowAuthority = Authorities{}

// AcquireChildAuthority binds (or, for the same live attempt, re-delivers) the
// grant of the run's active stage attempt the request's origin names.
func (a Authorities) AcquireChildAuthority(ctx context.Context, request engine.ChildAuthorityRequest) (engine.ChildAuthorityGrant, error) {
	if a.Queue == nil || a.Grants == nil {
		return engine.ChildAuthorityGrant{}, errors.New("childengine: authorities require child custody and a grant key")
	}
	if err := request.Validate(); err != nil {
		return engine.ChildAuthorityGrant{}, fmt.Errorf("%w: %w", engine.ErrChildCustodyRefused, err)
	}
	runID, origin := request.RunID, request.Origin
	issuer := childworkflow.GrantIssuer{Queue: a.Queue, Grants: a.Grants, Authority: a.Parents, Endpoint: request.Endpoint,
		// Control never journals or logs the bearer: it is returned to the
		// worker, which registers it with the stage's scrubber.
		Secrets: discardSecrets{}, Now: a.Now}
	access, _, err := issuer.AcquireOrRecover(ctx, apiv1.InvocationEnvelope{RunID: runID, ChildWorkflowOrigin: &origin})
	if err != nil {
		return engine.ChildAuthorityGrant{}, classify(err)
	}
	// The grant outlives this request: release is the worker's own call, not
	// the issuer's in-process close.
	grant, err := a.Grants.VerifyChildWorkflowGrant(access.BearerToken)
	if err != nil {
		return engine.ChildAuthorityGrant{}, err
	}
	return engine.ChildAuthorityGrant{Endpoint: access.Endpoint, BearerToken: access.BearerToken, GrantID: grant.ID, ExpiresAt: grant.ExpiresAt}, nil
}

// ReleaseChildAuthority revokes exactly the named grant of the run's stage
// occurrence. A grant already replaced by a newer attempt, or never bound, is
// released; an earlier epoch's custody that adoption has not reached is
// retried, never reported as released.
func (a Authorities) ReleaseChildAuthority(ctx context.Context, release engine.ChildAuthorityRelease) error {
	if a.Queue == nil {
		return errors.New("childengine: authorities require child custody")
	}
	if err := release.Validate(); err != nil {
		return fmt.Errorf("%w: %w", engine.ErrChildCustodyRefused, err)
	}
	runID, origin, grantID := release.RunID, release.Origin, release.GrantID
	pins, err := a.Queue.RunPinsOf(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return classify(err)
	}
	bound, err := a.Queue.ChildAuthority(ctx, triggerqueue.ChildParent{Gaggle: pins.Gaggle, ParentRunID: runID}, origin.StageOccurrence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return classify(err)
	}
	if bound.GrantID != grantID || bound.AttemptID != origin.AttemptID || bound.Revoked {
		return nil
	}
	err = a.Queue.RevokeChildAuthority(ctx, bound)
	if errors.Is(err, triggerqueue.ErrChildAuthorityChanged) {
		return nil
	}
	return classify(err)
}

type discardSecrets struct{}

func (discardSecrets) RegisterUntil([]byte, time.Time) {}
