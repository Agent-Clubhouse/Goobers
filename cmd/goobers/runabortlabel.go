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

// prOpenOperation is the runner.operation value the mutation-sidecar replay
// stamps on the ref.touched event for a PR this run itself OPENED
// (finishTaskDispatch in internal/runner/run.go). It is the ONLY operation
// that makes a PR ref "this run's PR" for run-abort labeling purposes.
//
// Other operations put a kind="pr" ref in a run's history without the run
// having created it — most importantly merge-review's apply-verdict stage,
// which journals operation="label" against somebody ELSE's PR. Treating that
// as ownership was #2238's scope bug: aborting a needs-changes merge-review
// run stamped the permanent, non-self-healing abortedRunLabel on the PR under
// review, blocking pr-select and merge-pr forever and overriding the very
// remediation loop the needs-changes verdict had just requested. Only the
// originating implementation run (which carries operation="open") may label.
const prOpenOperation = "open"

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

// newGiteaRunAbortLabelProvider is the Gitea arm of the same seam. It takes the
// resolved forge base URL because, unlike GitHub, a self-hosted Gitea has no
// well-known API host. Token resolution stays on the per-request TokenSource
// seam so the secret continues to flow through the run's registrar-based
// scrubbing rather than being copied into a second unregistered string.
var newGiteaRunAbortLabelProvider = func(baseURL string, source providers.TokenSource) workItemUpdater {
	return providers.NewGiteaProvider(baseURL, "", providers.WithGiteaTokenSource(source))
}

// newTerminalRunAbortLabelProviderForProject dispatches on the terminal
// repository's own declared provider kind. ADO is not reachable here: the
// label is a PR-write on the repo the terminal run acted on, and an unsupported
// kind must fail loudly rather than fall through to a GitHub call against a
// non-GitHub forge.
func newTerminalRunAbortLabelProviderForProject(cfg *instance.Config, project apiv1.RepoRef, source providers.TokenSource) (workItemUpdater, error) {
	repo := terminalRepositoryRefForProject(cfg, project)
	switch repo.Provider {
	case providers.ProviderGitea:
		baseURL, err := terminalGiteaBaseURLForProject(cfg, project)
		if err != nil {
			return nil, err
		}
		return newGiteaRunAbortLabelProvider(baseURL, source), nil
	case providers.ProviderGitHub:
		return newRunAbortLabelProvider(source), nil
	default:
		return nil, fmt.Errorf("run-abort labeling does not support repository provider %q", repo.Provider)
	}
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
		provider, err := newTerminalRunAbortLabelProviderForProject(cfg, project, set.For(string(capability.GitHubPRWrite)))
		if err != nil {
			return providers.WorkItem{}, scrubTerminalError(registrar, err)
		}
		result, err := provider.UpdateWorkItem(ctx, req)
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
// The ref must carry runner.operation==prOpenOperation: a bare kind=="pr"
// match also catches PRs this run merely touched (merge-review's
// operation="label"), which are not ours to label.
// Scans the FULL run history (not just the current resumed segment, unlike
// finalizeTerminalBranch's branch-cleanup scan) because a PR opened in an
// earlier segment must still be labeled if this run ultimately aborts.
func labelAbortedRunPR(runsDir, runID string, phase journal.RunPhase, annotate terminalAnnotator, repo providers.RepositoryRef, labelPR prLabelFunc) error {
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
		// Idempotency is keyed on the label event alone and stays
		// outside the ownership filter: once we have labeled, a
		// retried finalize must not re-issue the mutation.
		if ev.Runner["operation"] == runAbortLabelOperation {
			alreadyLabeled = true
			continue
		}
		if ev.Runner["operation"] != prOpenOperation {
			continue
		}
		if pr == nil || (pr.ID == "" && ev.ExternalRef.ID != "") {
			ref := *ev.ExternalRef
			pr = &ref
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
	return appendRunAbortLabelResult(annotate, pr, labelErr)
}

func appendRunAbortLabelResult(annotate terminalAnnotator, pr *journal.ExternalRef, labelErr error) error {
	ev := journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: pr,
		Runner:      map[string]any{"operation": runAbortLabelOperation},
	}
	if labelErr != nil {
		ev.Error = &journal.ErrorDetail{Code: "run_abort_label_failed", Message: labelErr.Error()}
	}
	if err := annotate.Append(ev); err != nil {
		return fmt.Errorf("journal run-abort label: %w", err)
	}
	return labelErr
}
