package main

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"

	"github.com/goobers/goobers/internal/sessionops"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workflow"
)

// daemonSessionRuntime keeps the current applied generation separate from each
// conversation's immutable profile. Reload publishes the pointer while holding
// the existing interactive policy barrier; creation never reads a mutable file.
type daemonSessionRuntime struct {
	setup      *schedulerSetup
	generation atomic.Pointer[string]
	process    harness.ProcessRunner
	operations *sessionops.Bridge
}

func (r *daemonSessionRuntime) replaceGeneration(generation string) { r.generation.Store(&generation) }
func (r *daemonSessionRuntime) pin(ctx context.Context, gaggle, name string) (sessioning.Profile, error) {
	generation := r.generation.Load()
	if generation == nil || *generation == "" {
		return sessioning.Profile{}, errors.New("interactive session has no applied generation")
	}
	profile := sessioning.Profile{Goober: name, ConfigGeneration: *generation}
	execution, release, err := r.load(ctx, gaggle, profile)
	if err != nil {
		return sessioning.Profile{}, err
	}
	defer release()
	profile.GooberDigest = execution.source.GooberDigest
	return profile, nil
}

func sessionMachine(gaggle, goober string) (*workflow.Machine, error) {
	return workflow.Compile(workflow.Definition{Name: "interactive-session", DSLVersion: "3.1", Version: 1, Spec: apiv1.WorkflowSpec{Gaggle: gaggle, Start: "respond", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Tasks: []apiv1.Task{{Name: "respond", Type: apiv1.TaskAgentic, Goober: goober, Goal: "Read the immutable interactive-session-context input. Respond to its last human message using the preceding attributed conversation as context. Put the complete response in the result summary. This turn has model-only scratch access; provider and repository changes require a separately authorized typed operation.", Workspace: apiv1.WorkspaceScratch, Capabilities: []string{"agent:model"}, Retry: &apiv1.RetryPolicy{MaxAttempts: 1}}}}})
}

func sessionSelectedConfig(set *instance.ConfigSet, gaggle, name string) (apiv1.Gaggle, map[string]apiv1.GooberSpec, error) {
	var selected []apiv1.Gaggle
	for _, g := range set.Gaggles {
		if g.Name == gaggle {
			selected = append(selected, *g.DeepCopy())
		}
	}
	if len(selected) != 1 || (selected[0].Spec.Enabled != nil && !*selected[0].Spec.Enabled) {
		return apiv1.Gaggle{}, nil, errors.New("interactive session gaggle is unavailable")
	}
	goobers := map[string]apiv1.GooberSpec{}
	for _, g := range set.Goobers {
		if g.Name == name && (g.Spec.Gaggle == "" || g.Spec.Gaggle == gaggle) {
			if _, exists := goobers[name]; exists {
				return apiv1.Gaggle{}, nil, errors.New("interactive session Goober is ambiguous")
			}
			spec := g.DeepCopy().Spec
			if !slices.Contains(spec.Capabilities, "agent:model") || len(spec.MCPServers) > 0 {
				return apiv1.Gaggle{}, nil, errors.New("interactive session requires model capability and no external MCP")
			}
			spec.Capabilities = []string{"agent:model"}
			goobers[name] = spec
		}
	}
	if len(goobers) != 1 {
		return apiv1.Gaggle{}, nil, errors.New("interactive session Goober is unavailable")
	}
	return selected[0], goobers, nil
}

func (r *daemonSessionRuntime) load(ctx context.Context, gaggle string, profile sessioning.Profile) (*interactiveRestartExecution, func(), error) {
	noop := func() {}
	if r.setup == nil || r.setup.Config == nil || r.setup.InteractiveAccess == nil {
		return nil, noop, errors.New("interactive session runtime unavailable")
	}
	layout := instance.NewLayout(r.setup.Root)
	owner, err := layout.ReadIdentity()
	if err != nil {
		return nil, noop, err
	}
	store, err := executionGenerationStore(layout)
	if err != nil {
		return nil, noop, err
	}
	directory, pin, err := store.Acquire(ctx, profile.ConfigGeneration)
	if err != nil {
		return nil, noop, err
	}
	release := func() { _ = pin.Release() }
	execution, err := r.loadProfile(directory, gaggle, profile)
	if err != nil {
		release()
		return nil, noop, err
	}
	execution.source.InstanceID = owner
	return execution, release, nil
}
func (r *daemonSessionRuntime) loadProfile(directory, gaggle string, profile sessioning.Profile) (*interactiveRestartExecution, error) {
	set, _, err := loadConfigDirectory(directory)
	if err != nil {
		return nil, err
	}
	selected, raw, err := sessionSelectedConfig(set, gaggle, profile.Goober)
	if err != nil {
		return nil, err
	}
	admitted, err := admitChildValidationGoobers(r.setup.Config, raw)
	if err != nil {
		return nil, err
	}
	machine, err := sessionMachine(gaggle, profile.Goober)
	if err != nil {
		return nil, err
	}
	instructions, err := loadGooberInstructions(directory, admitted.Goobers)
	if err != nil {
		return nil, err
	}
	skills, err := loadGooberSkillPackages(directory, gaggle, admitted.Goobers)
	if err != nil {
		return nil, err
	}
	digest, err := workflow.ComputeGooberDigest(machine.Def, admitted.Goobers, instructions, skills)
	if err != nil {
		return nil, err
	}
	if profile.GooberDigest != "" && profile.GooberDigest != digest {
		return nil, errors.New("interactive session retained Goober changed")
	}
	e := &interactiveRestartExecution{setup: r.setup, layout: instance.NewLayout(r.setup.Root).WithConfigDir(directory).ForGaggle(gaggle), gaggle: selected, machine: machine, goobers: admitted.Goobers, instructions: instructions, skills: skills, assets: map[string]*gooberassets.Bundle{}, models: map[string]func(context.Context) (string, error){}, process: r.process}
	e.source.Gaggle, e.source.Workflow, e.source.WorkflowVersion = gaggle, machine.Def.Name, machine.Def.Version
	e.source.ConfigGeneration, e.source.WorkflowDigest, e.source.GooberDigest = profile.ConfigGeneration, machine.Digest(), digest
	if err = e.validateBackend(); err != nil {
		return nil, err
	}
	for name, spec := range e.goobers {
		assets, loadErr := gooberassets.Load(filepath.Join(gooberDefinitionDir(directory, spec, name), gooberassets.SourceDir))
		if loadErr != nil {
			return nil, loadErr
		}
		e.assets[name] = assets
	}
	return e, nil
}
