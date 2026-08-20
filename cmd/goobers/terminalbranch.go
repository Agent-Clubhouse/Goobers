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

func buildTerminalBranchPreparer(l instance.Layout, cfg *instance.Config, project apiv1.RepoRef, registrar terminalSecretRegistry, stores credentials.StoreResolver) (runner.TerminalPreparer, error) {
	// An instance with no configured repo (the credential-free demo, #587)
	// never touches a branch by design — every one of its runs is
	// legitimately branch-less, not an anomaly finalizeTerminalBranch's
	// "branch-reference-missing" cleanup record exists to flag. Skip
	// branch cleanup entirely rather than journal a spurious ref.touched
	// for every single run.
	if len(cfg.Repos) == 0 {
		return func(string, journal.RunPhase, *journal.Run) error { return nil }, nil
	}
	deleteBranch, repo, err := buildTerminalBranchDelete(cfg, project, registrar, stores)
	if err != nil {
		return nil, err
	}
	labelAbortedPR, err := buildTerminalRunAbortLabeler(cfg, project, registrar, stores)
	if err != nil {
		return nil, err
	}
	return func(runID string, phase journal.RunPhase, jr *journal.Run) error {
		branchErr := finalizeTerminalBranch(l.RunsDir(), runID, jr, repo, deleteBranch)
		labelErr := labelAbortedRunPR(l.RunsDir(), runID, phase, jr, repo, labelAbortedPR)
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
	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    cfg.Repos[0].Owner,
		Name:     cfg.Repos[0].Name,
	}
	if project.Owner != "" && project.Name != "" {
		repo.Owner, repo.Name = project.Owner, project.Name
	}
	deleteBranch := func(ctx context.Context, req providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
		set, err := injector.Materialize(ctx, []string{string(capability.GitHubBranchDelete)})
		if err != nil {
			return providers.DeleteBranchResult{}, scrubTerminalError(registrar, err)
		}
		deleter := newTerminalBranchDeleter(set.For(string(capability.GitHubBranchDelete)))
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

func finalizeTerminalBranch(runsDir, runID string, jr *journal.Run, repo providers.RepositoryRef, deleteBranch deleteBranchFunc) error {
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
		return appendBranchCleanup(jr, &journal.ExternalRef{
			Provider: string(repo.Provider),
			Kind:     "branch",
		}, branchCleanupSkipped, "branch-reference-missing", nil)
	}
	if !pushed {
		return appendBranchCleanup(jr, branch, branchCleanupSkipped, "branch-not-pushed", nil)
	}
	if openedPR {
		return appendBranchCleanup(jr, branch, branchCleanupSkipped, "pull-request-opened", nil)
	}
	if lastGateOutcome == gate.OutcomeInfra {
		return appendBranchCleanup(jr, branch, branchCleanupSkipped, "remediable-validation-failure", nil)
	}
	if deleteBranch == nil {
		return appendBranchCleanup(jr, branch, branchCleanupFailed, "", errors.New("branch-delete provider is not configured"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, deleteErr := deleteBranch(ctx, providers.DeleteBranchRequest{Repository: repo, Name: branch.ID})
	switch {
	case deleteErr != nil:
		return appendBranchCleanup(jr, branch, branchCleanupFailed, "", deleteErr)
	case !result.Deleted:
		return appendBranchCleanup(jr, branch, branchCleanupSkipped, "branch-not-found", nil)
	default:
		return appendBranchCleanup(jr, branch, branchCleanupSucceeded, "", nil)
	}
}

func appendBranchCleanup(jr *journal.Run, branch *journal.ExternalRef, outcome, reason string, cleanupErr error) error {
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
	if err := jr.Append(ev); err != nil {
		return fmt.Errorf("journal terminal branch cleanup: %w", err)
	}
	return nil
}
