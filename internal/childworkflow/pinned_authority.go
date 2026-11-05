package childworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

// Generation is one retained config generation, pinned for the caller.
type Generation struct {
	// Directory is the generation's read-only config tree; Goober
	// instructions and skill packages are read from it.
	Directory string
	// Set is the generation's loaded configuration.
	Set *instance.ConfigSet
	// Release unpins the generation. It is never nil on success.
	Release func() error
}

// GenerationSource retains config generations by digest: the local daemon's
// execution generation store, or the cloud's object-store archive. The
// generation is always the one a parent run names (its ConfigGeneration),
// never whatever is currently applied.
type GenerationSource interface {
	// Acquire pins and loads generation. A missing or unloadable generation
	// is an error; the caller releases a successful pin.
	Acquire(ctx context.Context, generation string) (Generation, error)
}

// PinnedAuthority reconstructs a parent stage's child authority from the
// retained generation the parent ran under, narrowed by currentSet: the
// immutable, successfully APPLIED configuration supplied under the caller's
// config/authority lock. Pending edits are never authority. The caller has
// already verified that parent belongs to this instance. It performs offline
// harness admission only and never revives an expired stage tool grant.
func PinnedAuthority(ctx context.Context, source GenerationSource, cfg *instance.Config, currentSet *instance.ConfigSet, parent journal.RunIdentity, stage string, admit GooberAdmission) (Authority, error) {
	if source == nil || cfg == nil || currentSet == nil || parent.Child != nil || parent.ConfigGeneration == "" || parent.Gaggle == "" || parent.Workflow == "" {
		return Authority{}, ErrAuthorityUnavailable
	}
	generation, err := source.Acquire(ctx, parent.ConfigGeneration)
	if err != nil {
		return Authority{}, err
	}
	if generation.Release == nil {
		return Authority{}, errors.New("childworkflow: generation source returned no release")
	}
	defer func() { _ = generation.Release() }()
	if generation.Set == nil || generation.Directory == "" {
		return Authority{}, errors.New("childworkflow: generation source returned an unloaded generation")
	}
	backend := BackendRunner
	if parent.EngineDriven() {
		backend = BackendEngine
	}
	selected := ParentSelection{Gaggle: parent.Gaggle, Workflow: parent.Workflow, Stage: stage}
	pinned, machine, err := StageCatalog(cfg, generation.Set, selected, backend, admit)
	if err != nil {
		return Authority{}, err
	}
	if machine.Digest() != parent.WorkflowDigest {
		return Authority{}, fmt.Errorf("%w: child parent workflow digest does not match retained generation", ErrFrozenChildDrift)
	}
	if err = verifyParentGoobers(generation.Directory, generation.Set, pinned, machine, parent.GooberDigest); err != nil {
		return Authority{}, err
	}
	// Current policy narrows only effective grants and available Goobers. The
	// pinned ParentTask remains byte-for-byte the parent's compiled stage.
	current, err := CurrentCustodyPolicy(cfg, currentSet, selected, admit)
	if err != nil {
		return Authority{}, err
	}
	if err = pinAdmissionDigest(&pinned, parent); err != nil {
		return Authority{}, err
	}
	if !reflect.DeepEqual(current.Gaggle.Spec.Project, pinned.Gaggle.Spec.Project) || !reflect.DeepEqual(current.Gaggle.Spec.AdditionalRepos, pinned.Gaggle.Spec.AdditionalRepos) {
		pinned.ExecutionRefusal = "current child repository scope differs from pinned parent"
		pinned.WorkspaceMutationDenied = true
	}
	if current.ParentTask.ChildWorkflows == nil {
		pinned.ExecutionRefusal = "current parent stage no longer permits new children"
	} else if current.ParentTask.ChildWorkflows.EffectiveMaxChildren() < pinned.ParentTask.ChildWorkflows.EffectiveMaxChildren() {
		pinned.ExecutionRefusal = "current child allowance is below the pinned occurrence ceiling"
	}
	pinned.WorkspaceMutationDenied = pinned.WorkspaceMutationDenied || (pinned.ParentTask.Workspace.IsWritableRepo() && !current.ParentTask.Workspace.IsWritableRepo())
	intersectPermissions(&pinned, current)
	return snapshotAuthority(Authority{
		Origin:    Origin{Gaggle: parent.Gaggle, RunID: parent.RunID, ConfigDigest: pinned.ConfigDigest, PolicyDigest: AuthorityPolicyDigest(pinned)},
		Admission: pinned, ConfigGeneration: parent.ConfigGeneration,
		ParentWorkflow: parent.Workflow, ParentWorkflowDigest: parent.WorkflowDigest, ParentGooberDigest: parent.GooberDigest,
		Execution: ExecutionBindingFor(parent, pinned),
	})
}

