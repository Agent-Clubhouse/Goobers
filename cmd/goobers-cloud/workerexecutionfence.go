package main

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimewiring"
)

// localSharedExecutionFence admits root-only workers only for local claims.
// Shared claims require the daemon's authenticated, continuously observed fence.
func localSharedExecutionFence(layout instance.Layout) runtimewiring.ExecutionFenceStart {
	return func(ctx context.Context, _ apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
		stop := func() {}
		set, report, err := instance.LoadConfigDir(layout.ConfigDir())
		if err != nil {
			return ctx, stop, fmt.Errorf("verify worker claim configuration: %w (%s)", err, validationIssueSummary(report))
		}
		for _, workflow := range set.Workflows {
			if mode := workflow.Spec.Readiness.ClaimVisibility; mode != "" && mode != "local" {
				return ctx, stop, fmt.Errorf("shared worker execution requires a daemon ownership observer")
			}
		}
		return ctx, stop, nil
	}
}
