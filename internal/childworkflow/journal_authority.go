package childworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// PinnedStageAdmission comes from the exact archived config generation in the
// run identity, intersected with current permissions. No current-catalog
// fallback is permitted. The loader must independently verify all three pins.
// ParentTask is the compiled task from that generation, without policy edits.
type PinnedStageAdmission struct {
	Admission        AdmissionContext
	ConfigGeneration string
	WorkflowDigest   string
	GooberDigest     string
}

// JournalAuthorityResolver checks live tool authority against committed stage
// starts. It does not authenticate bearer tokens or bind grants: the issuer and
// submission store own those fences. Accepted custody has a separate lifecycle
// and must not use Resolve to resurrect an old stage's tool authority.
type JournalAuthorityResolver struct {
	// OpenJournal must use a trusted run root and journal.OpenReadOnly.
	OpenJournal func(context.Context, string) (*journal.Reader, error)
	// LoadPinnedStage must fail if the archive or current permission decision
	// is unavailable. It must never substitute a currently named workflow.
	LoadPinnedStage func(context.Context, journal.RunIdentity, string) (PinnedStageAdmission, error)
}

// PrepareStage verifies the trusted runner envelope before a grant is minted.
// The returned Origin intentionally has no GrantID; it cannot yet authorize a
// submission. Unknown or historical provenance fails closed.
func (r *JournalAuthorityResolver) PrepareStage(ctx context.Context, envelope apiv1.InvocationEnvelope) (Authority, error) {
	if envelope.ChildWorkflowOrigin == nil {
		return Authority{}, ErrAuthorityUnavailable
	}
	authority, id, event, err := r.prepare(ctx, envelope.RunID, *envelope.ChildWorkflowOrigin)
	if err != nil {
		return Authority{}, err
	}
	if !matchesEnvelope(envelope, id, event, authority.Admission) {
		return Authority{}, ErrAuthorityUnavailable
	}
	return authority, nil
}

// Resolve rechecks journal ownership and current effective permissions for an
// authenticated origin. Signature and queue registration remain separate checks.
func (r *JournalAuthorityResolver) Resolve(ctx context.Context, origin Origin) (Authority, error) {
	if !origin.valid() {
		return Authority{}, ErrAuthorityUnavailable
	}
	authority, _, _, err := r.prepare(ctx, origin.RunID, apiv1.ChildWorkflowOrigin{
		StageOccurrence: origin.StageOccurrence, AttemptID: origin.AttemptID,
	})
	if err != nil {
		return Authority{}, err
	}
	authority.Origin.GrantID = origin.GrantID
	if authority.Origin != origin {
		return Authority{}, ErrAuthorityUnavailable
	}
	return authority, nil
}

func (r *JournalAuthorityResolver) prepare(ctx context.Context, runID string, origin apiv1.ChildWorkflowOrigin) (Authority, journal.RunIdentity, journal.Event, error) {
	if r == nil || r.OpenJournal == nil || r.LoadPinnedStage == nil || !apiv1.ValidRunID(runID) {
		return Authority{}, journal.RunIdentity{}, journal.Event{}, ErrAuthorityUnavailable
	}
	rd, err := r.OpenJournal(ctx, runID)
	if err != nil || rd == nil {
		return Authority{}, journal.RunIdentity{}, journal.Event{}, errors.Join(ErrAuthorityUnavailable, err)
	}
	id, event, err := activeJournalStage(ctx, rd, runID, origin)
	if err != nil {
		return Authority{}, id, event, err
	}
	pinned, err := r.LoadPinnedStage(ctx, id, event.Stage)
	if err != nil {
		return Authority{}, id, event, errors.Join(ErrAuthorityUnavailable, err)
	}
	admission, err := checkedPinnedStage(rd, id, event, pinned)
	if err != nil {
		return Authority{}, id, event, errors.Join(ErrAuthorityUnavailable, err)
	}
	// Loading an archive can race completion or replacement. Re-read durable
	// state before returning; acceptance additionally uses the queue's CAS fence.
	currentID, currentEvent, err := activeJournalStage(ctx, rd, runID, origin)
	if err != nil || !sameJSON(id, currentID) || !sameJSON(event, currentEvent) {
		return Authority{}, id, event, errors.Join(ErrAuthorityChanged, err)
	}
	authority := Authority{
		Origin: Origin{Gaggle: id.Gaggle, RunID: id.RunID, StageOccurrence: origin.StageOccurrence,
			AttemptID: origin.AttemptID, ConfigDigest: admission.ConfigDigest, PolicyDigest: AuthorityPolicyDigest(admission)},
		Actor:     InvocationActor(id.RunID, origin.StageOccurrence),
		Admission: admission, ConfigGeneration: id.ConfigGeneration,
		ParentWorkflow: id.Workflow, ParentWorkflowDigest: id.WorkflowDigest, ParentGooberDigest: id.GooberDigest,
	}
	return authority, id, event, nil
}

