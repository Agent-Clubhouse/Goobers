package runtimewiring

import (
	"context"
	"fmt"
	"slices"

	"github.com/goobers/goobers/internal/journal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journalclient"

	"github.com/goobers/goobers/internal/workflow"
)

// RequireCurrentMergeAuthority keeps pinned execution semantics separate from
// current operator-owned merge grants (SEC-053). Invalid current config fails
// closed for merging without invalidating ordinary pinned stages.
func RequireCurrentMergeAuthority(layout instance.Layout, env apiv1.InvocationEnvelope) error {
	if env.ConfigGeneration == "" {
		return nil
	}
	return CheckCurrentMergeAuthority(layout, env)
}

// CheckCurrentMergeAuthority enforces current merge grants for pinned invocations (SEC-053).
func CheckCurrentMergeAuthority(layout instance.Layout, env apiv1.InvocationEnvelope) error {
	requested := make([]string, 0, 2)
	for _, name := range []string{string(capability.GitHubPRMerge), string(capability.ADOPRComplete)} {
		if slices.Contains(env.Capabilities, name) {
			requested = append(requested, name)
		}
	}
	if len(requested) == 0 {
		return nil
	}
	env.TaskID = journal.StageArtifactName(env.RunID, env.TaskID)
	set, report, err := instance.LoadConfigDir(instance.NewLayout(layout.Root).ConfigDir())
	if err != nil {
		return fmt.Errorf("current merge authority unavailable: %w (%s)", err, ValidationIssueSummary(report))
	}
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)
	goobers, err := ResolveGoobersForGaggle(set, env.Gaggle)
	if err != nil {
		return fmt.Errorf("merge authority revoked: %w", err)
	}
	for _, definition := range set.Workflows {
		if definition.Name != env.WorkflowID || definition.Spec.Gaggle != env.Gaggle {
			continue
		}
		machine, err := workflow.Compile(workflow.Definition{Name: definition.Name, Version: 1, DSLVersion: definition.DSLVersion, Spec: definition.Spec, Annotations: definition.Annotations}, workflow.WithGoobers(goobers), workflow.WithPreviewFeatures(workflow.PreviewFeaturesEnabled(definition.Annotations)))
		if err != nil {
			return fmt.Errorf("current merge authority unverifiable: %w", err)
		}
		var capabilities []string
		if task, ok := machine.Task(env.TaskID); ok {
			capabilities = task.Capabilities
		}
		if gate, ok := machine.Gate(env.TaskID); ok && gate.Agentic != nil {
			capabilities = goobers[gate.Agentic.Goober].Capabilities
		}
		for _, requested := range requested {
			if !slices.Contains(capabilities, requested) {
				return fmt.Errorf("merge authority revoked for stage %q: %s", env.TaskID, requested)
			}
		}
		return nil
	}
	return fmt.Errorf("merge authority revoked: workflow %q is no longer configured", env.WorkflowID)
}

// WithCurrentMergeAuthority binds nested commands to the admitted config tree
// and checks current merge grants before starting claim observation.
func WithCurrentMergeAuthority(layout instance.Layout, next ExecutionFenceStart) ExecutionFenceStart {
	return func(ctx context.Context, env apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
		ctx = executor.WithConfigDirectory(ctx, layout.ConfigDir())
		if err := RequireInvocationMergeAuthority(ctx, layout, env); err != nil {
			return ctx, func() {}, err
		}
		return next(ctx, env)
	}
}

// RequireInvocationMergeAuthority checks the current daemon grant when a
// journal plane is bound, otherwise the current operator-owned local tree.
func RequireInvocationMergeAuthority(ctx context.Context, layout instance.Layout, env apiv1.InvocationEnvelope) error {
	plane, remote := executor.JournalPlaneFromContext(ctx)
	if !remote {
		return RequireCurrentMergeAuthority(layout, env)
	}
	for _, name := range []string{string(capability.GitHubPRMerge), string(capability.ADOPRComplete)} {
		if !slices.Contains(env.Capabilities, name) {
			continue
		}
		client, err := journalclient.NewHTTP(journalclient.HTTPConfig{AllowAnonymousLoopback: true, BaseURL: plane.Endpoint, Token: plane.Token, RunID: env.RunID, Gaggle: env.Gaggle})
		if err != nil {
			return err
		}
		if err := client.RequireMergeAuthority(ctx, journal.StageArtifactName(env.RunID, env.TaskID), name); err != nil {
			return err
		}
	}
	return nil
}
