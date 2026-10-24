// Package prepare is the runner-free terminal "prepare" hook: what a finished
// run does about the branch it pushed and the pull request it opened. A pushed
// branch with no pull request is deleted; a pull request whose run did not
// complete (and did not fail on infrastructure) is stamped with the aborted-run
// label so no lane silently owns it. Both decisions read the run's journal
// events and record their outcome through an Annotator; the provider writes are
// handed in, so the daemon and the cloud controller share one policy.
package prepare

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/lifecycle"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
)

// Branch cleanup operation and outcomes, as journaled.
const (
	OperationDelete  = "delete"
	CleanupSucceeded = "succeeded"
	CleanupSkipped   = "unnecessary"
	CleanupFailed    = "failed"
)

// Annotator is the sink a terminal step records its outcome to. Both
// *journal.Run and *journal.InstanceLog satisfy it. An engine-driven run's
// journal is already closed when the daemon sees its result, so its cleanup is
// recorded in the instance log instead of appended to the closed run.
type Annotator interface {
	Append(journal.Event) error
}

// DeleteFunc deletes a branch.
type DeleteFunc func(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error)

// PRLabelFunc adds labels to a pull request, modeled as a work item.
type PRLabelFunc func(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error)

// AbortedRunLabel marks a PR whose originating implementation run ended
// without merging it (#2238, widened by #3490). Originally applied only to a
// deliberately cancelled/aborted run; a run that instead fails or escalates
// after opening a PR leaves exactly the same orphan behind — the PR is open,
// unlabeled, and its CI may never settle to green (a run terminated mid
// ci-poll, for instance), which makes it invisible to pr-select's normal
// selection (prselect.go requires CheckStatePassing) with no other path back
// into any lane. Stamping this label on non-completed work or policy outcomes
// gives that PR the one disposition every other terminal state already has:
// visibly excluded rather than silently unowned. Infrastructure failures are
// excluded from the marker because the reviewed PR remains valid and must stay
// eligible for a later merge-review run. pr-select excludes marked PRs
// (prselect.go, the same mechanism as
// noMergeReviewLabel) and merge-pr independently refuses to merge a PR
// carrying it (mergepr.go), even with a green verdict and passing CI —
// defense in depth so a bypass of selection can't bypass the block too.
//
// pr-select may clear a stale marker when the live PR is open, mergeable and
// green and either has no prior review attention (#5437), or carries
// provider-issued proof that a later reviewed remediation was published with
// no unresolved feedback or newer abort (#6407). Anything less still requires
// a human removing the label after deciding how to take over the orphan.
const AbortedRunLabel = lifecycle.LabelRunAborted

// PROpenOperation is the runner.operation value the mutation-sidecar replay
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
const PROpenOperation = "open"

// RunAbortLabelOperation marks the ref.touched event this file appends once
// it has labeled a PR for a given run, so a repeated terminal-preparer call
// (e.g. a retried finalize) never re-issues the label mutation.
const RunAbortLabelOperation = "label-run-aborted"

// FinalizeBranch decides the terminal fate of the run's branch from its journal
// events and records it: a pushed branch with no pull request is deleted, every
// other case is journaled as skipped with the reason.
func FinalizeBranch(events []journal.Event, annotate Annotator, repo providers.RepositoryRef, deleteBranch DeleteFunc) error {
	segmentStart := 0
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunResumed {
			segmentStart = i + 1
			break
		}
	}

	var branch *journal.ExternalRef
	var pushed, openedPR, alreadyFinalized, noWork bool
	var lastGateOutcome string
	for i := range events {
		ev := events[i]
		if ev.ExternalRef != nil && ev.ExternalRef.Kind == "branch" {
			if branch == nil || (branch.ID == "" && ev.ExternalRef.ID != "") {
				ref := *ev.ExternalRef
				branch = &ref
			}
		}
		if i < segmentStart {
			continue
		}
		if ev.ExternalRef != nil && ev.ExternalRef.Kind == "branch" {
			if ev.Runner["operation"] == OperationDelete {
				alreadyFinalized = true
			}
		}
		if ev.Type == journal.EventStageFinished && ev.Stage == "push-branch" && ev.Status == string(apiv1.ResultSuccess) {
			pushed = true
		}
		if ev.Type == journal.EventStageFinished && ev.Stage == "open-pr" && ev.Status == string(apiv1.ResultSuccess) {
			openedPR = true
		}
		if ev.Type == journal.EventStageFinished && ev.Status == string(apiv1.ResultNoWork) {
			noWork = true
		}
		if ev.Type == journal.EventGateEvaluated {
			lastGateOutcome = ev.Verdict
		}
		if ev.Type == journal.EventRefTouched && ev.ExternalRef != nil && ev.ExternalRef.Kind == "pr" {
			openedPR = true
		}
	}
	if alreadyFinalized {
		return nil
	}
	// The runner deliberately leaves an empty tick branchless; there is no
	// missing provider ref to diagnose or clean up in that case.
	if branch == nil && noWork {
		return nil
	}
	if branch == nil {
		return AppendBranchCleanup(annotate, &journal.ExternalRef{
			Provider: string(repo.Provider),
			Kind:     "branch",
		}, CleanupSkipped, "branch-reference-missing", nil)
	}
	if !pushed {
		return AppendBranchCleanup(annotate, branch, CleanupSkipped, "branch-not-pushed", nil)
	}
	if openedPR {
		return AppendBranchCleanup(annotate, branch, CleanupSkipped, "pull-request-opened", nil)
	}
	if lastGateOutcome == gate.OutcomeInfra {
		return AppendBranchCleanup(annotate, branch, CleanupSkipped, "remediable-validation-failure", nil)
	}
	if deleteBranch == nil {
		return AppendBranchCleanup(annotate, branch, CleanupFailed, "", errors.New("branch-delete provider is not configured"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, deleteErr := deleteBranch(ctx, providers.DeleteBranchRequest{Repository: repo, Name: branch.ID})
	return RecordBranchDelete(annotate, branch, result, deleteErr)
}

// RecordBranchDelete journals one terminal delete's outcome. A branch
// an open pull request uses (providers.BranchInUseError) is kept, not failed.
func RecordBranchDelete(annotate Annotator, branch *journal.ExternalRef, result providers.DeleteBranchResult, deleteErr error) error {
	var inUse *providers.BranchInUseError
	switch {
	case errors.As(deleteErr, &inUse) && inUse.AsBase:
		return AppendBranchCleanup(annotate, branch, CleanupSkipped, "stacked-pull-request-open", nil)
	case errors.As(deleteErr, &inUse):
		return AppendBranchCleanup(annotate, branch, CleanupSkipped, "pull-request-open", nil)
	case deleteErr != nil:
		return AppendBranchCleanup(annotate, branch, CleanupFailed, "", deleteErr)
	case !result.Deleted:
		return AppendBranchCleanup(annotate, branch, CleanupSkipped, "branch-not-found", nil)
	default:
		return AppendBranchCleanup(annotate, branch, CleanupSucceeded, "", nil)
	}
}

// AppendBranchCleanup journals one branch cleanup outcome.
func AppendBranchCleanup(annotate Annotator, branch *journal.ExternalRef, outcome, reason string, cleanupErr error) error {
	runnerFields := map[string]any{
		"operation": OperationDelete,
		"outcome":   outcome,
	}
	if reason != "" {
		runnerFields["reason"] = reason
	}
	ev := journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: branch,
		Runner:      runnerFields,
	}
	if cleanupErr != nil {
		ev.Error = journal.ErrorDetailFor("branch_delete_failed", cleanupErr)
	}
	if err := annotate.Append(ev); err != nil {
		return fmt.Errorf("journal terminal branch cleanup: %w", err)
	}
	return nil
}

// LabelsPhase reports whether a run ending in phase can orphan a pull request:
// every terminal phase except a completed run.
func LabelsPhase(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseAborted, journal.PhaseFailed, journal.PhaseEscalated:
		return true
	}
	return false
}