// InvocationActor names the stable logical principal for a verified parent
// occurrence. Grant renewal and attempt replacement do not change this actor.
// Calling it does not authenticate either input or authorize an operation.
func InvocationActor(runID, occurrence string) string {
	return "child-workflow:" + runID + ":" + occurrence
}

func checkedPinnedStage(rd *journal.Reader, id journal.RunIdentity, event journal.Event, pinned PinnedStageAdmission) (AdmissionContext, error) {
	if pinned.ConfigGeneration != id.ConfigGeneration || pinned.WorkflowDigest != id.WorkflowDigest || pinned.GooberDigest != id.GooberDigest {
		return AdmissionContext{}, ErrAuthorityUnavailable
	}
	machine, err := runner.PinnedWorkflowMachine(rd, id)
	if err != nil {
		return AdmissionContext{}, err
	}
	task, found := machine.Task(event.Stage)
	if !found || machine.Def.DSLVersion != "3.1" || machine.Def.Spec.Gaggle != id.Gaggle || !sameJSON(task, pinned.Admission.ParentTask) {
		return AdmissionContext{}, ErrAuthorityUnavailable
	}
	if task.Type != apiv1.TaskAgentic || task.ChildWorkflows == nil || task.Goober == "" || event.Runner["goober"] != task.Goober {
		return AdmissionContext{}, ErrAuthorityUnavailable
	}
	if !pinnedPermissions(id, task, pinned.Admission) {
		return AdmissionContext{}, ErrAuthorityUnavailable
	}
	// Snapshot current authority independently of whether a new proposal can
	// still run. Exact pinned stage checks above remain mandatory.
	raw, err := json.Marshal(pinned.Admission)
	if err != nil {
		return AdmissionContext{}, err
	}
	var copy AdmissionContext
	err = json.Unmarshal(raw, &copy)
	return copy, err
}

func pinnedPermissions(id journal.RunIdentity, task apiv1.Task, admission AdmissionContext) bool {
	if admission.Gaggle.Name != id.Gaggle || !blobstore.ValidDigest(admission.ConfigDigest) {
		return false
	}
	backend := BackendRunner
	if id.EngineDriven() {
		backend = BackendEngine
	}
	if admission.Backend != backend || admission.AllowPRPublication && !task.ChildWorkflows.AllowPRPublication {
		return false
	}
	for _, grant := range admission.GrantedCapabilities {
		if !slices.Contains(task.Capabilities, grant) {
			return false
		}
	}
	return true
}

func matchesEnvelope(env apiv1.InvocationEnvelope, id journal.RunIdentity, event journal.Event, admission AdmissionContext) bool {
	return env.TaskID == id.RunID+":"+event.Stage && env.Attempt == int32(event.Attempt) &&
		env.WorkflowID == id.Workflow && env.InstanceID == id.InstanceID && env.Gaggle == id.Gaggle &&
		env.ConfigGeneration == id.ConfigGeneration && env.GooberDigest == id.GooberDigest &&
		env.Goober == admission.ParentTask.Goober && envelopeCapabilities(env.Capabilities, admission)
}

func envelopeCapabilities(grants []string, admission AdmissionContext) bool {
	unique := slices.Clone(grants)
	slices.Sort(unique)
	if len(slices.Compact(unique)) != len(grants) {
		return false
	}
	for _, grant := range grants {
		if !slices.Contains(admission.ParentTask.Capabilities, grant) {
			return false
		}
	}
	for _, grant := range admission.GrantedCapabilities {
		if !slices.Contains(grants, grant) {
			return false
		}
	}
	return true
}

func sameJSON(left, right any) bool {
	a, err := json.Marshal(left)
	if err != nil {
		return false
	}
	b, err := json.Marshal(right)
	return err == nil && string(a) == string(b)
}
