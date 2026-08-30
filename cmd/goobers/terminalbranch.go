package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

const (
	branchCleanupOperation = "delete"
	branchCleanupSucceeded = "succeeded"
	branchCleanupSkipped   = "unnecessary"
	branchCleanupFailed    = "failed"
)

type deleteBranchFunc func(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error)

type terminalSecretRegistry interface {
	credentials.SecretRegistrar
	journal.Scrubber
}

var newTerminalBranchDeleter = func(source providers.TokenSource) providers.BranchDeleter {
	return providers.NewGitHubProvider("", providers.WithTokenSource(source))
}

// newGiteaTerminalBranchDeleter is the Gitea arm of the branch-delete seam.
var newGiteaTerminalBranchDeleter = func(baseURL string, source providers.TokenSource) providers.BranchDeleter {
	return providers.NewGiteaProvider(baseURL, "", providers.WithGiteaTokenSource(source))
}

func newTerminalBranchDeleteProviderForProject(cfg *instance.Config, project apiv1.RepoRef, source providers.TokenSource) (providers.BranchDeleter, error) {
	repo := terminalRepositoryRefForProject(cfg, project)
	switch repo.Provider {
	case providers.ProviderGitea:
		baseURL, err := terminalGiteaBaseURLForProject(cfg, project)
		if err != nil {
			return nil, err
		}
		return newGiteaTerminalBranchDeleter(baseURL, source), nil
	case providers.ProviderGitHub:
		return newTerminalBranchDeleter(source), nil
	default:
		return nil, fmt.Errorf("terminal branch cleanup does not support repository provider %q", repo.Provider)
	}
}

// terminalAnnotator is the sink a terminal cleanup step records its outcome
// to. Both *journal.Run and *journal.InstanceLog satisfy it.
//
// The interface exists because decision 005 D1 runs the SAME terminal
// preparation for engine-driven runs, and an engine run's journal is already
// closed by the time the daemon sees the workflow's result: the workflow
// wrote its own run.finished through the live journal plane. Appending a
// normative ref.touched after that would not merely be untidy — DiffLiveJournal
// compares the live journal's conformance view against the projected one
// ELEMENT BY ELEMENT AND BY LENGTH (internal/engine/verify.go), so the extra
// event would be filed as a live_journal_divergence on every engine run that
// cleaned up a branch. The cleanup still has to happen and still has to be
// recorded; for an engine run it is recorded in the INSTANCE log, which is the
// daemon's own record of what the daemon did, and is exactly where a
// daemon-side side effect on a run it does not own belongs.
type terminalAnnotator interface {
	Append(journal.Event) error
}

// terminalBranchPreparer is buildTerminalBranchPreparer's driver-neutral
// shape: runner.TerminalPreparer with the concrete *journal.Run widened to
// the sink interface above.
type terminalBranchPreparer func(runID string, phase journal.RunPhase, annotate terminalAnnotator) error

// runnerPreparer adapts a terminalBranchPreparer to the runner's hook type.
// The runner always passes the live run journal, which is correct for a run
// it is itself driving: the run.finished append has not happened yet, so the
// cleanup record lands inside the run's own history exactly as before.
func (p terminalBranchPreparer) runnerPreparer() runner.TerminalPreparer {
	return func(runID string, phase journal.RunPhase, jr *journal.Run) error {
		return p(runID, phase, jr)
	}
}

func buildTerminalBranchPreparer(l instance.Layout, cfg *instance.Config, project apiv1.RepoRef, registrar terminalSecretRegistry, stores credentials.StoreResolver) (terminalBranchPreparer, error) {
	// An instance with no configured repo (the credential-free demo, #587)
	// never touches a branch by design — every one of its runs is
	// legitimately branch-less, not an anomaly finalizeTerminalBranch's
	// "branch-reference-missing" cleanup record exists to flag. Skip
	// branch cleanup entirely rather than journal a spurious ref.touched
	// for every single run.
	if len(cfg.Repos) == 0 {
		return func(string, journal.RunPhase, terminalAnnotator) error { return nil }, nil
	}
	deleteBranch, repo, err := buildTerminalBranchDelete(cfg, project, registrar, stores)
	if err != nil {
		return nil, err
	}
	labelAbortedPR, err := buildTerminalRunAbortLabeler(cfg, project, registrar, stores)
	if err != nil {
		return nil, err
	}
	return func(runID string, phase journal.RunPhase, annotate terminalAnnotator) error {
		branchErr := finalizeTerminalBranch(l.RunsDir(), runID, annotate, repo, deleteBranch)
		labelErr := labelAbortedRunPR(l.RunsDir(), runID, phase, annotate, repo, labelAbortedPR)
		return errors.Join(branchErr, labelErr)
	}, nil
}

// terminalGaggleProject resolves the declared project repo of the gaggle that
// owns l's runs tree, so terminal branch cleanup targets the repo the run's
// branch was actually pushed to (#2692 sibling) — outside the daemon's
// per-gaggle runner wiring (one-shot abort, stalled-run sweep) the gaggle's
// project is not otherwise in hand. A layout outside any gaggle scope, or a
// gaggle no longer configured, resolves to the zero RepoRef — the first-repo
// fallback buildTerminalBranchDelete documents.
func terminalGaggleProject(l instance.Layout) (apiv1.RepoRef, error) {
	if l.Gaggle() == "" {
		return apiv1.RepoRef{}, nil
	}
	set, report, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		// This helper runs outside any CLI surface (one-shot abort, stalled-run
		// sweep), so the report's errors travel inside the returned error
		// rather than to a stderr this scope does not own.
		if summary := validationIssueSummary(report); summary != "" {
			return apiv1.RepoRef{}, fmt.Errorf("resolve gaggle %q project for terminal branch cleanup: %w (%s)", l.Gaggle(), err, summary)
		}
		return apiv1.RepoRef{}, fmt.Errorf("resolve gaggle %q project for terminal branch cleanup: %w", l.Gaggle(), err)
	}
	if gaggle := configuredGaggle(set, l.Gaggle()); gaggle != nil {
		return gaggle.Spec.Project, nil
	}
	return apiv1.RepoRef{}, nil
}