// LabelAbortedPR stamps abortedRunLabel on the PR this run opened, if any,
// when the run ends on a non-infrastructure terminal phase other than
// PhaseCompleted (#2238,
// widened by #3490 to cover PhaseFailed and PhaseEscalated alongside the
// original PhaseAborted — a run that fails or escalates after open-pr orphans
// its PR exactly as an abort does). Called on every terminal run
// (the cloud and daemon preparers' shared entrypoint), so it returns before
// any journal I/O for the overwhelmingly common completed case. Locates the
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
func LabelAbortedPR(events []journal.Event, runID string, annotate Annotator, repo providers.RepositoryRef, labelPR PRLabelFunc, reports terminalreport.Outbox, gaggle string) error {
	if labelPR == nil || FailureWasInfrastructure(events) {
		return nil
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
		if ev.Runner["operation"] == RunAbortLabelOperation && ev.Error == nil {
			alreadyLabeled = true
			continue
		}
		if ev.Runner["operation"] != PROpenOperation {
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
	effect := terminalreport.Effect{Kind: terminalreport.AbortLabel, Repository: repo, ItemID: pr.ID, RunID: runID}
	apply := func(ctx context.Context, e terminalreport.Effect) error {
		_, err := labelPR(ctx, providers.UpdateWorkItemRequest{Repository: e.Repository, ID: e.ItemID, AddLabels: []string{AbortedRunLabel}})
		return err
	}
	var labelErr error
	if reports != nil {
		effect.Gaggle = gaggle
		labelErr = reports.Publish(ctx, effect, apply)
	} else {
		labelErr = apply(ctx, effect)
	}
	return AppendRunAbortLabelResult(annotate, pr, labelErr)
}

// FailureWasInfrastructure reports whether the run's terminal was an
// infrastructure failure, which leaves its pull request eligible.
func FailureWasInfrastructure(events []journal.Event) bool {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		switch ev.Type {
		case journal.EventRunResumed, journal.EventGateOverridden, journal.EventStageRerunRequested:
			return false
		}
		if ev.Type == journal.EventRunFinished && ev.TerminalCause != nil {
			return ev.TerminalCause.Classification == journal.TerminalInfrastructureFailure
		}
		if ev.Type != journal.EventError || ev.Error == nil || ev.Error.Code != "run_failed" {
			continue
		}
		class, _ := ev.Runner["errorClass"].(string)
		retryClass, _ := ev.Runner[runsemantics.RetryFailureClassKey].(string)
		return retryClass == string(journal.AttemptInfra) || telemetry.ErrorClass(class).InfraFault()
	}
	return false
}

// AppendRunAbortLabelResult journals the label outcome.
func AppendRunAbortLabelResult(annotate Annotator, pr *journal.ExternalRef, labelErr error) error {
	ev := journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: pr,
		Runner:      map[string]any{"operation": RunAbortLabelOperation},
	}
	if labelErr != nil {
		ev.Error = journal.ErrorDetailFor("run_abort_label_failed", labelErr)
	}
	if err := annotate.Append(ev); err != nil {
		return fmt.Errorf("journal run-abort label: %w", err)
	}
	return labelErr
}
