package main

import (
	"context"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
)

func workerExecutionBearer(emitter *livejournal.HTTPEmitter, runID string) (string, error) {
	if emitter.Token != "" {
		return emitter.Token, nil
	}
	if emitter.Minter == nil {
		return "", nil
	}
	minter, ok := emitter.Minter.(interface {
		MintScoped(string, time.Duration, ...string) (string, error)
	})
	if !ok {
		return "", fmt.Errorf("worker shared execution requires a run-scoped bearer")
	}
	return minter.MintScoped(runID, 5*time.Minute, podauth.ScopeClaims)
}

func remoteSharedExecutionFence(baseURL string, tokenForRun func(string) (string, error)) executionFenceStart {
	return func(ctx context.Context, env apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
		stop := func() {}
		read := func(ctx context.Context) (string, claimsclient.Listing, error) {
			token, err := tokenForRun(env.RunID)
			if err != nil {
				return "", claimsclient.Listing{}, err
			}
			client, err := claimsclient.NewExecutionObserver(claimsclient.HTTPConfig{BaseURL: baseURL, Token: token, RunID: env.RunID})
			if err != nil {
				return "", claimsclient.Listing{}, err
			}
			return client.ExecutionSnapshot(ctx)
		}
		mode, initial, err := read(ctx)
		if err != nil || mode != "shared" {
			return ctx, stop, err
		}
		first := true
		return claimsclient.StartExecutionFence(ctx, env.RunID, func(ctx context.Context) (claimsclient.Listing, error) {
			if first {
				first = false
				return initial, nil
			}
			mode, listing, err := read(ctx)
			if err == nil && mode != "shared" {
				err = fmt.Errorf("shared execution policy changed during invocation")
			}
			return listing, err
		})
	}
}