// ExecutionBindingFor is where an engine child acts, taken only from the
// parent's durable identity and its pinned generation, never current config.
func ExecutionBindingFor(parent journal.RunIdentity, pinned AdmissionContext) *ExecutionBinding {
	if pinned.Backend != BackendEngine {
		return nil
	}
	repo := pinned.Gaggle.Spec.Project
	if parent.WorkspaceRepository != nil {
		repo = *parent.WorkspaceRepository
	}
	return &ExecutionBinding{InstanceID: parent.InstanceID, RepoRef: repo, BranchNamespace: providers.NormalizeBranchNamespace(pinned.Gaggle.Spec.BranchNamespace)}
}

func snapshotAuthority(authority Authority) (Authority, error) {
	raw, err := json.Marshal(authority)
	if err != nil {
		return Authority{}, err
	}
	var snapshot Authority
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		return Authority{}, err
	}
	return snapshot, nil
}

// StageCatalog compiles the parent's stage from source with the same
// structural compiler and raw Goober selection as normal daemon admission.
func StageCatalog(cfg *instance.Config, source *instance.ConfigSet, parent ParentSelection, backend Backend, admit GooberAdmission) (AdmissionContext, *workflow.Machine, error) {
	selected, err := selectedConfig(source, parent)
	if err != nil {
		return AdmissionContext{}, nil, err
	}
	instance.ApplyGaggleCICommand(selected)
	instance.ApplyGaggleOutboxMirror(selected)
	input, err := ConfiguredAdmission(parent, backend, cfg, selected, admit)
	if err != nil {
		return AdmissionContext{}, nil, err
	}
	// Use the same structural compiler and raw Goober selection as normal daemon
	// admission. Resolved model/options are used only in the Goober identity pin.
	machines, err := runtimewiring.CompileWorkflowMachines(selected, runtimewiring.GoobersByName(selected), input.KnownHarnesses, cfg.ExternalTelemetryConnectorNames())
	if err != nil {
		return AdmissionContext{}, nil, err
	}
	machine := machines[localscheduler.WorkflowIdentity{Gaggle: parent.Gaggle, Workflow: parent.Workflow}]
	if machine == nil {
		return AdmissionContext{}, nil, ErrAuthorityUnavailable
	}
	task, ok := machine.Task(parent.Stage)
	if !ok || task.Type != apiv1.TaskAgentic || task.ChildWorkflows == nil {
		return AdmissionContext{}, nil, fmt.Errorf("child parent compiled stage unavailable: found=%t type=%s policy=%t", ok, task.Type, task.ChildWorkflows != nil)
	}
	input.ParentTask = task
	input.GrantedCapabilities = slices.Clone(task.Capabilities)
	return input, machine, nil
}

// CurrentCustodyPolicy is the current parent stage's policy. Cleanup of
// accepted custody still requires an enabled current parent stage; removing
// its child opt-in revokes execution, not ownership of prior results.
func CurrentCustodyPolicy(cfg *instance.Config, source *instance.ConfigSet, parent ParentSelection, admit GooberAdmission) (AdmissionContext, error) {
	selected, err := selectedConfig(source, parent)
	if err != nil {
		return AdmissionContext{}, err
	}
	instance.ApplyGaggleCICommand(selected)
	instance.ApplyGaggleOutboxMirror(selected)
	admitted, err := admit(cfg, runtimewiring.GoobersByName(selected))
	if err != nil {
		return AdmissionContext{}, err
	}
	machines, err := runtimewiring.CompileWorkflowMachines(selected, runtimewiring.GoobersByName(selected), admitted.HarnessNames, cfg.ExternalTelemetryConnectorNames())
	if err != nil {
		return AdmissionContext{}, err
	}
	machine := machines[localscheduler.WorkflowIdentity{Gaggle: parent.Gaggle, Workflow: parent.Workflow}]
	if machine == nil {
		return AdmissionContext{}, ErrAuthorityUnavailable
	}
	task, ok := machine.Task(parent.Stage)
	if !ok || task.Type != apiv1.TaskAgentic {
		return AdmissionContext{}, fmt.Errorf("current child parent stage is unavailable")
	}
	policy := AdmissionContext{Gaggle: selected.Gaggles[0], ParentTask: task, Goobers: admitted.Goobers, GrantedCapabilities: slices.Clone(task.Capabilities)}
	if task.ChildWorkflows != nil {
		policy.AllowPRPublication = task.ChildWorkflows.AllowPRPublication
	}
	return policy, nil
}

