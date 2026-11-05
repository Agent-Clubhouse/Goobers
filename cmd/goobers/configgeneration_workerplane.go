package main

import (
	"context"
	"os"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
)

func podAgenticMergeAuthorityContext(ctx context.Context, env apiv1.InvocationEnvelope) context.Context {
	if !slices.Contains(env.Capabilities, string(capability.GitHubPRMerge)) && !slices.Contains(env.Capabilities, string(capability.ADOPRComplete)) {
		return ctx
	}
	endpoint := os.Getenv(dispatcher.JournalEndpointEnv)
	if endpoint == "" {
		endpoint = os.Getenv(dispatcher.EnvDaemonAPI)
	}
	return executor.WithJournalPlane(ctx, executor.JournalPlane{Endpoint: endpoint, Token: os.Getenv(dispatcher.JournalTokenEnv)})
}
