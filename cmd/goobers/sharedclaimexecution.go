package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

func localSharedExecutionFence(layout instance.Layout) runtimewiring.ExecutionFenceStart {
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