// Selecting deep copies prevents inheritance/defaulting from mutating the
// applied snapshot while another run is reading it.
func selectedConfig(source *instance.ConfigSet, parent ParentSelection) (*instance.ConfigSet, error) {
	selected := &instance.ConfigSet{}
	for _, g := range source.Gaggles {
		if g.Name == parent.Gaggle {
			if g.Spec.Enabled != nil && !*g.Spec.Enabled {
				return nil, errors.New("child parent gaggle is disabled")
			}
			selected.Gaggles = append(selected.Gaggles, *g.DeepCopy())
		}
	}
	for _, w := range source.Workflows {
		if w.Name == parent.Workflow && w.Spec.Gaggle == parent.Gaggle {
			if w.Spec.Enabled != nil && !*w.Spec.Enabled {
				return nil, errors.New("child parent workflow is disabled")
			}
			selected.Workflows = append(selected.Workflows, *w.DeepCopy())
		}
	}
	for _, g := range source.Goobers {
		if g.Spec.Gaggle == "" || g.Spec.Gaggle == parent.Gaggle {
			selected.Goobers = append(selected.Goobers, *g.DeepCopy())
		}
	}
	if len(selected.Gaggles) != 1 || len(selected.Workflows) != 1 {
		return nil, fmt.Errorf("child parent selection missing or ambiguous: %d gaggles, %d workflows", len(selected.Gaggles), len(selected.Workflows))
	}
	return selected, nil
}

func verifyParentGoobers(directory string, set *instance.ConfigSet, input AdmissionContext, machine *workflow.Machine, expected string) error {
	instructions, err := runtimewiring.LoadGooberInstructions(directory, set, input.Goobers)
	if err != nil {
		return err
	}
	skills, err := runtimewiring.LoadGooberSkillPackages(directory, input.Gaggle.Name, input.Goobers)
	if err != nil {
		return err
	}
	digest, err := workflow.ComputeGooberDigest(machine.Def, input.Goobers, instructions, skills)
	if err != nil {
		return err
	}
	if digest != expected {
		return fmt.Errorf("%w: child parent Goober digest does not match retained generation", ErrFrozenChildDrift)
	}
	return nil
}

func pinAdmissionDigest(input *AdmissionContext, parent journal.RunIdentity) error {
	// Declarative input identity is separate from the archive address. Runtime
	// instance policy is intentionally included; incompatible changes refuse an
	// accepted proposal instead of silently changing placement or grants.
	raw, err := json.Marshal(struct {
		Generation, WorkflowDigest, GooberDigest string
		Admission                                AdmissionContext
	}{parent.ConfigGeneration, parent.WorkflowDigest, parent.GooberDigest, *input})
	if err != nil {
		return err
	}
	input.ConfigDigest = journal.Digest(raw)
	return nil
}

func intersectPermissions(pinned *AdmissionContext, current AdmissionContext) {
	if current.ParentTask.ChildWorkflows == nil {
		pinned.GrantedCapabilities, pinned.AllowPRPublication = nil, false
		clear(pinned.Goobers)
		return
	}
	pinned.GrantedCapabilities = PermissionIntersection(pinned.GrantedCapabilities, current.GrantedCapabilities)
	pinned.GrantedCapabilities = PermissionIntersection(pinned.GrantedCapabilities, current.ParentTask.ChildWorkflows.AllowedCapabilities)
	pinned.AllowPRPublication = pinned.AllowPRPublication && current.AllowPRPublication
	for name, g := range pinned.Goobers {
		now, ok := current.Goobers[name]
		if !ok || !slices.Contains(current.ParentTask.ChildWorkflows.AllowedGoobers, name) {
			delete(pinned.Goobers, name)
			continue
		}
		g.Capabilities = PermissionIntersection(g.Capabilities, now.Capabilities)
		pinned.Goobers[name] = g
	}
}

// PermissionIntersection keeps the pinned grants current policy still allows,
// in pinned order.
func PermissionIntersection(pinned, current []string) []string {
	var allowed []string
	for _, grant := range pinned {
		if slices.Contains(current, grant) {
			allowed = append(allowed, grant)
		}
	}
	return allowed
}
