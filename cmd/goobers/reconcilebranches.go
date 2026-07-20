package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const (
	branchReconcilePrefix       = "goobers/"
	defaultBranchReconcileBatch = 25
	maxBranchReconcileBatch     = 100
	defaultBranchReconcileAge   = 7 * 24 * time.Hour
	branchReconcileOperation    = "branch-reconcile"
)

type branchReconcileOptions struct {
	Repository providers.RepositoryRef
	RunsDir    string
	Prefix     string
	After      string
	Limit      int
	MinimumAge time.Duration
	Delete     bool
	Now        func() time.Time
}

type branchReconcileReport struct {
	Scanned    int
	Candidates int
	Deleted    int
	Preserved  int
	Ambiguous  int
	Failures   int
	NextAfter  string
}

type branchReconcileOwner struct {
	Workflow   string
	RunID      string
	StartedAt  time.Time
	TerminalAt time.Time
	Phase      journal.RunPhase
}

type branchReconcileProviderError struct {
	err error
}

func (e *branchReconcileProviderError) Error() string { return e.err.Error() }
func (e *branchReconcileProviderError) Unwrap() error { return e.err }

func runReconcileBranches(args []string, stdout, stderr io.Writer) int {
	deleteDefault, err := strconv.ParseBool(providerInput("deleteBranches", "false"))
	if err != nil {
		pf(stderr, "error: invalid deleteBranches input: %v\n", err)
		return 1
	}
	limitDefault, err := strconv.Atoi(providerInput("maxBranches", strconv.Itoa(defaultBranchReconcileBatch)))
	if err != nil {
		pf(stderr, "error: invalid maxBranches input: %v\n", err)
		return 1
	}
	ageDefault, err := time.ParseDuration(providerInput("minimumAge", defaultBranchReconcileAge.String()))
	if err != nil {
		pf(stderr, "error: invalid minimumAge input: %v\n", err)
		return 1
	}

	fs := flag.NewFlagSet("reconcile-branches", flag.ContinueOnError)
	fs.SetOutput(stderr)
	deleteBranches := fs.Bool("delete", deleteDefault, "delete eligible branches (opt-in; default is dry-run)")
	limit := fs.Int("max", limitDefault, "maximum candidates inspected in one sweep (1-100)")
	minimumAge := fs.Duration("min-age", ageDefault, "minimum terminal run age required for deletion")
	after := fs.String("after", providerInput("after", ""), "resume after this branch name in lexical order")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers reconcile-branches [--delete] [--max N] [--min-age D] [--after BRANCH] [path]\n\n"+
			"Inspect a bounded page of remote goobers/* branches. The default is a\n"+
			"dry-run: no branch is deleted. --delete opts into deletion only for a\n"+
			"branch whose local run journal proves ownership, has been terminal for\n"+
			"at least 168h by default, has no remote activity in that window, and has\n"+
			"no open pull request. Every candidate\n"+
			"decision and deletion outcome is appended to scheduler/events.jsonl.\n"+
			"The default batch is 25 and the hard ceiling is 100 candidates. Task inputs deleteBranches,\n"+
			"maxBranches, minimumAge, and after provide the same workflow-stage\n"+
			"configuration surface. Exit codes: 0 = sweep completed, 1 = business or\n"+
			"provider error, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if *limit < 1 || *limit > maxBranchReconcileBatch {
		pf(stderr, "error: max must be between 1 and %d\n", maxBranchReconcileBatch)
		return 1
	}
	if *minimumAge <= 0 {
		pln(stderr, "error: min-age must be positive")
		return 1
	}
	if *after != "" && !strings.HasPrefix(*after, branchReconcilePrefix) {
		pf(stderr, "error: after must start with %q\n", branchReconcilePrefix)
		return 1
	}

	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := providerToken(capability.GitHubBranchDelete)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte(token))
	log, _, err := journal.OpenInstanceLog(layoutFor(root).SchedulerDir(), journal.WithScrubber(scrubber))
	if err != nil {
		pf(stderr, "error: open instance log: %v\n", err)
		return 1
	}
	defer func() { _ = log.Close() }()

	provider := newGitHubProvider(
		token,
		providers.WithMutationRecorder(sidecarMutationRecorder{kind: "branch"}),
		providers.WithMaxRateLimitRetries(0),
	)
	ctx, cancel := providerCommandContext()
	defer cancel()
	report, err := reconcileRemoteBranches(ctx, provider, log, branchReconcileOptions{
		Repository: repo,
		RunsDir:    layoutFor(root).RunsDir(),
		Prefix:     branchReconcilePrefix,
		After:      *after,
		Limit:      *limit,
		MinimumAge: *minimumAge,
		Delete:     *deleteBranches,
		Now:        time.Now,
	})
	if err != nil {
		var providerErr *branchReconcileProviderError
		if errors.As(err, &providerErr) {
			return failProviderStage(stderr, "reconcile remote branches", providerErr.err, "branch-reconcile.json")
		}
		pf(stderr, "error: reconcile remote branches: %v\n", err)
		return 1
	}
	if err := writeBranchReconcileResult(report, !*deleteBranches, *limit, *minimumAge); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	mode := "dry-run"
	if *deleteBranches {
		mode = "delete"
	}
	pf(stdout, "branch reconciliation (%s): scanned %d, candidates %d, deleted %d, preserved %d, failures %d\n",
		mode, report.Scanned, report.Candidates, report.Deleted, report.Preserved, report.Failures)
	return 0
}

