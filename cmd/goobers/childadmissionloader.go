package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

// loadPinnedChildStage is shared by run-bound tool authority and queued child
// execution. The caller supplies an immutable, successfully APPLIED currentSet
// under its config/authority lock. Pending edits on disk are never authority.
// Credentials remain in normal execution wiring; this loader performs offline
// harness admission only and never revives an expired stage tool grant.
func loadPinnedChildStage(ctx context.Context, layout instance.Layout, cfg *instance.Config, currentSet *instance.ConfigSet, parent journal.RunIdentity, stage string) (childworkflow.Authority, error) {
	if cfg == nil || currentSet == nil || parent.Child != nil || parent.ConfigGeneration == "" || parent.Gaggle == "" || parent.Workflow == "" {
		return childworkflow.Authority{}, childworkflow.ErrAuthorityUnavailable
	}
	owner, err := layout.ReadIdentity()
	if err != nil || parent.InstanceID != owner {
		return childworkflow.Authority{}, fmt.Errorf("child parent instance identity unavailable or mismatched: %w", errors.Join(childworkflow.ErrAuthorityUnavailable, err))
	}
	store, err := executionGenerationStore(layout)
	if err != nil {
		return childworkflow.Authority{}, err
	}
	directory, lease, err := store.Acquire(ctx, parent.ConfigGeneration)
	if err != nil {
		return childworkflow.Authority{}, err
	}
	defer func() { _ = lease.Release() }()
	set, _, err := loadConfigDirectory(directory)
	if err != nil {
		return childworkflow.Authority{}, fmt.Errorf("load child parent generation: %w", err)
	}
	backend := childworkflow.BackendRunner
	if parent.EngineDriven() {
		backend = childworkflow.BackendEngine
	}
	selected := childworkflow.ParentSelection{Gaggle: parent.Gaggle, Workflow: parent.Workflow, Stage: stage}
	pinned, machine, err := childStageCatalog(cfg, set, selected, backend)
	if err != nil {
		return childworkflow.Authority{}, err
	}
	if machine.Digest() != parent.WorkflowDigest {
		return childworkflow.Authority{}, errors.New("child parent workflow digest does not match retained generation")
	}
	if err = verifyChildParentGoobers(directory, pinned, machine, parent.GooberDigest); err != nil {
		return childworkflow.Authority{}, err
	}
	// Current policy narrows only effective grants and available Goobers. The
	// pinned ParentTask remains byte-for-byte the parent's compiled stage.
	current, err := childCurrentCustodyPolicy(cfg, currentSet, selected)
	if err != nil {
		return childworkflow.Authority{}, err
	}
	if err = pinChildAdmissionDigest(&pinned, parent); err != nil {
		return childworkflow.Authority{}, err
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
	intersectChildPermissions(&pinned, current)
	return snapshotChildAuthority(childworkflow.Authority{
		Origin:    childworkflow.Origin{Gaggle: parent.Gaggle, RunID: parent.RunID, ConfigDigest: pinned.ConfigDigest, PolicyDigest: childworkflow.AuthorityPolicyDigest(pinned)},
		Admission: pinned, ConfigGeneration: parent.ConfigGeneration,
		ParentWorkflow: parent.Workflow, ParentWorkflowDigest: parent.WorkflowDigest, ParentGooberDigest: parent.GooberDigest,
	})
}

func snapshotChildAuthority(authority childworkflow.Authority) (childworkflow.Authority, error) {
	raw, err := json.Marshal(authority)
	if err != nil {
		return childworkflow.Authority{}, err
	}
	var snapshot childworkflow.Authority
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		return childworkflow.Authority{}, err
	}
	return snapshot, nil
}