func prepareAbortedRunBranch(l instance.Layout, runID string, jr *journal.Run, registrar terminalSecretRegistry) error {
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return fmt.Errorf("load terminal branch cleanup config: %w", err)
	}
	// One-shot command scope: this is its own composition root, so it builds
	// its own store registry (#683) rather than threading a daemon's.
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return fmt.Errorf("build terminal branch cleanup secret store registry: %w", err)
	}
	var project apiv1.RepoRef
	if len(cfg.Repos) > 0 {
		if project, err = terminalGaggleProject(l); err != nil {
			return err
		}
	}
	prepare, err := buildTerminalBranchPreparer(l, cfg, project, registrar, stores)
	if err != nil {
		return err
	}
	return prepare(runID, journal.PhaseAborted, jr)
}

func buildTerminalBranchDelete(cfg *instance.Config, project apiv1.RepoRef, registrar terminalSecretRegistry, stores credentials.StoreResolver) (deleteBranchFunc, providers.RepositoryRef, error) {
	if len(cfg.Repos) == 0 {
		return nil, providers.RepositoryRef{}, nil
	}
	// Per-gaggle scoping (#2692 sibling): the branch a run pushed lives in its
	// gaggle's own project repo, so the delete must target that repo with that
	// repo's own token — RunnerGrants matches the owner/name binding exactly as
	// the run's push credentials were scoped. A zero project (single-gaggle /
	// legacy instance) keeps the first repo and its first-binding token.
	gaggleOwner := project.Owner
	if project.Provider == apiv1.ProviderADO && project.Project != "" {
		gaggleOwner += "/" + project.Project
	}
	resolver, grants, err := buildCredentials(cfg, stores, gaggleOwner, project.Name, nil, registrar)
	if err != nil {
		return nil, providers.RepositoryRef{}, err
	}
	injector, err := credentials.NewInjector(resolver, grants, registrar)
	if err != nil {
		return nil, providers.RepositoryRef{}, fmt.Errorf("build terminal branch-delete credentials: %w", err)
	}
	repo := terminalRepositoryRefForProject(cfg, project)
	deleteBranch := func(ctx context.Context, req providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
		set, err := injector.Materialize(ctx, []string{string(capability.GitHubBranchDelete)})
		if err != nil {
			return providers.DeleteBranchResult{}, scrubTerminalError(registrar, err)
		}
		deleter, err := newTerminalBranchDeleteProviderForProject(cfg, project, set.For(string(capability.GitHubBranchDelete)))
		if err != nil {
			return providers.DeleteBranchResult{}, scrubTerminalError(registrar, err)
		}
		result, err := deleter.DeleteBranch(ctx, req)
		return result, scrubTerminalError(registrar, err)
	}
	return deleteBranch, repo, nil
}

func scrubTerminalError(scrubber journal.Scrubber, err error) error {
	if err == nil {
		return nil
	}
	return errors.New(string(scrubber.Scrub([]byte(err.Error()))))
}

func finalizeTerminalBranch(runsDir, runID string, annotate terminalAnnotator, repo providers.RepositoryRef, deleteBranch deleteBranchFunc) error {
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		return fmt.Errorf("open terminal run journal: %w", err)
	}
	events, err := rd.Events()
	if err != nil {
		return fmt.Errorf("read terminal run events: %w", err)
	}

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
			if ev.Runner["operation"] == branchCleanupOperation {
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
		return appendBranchCleanup(annotate, &journal.ExternalRef{
			Provider: string(repo.Provider),
			Kind:     "branch",
		}, branchCleanupSkipped, "branch-reference-missing", nil)
	}
	if !pushed {
		return appendBranchCleanup(annotate, branch, branchCleanupSkipped, "branch-not-pushed", nil)
	}
	if openedPR {
		return appendBranchCleanup(annotate, branch, branchCleanupSkipped, "pull-request-opened", nil)
	}
	if lastGateOutcome == gate.OutcomeInfra {
		return appendBranchCleanup(annotate, branch, branchCleanupSkipped, "remediable-validation-failure", nil)
	}
	if deleteBranch == nil {
		return appendBranchCleanup(annotate, branch, branchCleanupFailed, "", errors.New("branch-delete provider is not configured"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, deleteErr := deleteBranch(ctx, providers.DeleteBranchRequest{Repository: repo, Name: branch.ID})
	switch {
	case deleteErr != nil:
		return appendBranchCleanup(annotate, branch, branchCleanupFailed, "", deleteErr)
	case !result.Deleted:
		return appendBranchCleanup(annotate, branch, branchCleanupSkipped, "branch-not-found", nil)
	default:
		return appendBranchCleanup(annotate, branch, branchCleanupSucceeded, "", nil)
	}
}

func appendBranchCleanup(annotate terminalAnnotator, branch *journal.ExternalRef, outcome, reason string, cleanupErr error) error {
	runnerFields := map[string]any{
		"operation": branchCleanupOperation,
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
		ev.Error = &journal.ErrorDetail{Code: "branch_delete_failed", Message: cleanupErr.Error()}
	}
	if err := annotate.Append(ev); err != nil {
		return fmt.Errorf("journal terminal branch cleanup: %w", err)
	}
	return nil
}