func reconcileRemoteBranches(ctx context.Context, provider providers.BranchReconciliationProvider, log *journal.InstanceLog, opts branchReconcileOptions) (branchReconcileReport, error) {
	var report branchReconcileReport
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Prefix == "" {
		opts.Prefix = branchReconcilePrefix
	}
	if opts.Limit < 1 || opts.Limit > maxBranchReconcileBatch {
		return report, fmt.Errorf("branch reconcile limit must be between 1 and %d", maxBranchReconcileBatch)
	}
	if opts.MinimumAge <= 0 {
		return report, fmt.Errorf("branch reconcile minimum age must be positive")
	}
	if log == nil {
		return report, fmt.Errorf("branch reconcile instance journal is required")
	}

	branches, err := provider.ListBranches(ctx, providers.ListBranchesRequest{
		Repository: opts.Repository,
		Prefix:     opts.Prefix,
		After:      opts.After,
		Limit:      opts.Limit,
	})
	if err != nil {
		reason := "provider-lookup-failed"
		if isProviderRateLimit(err) {
			reason = "rate-limited"
		}
		if journalErr := appendBranchReconcileEvent(log, providers.BranchSummary{}, branchReconcileEvent{
			Kind: "sweep", Outcome: "failed", Reason: reason, Err: err,
		}); journalErr != nil {
			return report, errors.Join(err, journalErr)
		}
		return report, &branchReconcileProviderError{err: err}
	}
	if len(branches) > opts.Limit {
		branches = branches[:opts.Limit]
	}

	for _, branch := range branches {
		report.Scanned++
		report.NextAfter = branch.Name

		owner, reason, inspectErr := inspectBranchOwner(opts.RunsDir, opts.Prefix, branch.Name)
		if inspectErr != nil {
			report.Preserved++
			report.Ambiguous++
			if err := appendBranchReconcileEvent(log, branch, branchReconcileEvent{
				Kind: "decision", Outcome: "preserved", Reason: reason, Err: inspectErr,
			}); err != nil {
				return report, err
			}
			continue
		}

		event := branchReconcileEvent{Kind: "decision", Owner: &owner, MinimumAge: opts.MinimumAge}
		switch {
		case !terminalBranchRunPhase(owner.Phase):
			report.Preserved++
			event.Outcome, event.Reason = "preserved", "run-active"
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			continue
		case opts.Now().Sub(owner.TerminalAt) < opts.MinimumAge:
			report.Preserved++
			event.Outcome, event.Reason = "preserved", "safety-window"
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			continue
		}

		pr, open, lookupErr := provider.FindPullRequestByBranch(ctx, opts.Repository, branch.Name, "")
		if lookupErr != nil {
			report.Preserved++
			report.Failures++
			event.Outcome, event.Reason, event.Err = "preserved", "provider-lookup-failed", lookupErr
			if isProviderRateLimit(lookupErr) {
				event.Reason = "rate-limited"
			}
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			if isProviderRateLimit(lookupErr) {
				return report, &branchReconcileProviderError{err: lookupErr}
			}
			continue
		}
		if open {
			report.Preserved++
			event.Outcome, event.Reason, event.PullRequest = "preserved", "open-pull-request", pr.ID
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			continue
		}

		if branch.SHA == "" {
			report.Preserved++
			report.Failures++
			event.Outcome, event.Reason, event.Err = "preserved", "branch-tip-unavailable",
				fmt.Errorf("listed branch %q has no tip SHA", branch.Name)
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			continue
		}
		current, found, lookupErr := provider.GetBranch(ctx, opts.Repository, branch.Name)
		if lookupErr != nil {
			report.Preserved++
			report.Failures++
			event.Outcome, event.Reason, event.Err = "preserved", "provider-lookup-failed", lookupErr
			if isProviderRateLimit(lookupErr) {
				event.Reason = "rate-limited"
			}
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			if isProviderRateLimit(lookupErr) {
				return report, &branchReconcileProviderError{err: lookupErr}
			}
			continue
		}
		if !found {
			report.Preserved++
			event.Outcome, event.Reason = "preserved", "branch-not-found"
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			continue
		}
		if current.SHA == "" {
			report.Preserved++
			report.Failures++
			event.Outcome, event.Reason, event.Err = "preserved", "branch-tip-unavailable",
				fmt.Errorf("re-read branch %q has no tip SHA", branch.Name)
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			continue
		}
		if current.SHA != branch.SHA {
			report.Preserved++
			event.Outcome, event.Reason, event.ObservedSHA = "preserved", "branch-tip-changed", current.SHA
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			continue
		}
		if current.LastActivityAt == nil || current.LastActivityAt.IsZero() {
			report.Preserved++
			report.Failures++
			event.Outcome, event.Reason, event.Err = "preserved", "branch-activity-unavailable",
				fmt.Errorf("branch %q has no remote activity timestamp", branch.Name)
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			continue
		}
		event.LastActivityAt = *current.LastActivityAt
		if opts.Now().Sub(*current.LastActivityAt) < opts.MinimumAge {
			report.Preserved++
			event.Outcome, event.Reason = "preserved", "branch-activity-recent"
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			continue
		}

		report.Candidates++
		if !opts.Delete {
			report.Preserved++
			event.Outcome, event.Reason = "candidate", "dry-run"
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			continue
		}

		pr, open, lookupErr = provider.FindPullRequestByBranch(ctx, opts.Repository, branch.Name, "")
		if lookupErr != nil {
			report.Preserved++
			report.Failures++
			event.Outcome, event.Reason, event.Err = "preserved", "provider-lookup-failed", lookupErr
			if isProviderRateLimit(lookupErr) {
				event.Reason = "rate-limited"
			}
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			if isProviderRateLimit(lookupErr) {
				return report, &branchReconcileProviderError{err: lookupErr}
			}
			continue
		}
		if open {
			report.Preserved++
			event.Outcome, event.Reason, event.PullRequest = "preserved", "open-pull-request-before-delete", pr.ID
			if err := appendBranchReconcileEvent(log, branch, event); err != nil {
				return report, err
			}
			continue
		}

		event.Outcome, event.Reason = "delete-approved", ""
		if err := appendBranchReconcileEvent(log, branch, event); err != nil {
			return report, err
		}

		result, deleteErr := provider.DeleteBranch(ctx, providers.DeleteBranchRequest{Repository: opts.Repository, Name: branch.Name})
		mutation := branchReconcileEvent{Kind: "mutation", Owner: &owner}
		switch {
		case deleteErr != nil:
			report.Preserved++
			report.Failures++
			mutation.Outcome, mutation.Reason, mutation.Err = "failed", "delete-failed", deleteErr
			if isProviderRateLimit(deleteErr) {
				mutation.Reason = "rate-limited"
			}
		case !result.Deleted:
			report.Preserved++
			mutation.Outcome, mutation.Reason = "preserved", "branch-not-found"
		default:
			report.Deleted++
			mutation.Outcome = "deleted"
		}
		if err := appendBranchReconcileEvent(log, branch, mutation); err != nil {
			return report, err
		}
		if isProviderRateLimit(deleteErr) {
			return report, &branchReconcileProviderError{err: deleteErr}
		}
	}
	return report, nil
}

