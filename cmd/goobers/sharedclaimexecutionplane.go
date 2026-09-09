package main

import (
	"context"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
)

func workerExecutionBearer(emitter *livejournal.HTTPEmitter, runID string) (string, error) {
	if emitter.Token != "" {
		return emitter.Token, nil
	}
	minter, ok := emitter.Minter.(interface {
		MintScoped(string, time.Duration, ...string) (string, error)
	})
	if !ok {
		return "", fmt.Errorf("worker shared execution requires a run-scoped bearer")
	}
	return minter.MintScoped(runID, 5*time.Minute, podauth.ScopeClaims)
}

func (s *daemonClaimService) executionClaimVisibility(ctx context.Context, runID string) (string, error) {
	directory, err := runDirFor(s.layout, runID)
	if err != nil {
		return "", err
	}
	reader, err := journal.OpenReadOnly(directory)
	if err != nil {
		return "", err
	}
	identity, err := reader.Identity()
	if err != nil {
		return "", err
	}
	if identity.RunID != runID {
		return "", fmt.Errorf("execution requires the owning run identity")
	}
	mode, err := localExecutionClaimVisibility(s.layout, apiv1.InvocationEnvelope{RunID: runID, Gaggle: identity.Gaggle, WorkflowID: identity.Workflow})
	if err != nil || mode != "shared" {
		return mode, err
	}
	phase, err := reader.PhaseBounded(ctx)
	if err != nil {
		return "", err
	}
	if terminalRunPhase(phase) {
		return "", claimsclient.ErrSharedExecutionExpired
	}
	return mode, nil
}

// The parent runtime's bearer stays in the parent. Nothing in this reader
// passes claims authority to a shell command or an agentic subprocess.
func remoteSharedExecutionFence(baseURL string, tokenForRun func(string) (string, error)) executionFenceStart {
	return func(ctx context.Context, env apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
		stop := func() {}
		read := func(ctx context.Context) (string, claimsclient.Listing, error) {
			token, err := tokenForRun(env.RunID)
			if err != nil {
				return "", claimsclient.Listing{}, err
			}
			client, err := claimsclient.NewHTTP(claimsclient.HTTPConfig{BaseURL: baseURL, Token: token, RunID: env.RunID})
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
