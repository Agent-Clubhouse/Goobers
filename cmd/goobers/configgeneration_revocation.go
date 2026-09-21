package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/mergepolicy"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

// Pins preserve execution semantics, not irrevocable merge authority (SEC-053).
// Consult the current operator-owned tree before materializing a merge grant.
// Invalid current configuration fails closed for merging, without invalidating
// ordinary stages whose admitted generation remains independently verifiable.
func requireCurrentMergeAuthority(layout instance.Layout, env apiv1.InvocationEnvelope) error {
	if env.ConfigGeneration == "" {
		return nil
	}
	return checkCurrentMergeAuthority(layout, env)
}

func checkCurrentMergeAuthority(layout instance.Layout, env apiv1.InvocationEnvelope) error {
	requested := make([]string, 0, 2)
	for _, name := range []string{string(capability.GitHubPRMerge), string(capability.ADOPRComplete)} {
		if slices.Contains(env.Capabilities, name) {
			requested = append(requested, name)
		}
	}
	if len(requested) == 0 {
		return nil
	}
	env.TaskID = strings.TrimPrefix(env.TaskID, env.RunID+":")
	set, _, err := instance.LoadConfigDir(instance.NewLayout(layout.Root).ConfigDir())
	if err != nil {
		return fmt.Errorf("current merge authority unavailable: %w", err)
	}
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)
	goobers, err := resolveGoobersForGaggle(set, env.Gaggle)
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

func withCurrentMergeAuthority(layout instance.Layout, next executionFenceStart) executionFenceStart {
	return func(ctx context.Context, env apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
		ctx = executor.WithConfigDirectory(ctx, layout.ConfigDir())
		if err := requireInvocationMergeAuthority(ctx, layout, env); err != nil {
			return ctx, func() {}, err
		}
		return next(ctx, env)
	}
}

func requireCurrentStageMergeAuthority(cap capability.Capability) error {
	return requireCurrentStageMergeAuthorityContext(context.Background(), cap)
}
func requireCurrentStageMergeAuthorityContext(ctx context.Context, cap capability.Capability) error {
	if endpoint, token := os.Getenv(journalclient.EnvEndpoint), os.Getenv(journalclient.EnvToken); endpoint != "" || token != "" {
		ctx = executor.WithJournalPlane(ctx, executor.JournalPlane{Endpoint: endpoint, Token: token})
	}
	return requireInvocationMergeAuthority(ctx, instance.NewLayout(os.Getenv(executor.InstanceRootEnvVar)), apiv1.InvocationEnvelope{
		RunID: os.Getenv(executor.RunIDEnvVar), ConfigGeneration: os.Getenv(executor.ConfigGenerationEnvVar), Gaggle: os.Getenv(executor.GaggleEnvVar),
		WorkflowID: os.Getenv(executor.WorkflowEnvVar), TaskID: stageMergeAuthorityTask(), Capabilities: []string{string(cap)},
	})
}

func runnerExecutionFence(input runnerCompositionInput) executionFenceStart {
	next := input.ExecutionFence
	if next == nil {
		next = localSharedExecutionFence(input.Layout)
	}
	return withCurrentMergeAuthority(input.Layout, next)
}

type currentMergeAuthorityLander struct {
	mergepolicy.Lander
	capability capability.Capability
}

func mergeLanderForCurrentAuthority(policy providers.MergePolicy, authority capability.Capability) (mergepolicy.Lander, error) {
	lander, err := mergepolicy.ForPolicy(policy)
	if err != nil {
		return nil, err
	}
	return currentMergeAuthorityLander{Lander: lander, capability: authority}, nil
}
func (l currentMergeAuthorityLander) Land(ctx context.Context, provider *providers.Dispatcher, request mergepolicy.Request) (mergepolicy.Result, error) {
	if err := requireCurrentStageMergeAuthorityContext(ctx, l.capability); err != nil {
		return mergepolicy.Result{}, err
	}
	return l.Lander.Land(ctx, provider, request)
}

func requireInvocationMergeAuthority(ctx context.Context, layout instance.Layout, env apiv1.InvocationEnvelope) error {
	plane, remote := executor.JournalPlaneFromContext(ctx)
	if !remote {
		return requireCurrentMergeAuthority(layout, env)
	}
	for _, name := range []string{string(capability.GitHubPRMerge), string(capability.ADOPRComplete)} {
		if !slices.Contains(env.Capabilities, name) {
			continue
		}
		client, err := journalclient.NewHTTP(journalclient.HTTPConfig{AllowAnonymousLoopback: true, BaseURL: plane.Endpoint, Token: plane.Token, RunID: env.RunID, Gaggle: env.Gaggle})
		if err != nil {
			return err
		}
		if err := client.RequireMergeAuthority(ctx, strings.TrimPrefix(env.TaskID, env.RunID+":"), name); err != nil {
			return err
		}
	}
	return nil
}

func stageMergeAuthorityTask() string {
	if task := os.Getenv(executor.TaskEnvVar); task != "" {
		return task
	}
	return os.Getenv("GOOBERS_STAGE")
}