func inspectBranchOwner(runsDir, prefix, branch string) (branchReconcileOwner, string, error) {
	relative, ok := strings.CutPrefix(branch, prefix)
	if !ok {
		return branchReconcileOwner{}, "ambiguous-ownership", fmt.Errorf("branch %q is outside prefix %q", branch, prefix)
	}
	parts := strings.Split(relative, "/")
	if len(parts) != 2 || parts[0] == "" || !apiv1.ValidRunID(parts[1]) {
		return branchReconcileOwner{}, "ambiguous-ownership", fmt.Errorf("branch %q does not match %s<workflow>/<run-id>", branch, prefix)
	}
	workflow, runID := parts[0], parts[1]
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		return branchReconcileOwner{}, "ambiguous-ownership", fmt.Errorf("open owning run %s: %w", runID, err)
	}
	id, err := rd.Identity()
	if err != nil {
		return branchReconcileOwner{}, "ambiguous-ownership", fmt.Errorf("read owning run %s identity: %w", runID, err)
	}
	if id.RunID != runID || id.Workflow != workflow || id.StartedAt.IsZero() {
		return branchReconcileOwner{}, "ambiguous-ownership", fmt.Errorf("branch %q does not match owning run identity", branch)
	}
	events, err := rd.Events()
	if err != nil {
		return branchReconcileOwner{}, "run-journal-unreadable", fmt.Errorf("read owning run %s events: %w", runID, err)
	}
	owned := false
	var terminalAt time.Time
	for _, event := range events {
		if event.Type == journal.EventRefTouched && event.ExternalRef != nil &&
			event.ExternalRef.Provider == string(providers.ProviderGitHub) &&
			event.ExternalRef.Kind == "branch" && event.ExternalRef.ID == branch {
			owned = true
		}
		if event.Type == journal.EventRunFinished {
			terminalAt = event.Time
		}
	}
	if !owned {
		return branchReconcileOwner{}, "ambiguous-ownership", fmt.Errorf("owning run %s has no journaled reference to branch %q", runID, branch)
	}
	phase, err := rd.Phase()
	if err != nil {
		return branchReconcileOwner{}, "run-journal-unreadable", fmt.Errorf("read owning run %s phase: %w", runID, err)
	}
	if terminalBranchRunPhase(phase) && terminalAt.IsZero() {
		return branchReconcileOwner{}, "run-journal-unreadable", fmt.Errorf("owning run %s has no timestamped terminal event", runID)
	}
	return branchReconcileOwner{
		Workflow: workflow, RunID: runID, StartedAt: id.StartedAt, TerminalAt: terminalAt, Phase: phase,
	}, "", nil
}