func childStageCatalog(cfg *instance.Config, source *instance.ConfigSet, parent childworkflow.ParentSelection, backend childworkflow.Backend) (childworkflow.AdmissionContext, *workflow.Machine, error) {
	selected, err := childSelectedConfig(source, parent)
	if err != nil {
		return childworkflow.AdmissionContext{}, nil, err
	}
	instance.ApplyGaggleCICommand(selected)
	instance.ApplyGaggleOutboxMirror(selected)
	input, err := childworkflow.ConfiguredAdmission(parent, backend, cfg, selected, admitChildValidationGoobers)
	if err != nil {
		return childworkflow.AdmissionContext{}, nil, err
	}
	// Use the same structural compiler and raw Goober selection as normal daemon
	// admission. Resolved model/options are used only in the Goober identity pin.
	machines, err := compileWorkflowMachines(selected, goobersByName(selected), input.KnownHarnesses, cfg.ExternalTelemetryConnectorNames())
	if err != nil {
		return childworkflow.AdmissionContext{}, nil, err
	}
	machine := machines[localscheduler.WorkflowIdentity{Gaggle: parent.Gaggle, Workflow: parent.Workflow}]
	if machine == nil {
		return childworkflow.AdmissionContext{}, nil, childworkflow.ErrAuthorityUnavailable
	}
	task, ok := machine.Task(parent.Stage)
	if !ok || task.Type != apiv1.TaskAgentic || task.ChildWorkflows == nil {
		return childworkflow.AdmissionContext{}, nil, fmt.Errorf("child parent compiled stage unavailable: found=%t type=%s policy=%t", ok, task.Type, task.ChildWorkflows != nil)
	}
	input.ParentTask = task
	input.GrantedCapabilities = slices.Clone(task.Capabilities)
	return input, machine, nil
}

// Selecting deep copies prevents inheritance/defaulting from mutating the
// applied snapshot while another run is reading it.
func childSelectedConfig(source *instance.ConfigSet, parent childworkflow.ParentSelection) (*instance.ConfigSet, error) {
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

func verifyChildParentGoobers(directory string, input childworkflow.AdmissionContext, machine *workflow.Machine, expected string) error {
	instructions, err := loadGooberInstructions(directory, input.Goobers)
	if err != nil {
		return err
	}
	skills, err := loadGooberSkillPackages(directory, input.Gaggle.Name, input.Goobers)
	if err != nil {
		return err
	}
	digest, err := workflow.ComputeGooberDigest(machine.Def, input.Goobers, instructions, skills)
	if err != nil {
		return err
	}
	if digest != expected {
		return errors.New("child parent Goober digest does not match retained generation")
	}
	return nil
}

func pinChildAdmissionDigest(input *childworkflow.AdmissionContext, parent journal.RunIdentity) error {
	// Declarative input identity is separate from the archive address. Runtime
	// instance policy is intentionally included; incompatible changes refuse an
	// accepted proposal instead of silently changing placement or grants.
	raw, err := json.Marshal(struct {
		Generation, WorkflowDigest, GooberDigest string
		Admission                                childworkflow.AdmissionContext
	}{parent.ConfigGeneration, parent.WorkflowDigest, parent.GooberDigest, *input})
	if err != nil {
		return err
	}
	input.ConfigDigest = journal.Digest(raw)
	return nil
}

func intersectChildPermissions(pinned *childworkflow.AdmissionContext, current childworkflow.AdmissionContext) {
	if current.ParentTask.ChildWorkflows == nil {
		pinned.GrantedCapabilities, pinned.AllowPRPublication = nil, false
		clear(pinned.Goobers)
		return
	}
	pinned.GrantedCapabilities = childPermissionIntersection(pinned.GrantedCapabilities, current.GrantedCapabilities)
	pinned.GrantedCapabilities = childPermissionIntersection(pinned.GrantedCapabilities, current.ParentTask.ChildWorkflows.AllowedCapabilities)
	pinned.AllowPRPublication = pinned.AllowPRPublication && current.AllowPRPublication
	for name, g := range pinned.Goobers {
		now, ok := current.Goobers[name]
		if !ok || !slices.Contains(current.ParentTask.ChildWorkflows.AllowedGoobers, name) {
			delete(pinned.Goobers, name)
			continue
		}
		g.Capabilities = childPermissionIntersection(g.Capabilities, now.Capabilities)
		pinned.Goobers[name] = g
	}
}

func childPermissionIntersection(pinned, current []string) []string {
	var allowed []string
	for _, grant := range pinned {
		if slices.Contains(current, grant) {
			allowed = append(allowed, grant)
		}
	}
	return allowed
}
