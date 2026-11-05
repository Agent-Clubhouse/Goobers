package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/controlplane/scheduling"
	"github.com/goobers/goobers/internal/instance"
)

type workflowSourceApplier = scheduling.WorkflowSourceApplier
type sourceReloader struct{ *configReloader }

func (r sourceReloader) PollSourceOnce(now time.Time) (bool, string, string, string, error) {
	return r.pollSourceOnce(now)
}
func (r sourceReloader) RefreshRestoredSource(now time.Time) { r.refreshRestoredSource(now) }
func newWorkflowSourceApplier(root string, source instance.WorkflowSource, tokens instance.GitTokenSource, setup *schedulerSetup, reloader *configReloader) *workflowSourceApplier {
	return &workflowSourceApplier{Source: source, Reloader: sourceReloader{reloader}, Prepare: func(ctx context.Context, current string) (string, bool, *instance.PreparedConfigSwap, error) {
		revision, changed, _, swap, err := instance.PrepareGitWorkflowSourceIfChanged(ctx, root, source, current, tokens, setup.SharedRegistry, setup.SecretStores)
		return revision, changed, swap, err
	}}
}