func terminalBranchRunPhase(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
		return true
	default:
		return false
	}
}

type branchReconcileEvent struct {
	Kind           string
	Outcome        string
	Reason         string
	Owner          *branchReconcileOwner
	MinimumAge     time.Duration
	PullRequest    string
	ObservedSHA    string
	LastActivityAt time.Time
	Err            error
}

func appendBranchReconcileEvent(log *journal.InstanceLog, branch providers.BranchSummary, detail branchReconcileEvent) error {
	fields := map[string]any{
		"operation": branchReconcileOperation,
		"event":     detail.Kind,
		"outcome":   detail.Outcome,
	}
	if branch.Name != "" {
		fields["branch"] = branch.Name
	}
	if branch.SHA != "" {
		fields["sha"] = branch.SHA
	}
	if detail.Reason != "" {
		fields["reason"] = detail.Reason
	}
	if detail.Owner != nil {
		fields["workflow"] = detail.Owner.Workflow
		fields["runID"] = detail.Owner.RunID
		fields["runPhase"] = string(detail.Owner.Phase)
		fields["startedAt"] = detail.Owner.StartedAt.UTC().Format(time.RFC3339)
		if !detail.Owner.TerminalAt.IsZero() {
			fields["terminalAt"] = detail.Owner.TerminalAt.UTC().Format(time.RFC3339)
		}
	}
	if detail.MinimumAge > 0 {
		fields["minimumAge"] = detail.MinimumAge.String()
	}
	if detail.PullRequest != "" {
		fields["pullRequest"] = detail.PullRequest
	}
	if detail.ObservedSHA != "" {
		fields["observedSHA"] = detail.ObservedSHA
	}
	if !detail.LastActivityAt.IsZero() {
		fields["lastActivityAt"] = detail.LastActivityAt.UTC().Format(time.RFC3339)
	}
	event := journal.Event{Type: journal.EventRunnerAnnotation, Runner: fields}
	if detail.Err != nil {
		code := "branch_reconcile_failed"
		if isProviderRateLimit(detail.Err) {
			code = providers.ErrorCodeRateLimited
		} else {
			switch detail.Reason {
			case "ambiguous-ownership":
				code = "branch_ownership_ambiguous"
			case "run-journal-unreadable":
				code = "branch_run_journal_unreadable"
			case "provider-lookup-failed":
				code = "branch_provider_lookup_failed"
			case "branch-tip-unavailable":
				code = "branch_tip_unavailable"
			case "branch-activity-unavailable":
				code = "branch_activity_unavailable"
			case "delete-failed":
				code = "branch_delete_failed"
			}
		}
		event.Error = &journal.ErrorDetail{Code: code, Message: detail.Err.Error()}
	}
	if err := log.Append(event); err != nil {
		return fmt.Errorf("journal branch reconciliation %s for %q: %w", detail.Kind, branch.Name, err)
	}
	return nil
}

func isProviderRateLimit(err error) bool {
	var rateLimit *providers.RateLimitError
	return errors.As(err, &rateLimit)
}

func writeBranchReconcileResult(report branchReconcileReport, dryRun bool, limit int, minimumAge time.Duration) error {
	resultFile := providerInput(executor.InputResultFile, "branch-reconcile.json")
	data, err := json.Marshal(map[string]any{
		"scanned":    report.Scanned,
		"candidates": report.Candidates,
		"deleted":    report.Deleted,
		"preserved":  report.Preserved,
		"ambiguous":  report.Ambiguous,
		"failures":   report.Failures,
		"nextAfter":  report.NextAfter,
		"dryRun":     dryRun,
		"batchLimit": limit,
		"minimumAge": minimumAge.String(),
	})
	if err != nil {
		return fmt.Errorf("marshal branch reconciliation result: %w", err)
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", resultFile, err)
	}
	return nil
}
