package main

import (
	"context"
	"os"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/mergepolicy"
	"github.com/goobers/goobers/providers"
)

func requireCurrentStageMergeAuthority(cap capability.Capability) error {
	return requireCurrentStageMergeAuthorityContext(context.Background(), cap)
}
func requireCurrentStageMergeAuthorityContext(ctx context.Context, cap capability.Capability) error {
	if endpoint, token := os.Getenv(journalclient.EnvEndpoint), os.Getenv(journalclient.EnvToken); endpoint != "" || token != "" {
		ctx = executor.WithJournalPlane(ctx, executor.JournalPlane{Endpoint: endpoint, Token: token})
	}
	return runtimewiring.RequireInvocationMergeAuthority(ctx, instance.NewLayout(os.Getenv(executor.InstanceRootEnvVar)), apiv1.InvocationEnvelope{
		RunID: os.Getenv(executor.RunIDEnvVar), ConfigGeneration: os.Getenv(executor.ConfigGenerationEnvVar), Gaggle: os.Getenv(executor.GaggleEnvVar),
		WorkflowID: os.Getenv(executor.WorkflowEnvVar), TaskID: stageMergeAuthorityTask(), Capabilities: []string{string(cap)},
	})
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

func stageMergeAuthorityTask() string {
	if task := os.Getenv(executor.TaskEnvVar); task != "" {
		return task
	}
	return os.Getenv("GOOBERS_STAGE")
}
