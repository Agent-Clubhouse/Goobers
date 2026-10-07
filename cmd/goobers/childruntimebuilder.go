package main

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/worktree"
)

// Generated definitions are private to one admitted execution. The normal
// runtime builder still owns harness, provider, credentials and placement;
// replacing this local set never registers the child name in the scheduler.
type childRuntimeBuilder func(context.Context, childExecutionStart) (preparedChildRuntime, error)

type preparedChildRuntime struct {
	executionGenerationRuntime
	entry     localscheduler.WorkflowEntry
	controls  apiv1.RunControls
	worktrees *worktree.Manager
	release   func()
}

func childRuntimeBuilderFor(layout instance.Layout, retainer *configgeneration.Retainer, cfg *instance.Config, build generationDefinitionBuilder) childRuntimeBuilder {
	return func(ctx context.Context, start childExecutionStart) (preparedChildRuntime, error) {
		if retainer == nil || cfg == nil || build == nil || start.Proposal == nil || start.Envelope.Backend != childworkflow.BackendRunner {
			return preparedChildRuntime{}, errors.New("child execution builder unavailable")
		}
		directory, lease, err := retainer.Store.Acquire(ctx, start.Envelope.ConfigGeneration)
		if err != nil {
			return preparedChildRuntime{}, err
		}
		runtime, err := buildChildRuntime(ctx, layout.WithConfigDir(directory), cfg, build, start)
		if err != nil {
			_ = lease.Release()
			return preparedChildRuntime{}, err
		}
		runtime.release = func() { _ = lease.Release() }
		return runtime, nil
	}
}

func buildChildRuntime(ctx context.Context, layout instance.Layout, cfg *instance.Config, build generationDefinitionBuilder, start childExecutionStart) (preparedChildRuntime, error) {
	if err := ctx.Err(); err != nil {
		return preparedChildRuntime{}, err
	}
	set, report, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		return preparedChildRuntime{}, fmt.Errorf("load child generation: %w", err)
	}
	// Source already passed schema, pinned parent policy and current permission
	// checks. Keep the archive untouched and compile only this generated machine.
	set.Workflows = []apiv1.Workflow{*start.Proposal.Workflow.DeepCopy()}
	definitions, err := build(layout, set, report)
	if err != nil {
		return preparedChildRuntime{}, err
	}
	key := localscheduler.WorkflowIdentity{Gaggle: start.Envelope.Gaggle, Workflow: start.Envelope.Workflow}
	runtime := preparedChildRuntime{executionGenerationRuntime: executionGenerationRuntime{
		runner: definitions.Runners[key.Gaggle], machine: definitions.Machines[key],
		gooberDigest: definitions.GooberDigests[key], repoRef: definitions.RepoRefs[key],
	}, worktrees: definitions.WorktreesByGaggle[key.Gaggle]}
	if runtime.runner == nil || runtime.machine == nil || runtime.machine.Digest() != start.Envelope.WorkflowDigest {
		return preparedChildRuntime{}, errors.New("generated runtime differs from accepted machine")
	}
	for _, entry := range definitions.Entries {
		if entry.Gaggle == key.Gaggle && entry.Workflow == key.Workflow {
			runtime.entry = entry
			break
		}
	}
	if runtime.entry.Workflow == "" {
		return preparedChildRuntime{}, errors.New("generated runtime has no normal admission requirements")
	}
	var gaggle apiv1.Gaggle
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == key.Gaggle {
			gaggle = set.Gaggles[i]
			break
		}
	}
	controls, err := resolveWorkflowRunControls(cfg, runtime.repoRef, gaggle, start.Proposal.Workflow)
	if err != nil {
		return preparedChildRuntime{}, err
	}
	runtime.controls = controls.Overrides()
	return runtime, nil
}
