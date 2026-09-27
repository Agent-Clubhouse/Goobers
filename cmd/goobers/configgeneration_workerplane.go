package main

import (
	"context"
	"os"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
)

func (w *workerSeams) mergeAuthorityContext(ctx context.Context, env apiv1.InvocationEnvelope) (context.Context, error) {
	if w.recoveryEmitter == nil || w.recoveryEmitter.BaseURL == "" {
		return ctx, nil
	}
	if !slices.Contains(env.Capabilities, string(capability.GitHubPRMerge)) && !slices.Contains(env.Capabilities, string(capability.ADOPRComplete)) {
		return ctx, nil
	}
	// The parent worker's ambient token may authorize surrender and credential
	// materialization. It must never reach the CLI child. Mint the read-plane
	// capability separately, with the same scope/TTL as dispatched stage pods.
	if w.journalMinter == nil {
		// Preserve explicitly unauthenticated loopback workers. The client rejects
		// hostnames and non-loopback addresses; authenticated daemons still deny it.
		_, err := journalclient.NewHTTP(journalclient.HTTPConfig{BaseURL: w.recoveryEmitter.BaseURL, RunID: env.RunID, Gaggle: env.Gaggle, AllowAnonymousLoopback: true})
		if err != nil {
			return ctx, err
		}
		return executor.WithJournalPlane(ctx, executor.JournalPlane{Endpoint: w.recoveryEmitter.BaseURL}), nil
	}
	token, err := w.journalMinter.MintScoped(env.RunID, dispatcher.PlaneTokenTTL, podauth.ScopeJournal)
	if err != nil {
		return ctx, err
	}
	if w.shared != nil {
		w.shared.Register([]byte(token))
	}
	return executor.WithJournalPlane(ctx, executor.JournalPlane{Endpoint: w.recoveryEmitter.BaseURL, Token: token}), nil
}

func wireWorkerJournalAuthority(seams *workerSeams, emitter *livejournal.HTTPEmitter, root string) error {
	if seams == nil {
		return nil
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		return err
	}
	signed, err := podTokenMinter(cfg)
	if err != nil {
		return err
	}
	if signed != nil {
		seams.journalMinter = signed
	}
	seams.recoveryEmitter = emitter
	seams.checkpointEmitter = emitter
	seams.executionFence = remoteSharedExecutionFence(emitter.BaseURL, func(runID string) (string, error) { return workerExecutionBearer(emitter, runID) })
	return nil
}

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
