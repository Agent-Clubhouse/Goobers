package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// abortedRunLabel marks a PR whose originating implementation run was
// cancelled/aborted after the PR was already opened (#2238). Cancelling a
// run stops only the run itself — on its own that leaves the PR open,
// unlabeled, and fully eligible for a later INDEPENDENT merge-review run to
// select, approve, and auto-merge it against acceptance criteria snapshotted
// into its body before an operator corrected the source issue. This label is
// the durable, cross-run block: pr-select excludes it (prselect.go, the same
// mechanism as noMergeReviewLabel) and merge-pr independently refuses to
// merge a PR carrying it (mergepr.go), even with a green verdict and passing
// CI — defense in depth so a bypass of selection can't bypass the block too.
//
// Deliberately NOT self-healing (unlike goobers:merge-demoted, #950): an
// operator cancelling a run is a deliberate decision, not a transient
// condition a later commit should silently clear. Only a human removing the
// label re-enables the PR for auto-merge.
const abortedRunLabel = "goobers:run-aborted"

// runAbortLabelOperation marks the ref.touched event this file appends once
// it has labeled a PR for a given run, so a repeated terminal-preparer call
// (e.g. a retried finalize) never re-issues the label mutation.
const runAbortLabelOperation = "label-run-aborted"

// prLabelFunc adds labels to a PR, modeled as a work item — the same shape
// every other label mutation in this codebase uses (UpdateWorkItem with a PR
// number as ID; see remediationcheckpoint.go / mergedemotion.go).
type prLabelFunc func(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error)

// workItemUpdater is the narrow seam buildTerminalRunAbortLabeler needs —
// just enough of providers.BacklogProvider to swap in a fake in tests,
// mirroring newTerminalBranchDeleter's providers.BranchDeleter seam.
type workItemUpdater interface {
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
}

var newRunAbortLabelProvider = func(source providers.TokenSource) workItemUpdater {
	return providers.NewGitHubProvider("", providers.WithTokenSource(source))
}

// buildTerminalRunAbortLabeler mirrors buildTerminalBranchDelete's shape: the
// same credential/capability wiring — including the per-gaggle project scoping
// (#2692 sibling), since the PR being labeled lives in the run gaggle's own
// repo — but github:pr:write (already used for PR open/poll/close) rather than
// github:branch:delete, since labeling a PR is a PR-write operation.
func buildTerminalRunAbortLabeler(cfg *instance.Config, project apiv1.RepoRef, registrar terminalSecretRegistry, stores credentials.StoreResolver) (prLabelFunc, error) {
	if len(cfg.Repos) == 0 {
		return nil, nil
	}
	gaggleOwner := project.Owner
	if project.Provider == apiv1.ProviderADO && project.Project != "" {
		gaggleOwner += "/" + project.Project
	}
	resolver, grants, err := buildCredentials(cfg, stores, gaggleOwner, project.Name, nil, registrar)
	if err != nil {
		return nil, err
	}
	injector, err := credentials.NewInjector(resolver, grants, registrar)
	if err != nil {
		return nil, fmt.Errorf("build run-abort label credentials: %w", err)
	}
	label := func(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
		set, err := injector.Materialize(ctx, []string{string(capability.GitHubPRWrite)})
		if err != nil {
			return providers.WorkItem{}, scrubTerminalError(registrar, err)
		}
		result, err := newRunAbortLabelProvider(set.For(string(capability.GitHubPRWrite))).UpdateWorkItem(ctx, req)
		return result, scrubTerminalError(registrar, err)
	}
	return label, nil
}

// labelAbortedRunPR stamps abortedRunLabel on the PR this run opened, if any,
// when the run's terminal phase is aborted (#2238). Called on every terminal
// run (buildTerminalBranchPreparer's shared entrypoint), so it returns before
// any journal I/O for the overwhelmingly common non-aborted case. Locates the
// PR via the ExternalRef{Kind:"pr"} the runner's own mutation-sidecar replay
// journals for a successful open-pr stage (finishTaskDispatch in
// internal/runner/run.go) — the same signal finalizeTerminalBranch reads to
// detect "a PR was opened" — rather than re-deriving it from stage outputs.
// Scans the FULL run history (not just the current resumed segment, unlike
// finalizeTerminalBranch's branch-cleanup scan) because a PR opened in an
// earlier segment must still be labeled if this run ultimately aborts.
func labelAbortedRunPR(runsDir, runID string, phase journal.RunPhase, jr *journal.Run, repo providers.RepositoryRef, labelPR prLabelFunc) error {
	if phase != journal.PhaseAborted || labelPR == nil {
		return nil
	}
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		return fmt.Errorf("open terminal run journal for run-abort label: %w", err)
	}
	events, err := rd.Events()
	if err != nil {
		return fmt.Errorf("read terminal run events for run-abort label: %w", err)
	}

	var pr *journal.ExternalRef
	var alreadyLabeled bool
	for i := range events {
		ev := events[i]
		if ev.ExternalRef == nil || ev.ExternalRef.Kind != "pr" {
			continue
		}
		if pr == nil || (pr.ID == "" && ev.ExternalRef.ID != "") {
			ref := *ev.ExternalRef
			pr = &ref
		}
		if ev.Runner["operation"] == runAbortLabelOperation {
			alreadyLabeled = true
		}
	}
	if pr == nil || pr.ID == "" || alreadyLabeled {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, labelErr := labelPR(ctx, providers.UpdateWorkItemRequest{
		Repository: repo, ID: pr.ID, AddLabels: []string{abortedRunLabel},
	})
	return appendRunAbortLabelResult(jr, pr, labelErr)
}

func appendRunAbortLabelResult(jr *journal.Run, pr *journal.ExternalRef, labelErr error) error {
	ev := journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: pr,
		Runner:      map[string]any{"operation": runAbortLabelOperation},
	}
	if labelErr != nil {
		ev.Error = &journal.ErrorDetail{Code: "run_abort_label_failed", Message: labelErr.Error()}
	}
	if err := jr.Append(ev); err != nil {
		return fmt.Errorf("journal run-abort label: %w", err)
	}
	return labelErr
}
