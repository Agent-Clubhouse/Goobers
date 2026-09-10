package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/localscheduler"
)

type executionFenceStart func(context.Context, apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error)

func localSharedExecutionFence(layout instance.Layout) executionFenceStart {
	return func(ctx context.Context, env apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
		mode, err := localExecutionClaimVisibility(layout, env)
		if err != nil || mode != "shared" {
			return ctx, func() {}, err
		}
		return claimsclient.StartExecutionFence(ctx, env.RunID, func(ctx context.Context) (claimsclient.Listing, error) {
			if err := ctx.Err(); err != nil {
				return claimsclient.Listing{}, err
			}
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
			if err != nil {
				return claimsclient.Listing{}, err
			}
			return claimsclient.Listing{Entries: ledger.Snapshot(), History: ledger.HistorySnapshot()}, nil
		})
	}
}

func localExecutionClaimVisibility(layout instance.Layout, env apiv1.InvocationEnvelope) (string, error) {
	directory, err := runDirFor(layout, env.RunID)
	if errors.Is(err, os.ErrNotExist) {
		return "local", requireLocalOnlyClaimConfiguration(layout)
	}
	if err != nil {
		return "", err
	}
	legacy, err := legacyUnpinnedClaimRun(directory, env.RunID)
	if err != nil {
		return "", err
	}
	if legacy {
		return "local", requireLocalOnlyClaimConfiguration(layout)
	}
	resolver := pinnedSharedClaimResolver{layout: layout}
	_, _, mode, err := resolver.claimPolicy(claimsclient.Key{Gaggle: env.Gaggle, Provider: "github"}, env.RunID, env.WorkflowID)
	return mode, err
}

type claimFencedDeterministic struct {
	invoke.Deterministic
	start executionFenceStart
}

func (e claimFencedDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	ctx, stop, err := e.start(ctx, env)
	defer stop()
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	result, err := e.Deterministic.Run(ctx, env, run)
	return result, errors.Join(err, context.Cause(ctx))
}

type claimFencedGoober struct {
	invoke.Goober
	start executionFenceStart
}

// Preserve the runner's asset protection contract across the wrapper.
func (e claimFencedGoober) HasAssetBundle() bool {
	assets, ok := e.Goober.(interface{ HasAssetBundle() bool })
	return ok && assets.HasAssetBundle()
}

func (e claimFencedGoober) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	ctx, stop, err := e.start(ctx, env)
	defer stop()
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	result, err := e.Goober.Invoke(ctx, env)
	return result, errors.Join(err, context.Cause(ctx))
}

func (e claimFencedGoober) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	ctx, stop, err := e.start(ctx, env)
	defer stop()
	if err != nil {
		return apiv1.Verdict{}, err
	}
	result, err := e.Goober.Review(ctx, env)
	return result, errors.Join(err, context.Cause(ctx))
}
