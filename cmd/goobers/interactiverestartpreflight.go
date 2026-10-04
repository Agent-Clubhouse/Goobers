package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

func (s *interactiveStageRestart) preflight(ctx context.Context, plan *runner.StageRestartPlan, load interactiveaccess.RestartSourceLoader) ([]localscheduler.ClaimEntry, error) {
	if err := s.verifyReleasedSource(ctx, plan.Source); err != nil {
		return nil, err
	}
	pinned, release, err := pinnedCredentialDefinitions(ctx, s.layout, plan.Source.ConfigGeneration)
	if err != nil {
		return nil, err
	}
	defer release()
	scope, ok := pinned.Scopes[plan.Source.Gaggle]
	if !ok {
		return nil, restartRefusal("restart_source_missing", "The retained generation has no source gaggle.")
	}
	repository := restartSourceRepository(*plan, scope.Project)
	claims, err := claimHistoryForRun(s.layout, plan.Source.RunID, "")
	if err != nil {
		return nil, err
	}
	request := interactiveaccess.RestartSourceRequest{}
	if repository != nil {
		identity := interactiveRepositoryIdentity(*repository)
		request.Repository = &identity
	}
	for _, claim := range claims {
		if !strings.HasPrefix(continuationClaimID(claim), pullRequestClaimPrefix) {
			request.Backlog = true
		} else if repository == nil || interactiveRepositoryIdentity(*repository) != interactiveRepositoryIdentity(scope.Project) || repository.BaseURL != scope.Project.BaseURL {
			return nil, restartRefusal("restart_pr_scope_ambiguous", "The selected workspace differs from the source PR repository; an explicit claim-to-repository binding is required.")
		}
	}
	sources, err := load(ctx, request)
	if err != nil {
		return nil, err
	}
	if sources.Gaggle.Spec.Enabled != nil && !*sources.Gaggle.Spec.Enabled {
		return nil, restartRefusal("restart_gaggle_disabled", "The source gaggle is disabled.")
	}
	current := s.setup.Interventions.Snapshot().machines[localscheduler.WorkflowIdentity{Gaggle: plan.Source.Gaggle, Workflow: plan.Source.Workflow}]
	if current == nil || (current.Def.Spec.Enabled != nil && !*current.Def.Spec.Enabled) {
		return nil, restartRefusal("restart_workflow_disabled", "The source workflow is removed or disabled.")
	}
	definition := &apiv1.Workflow{Spec: current.Def.Spec}
	definition.Name = current.Def.Name
	policy, err := continuationEligibilityPolicyFromDefinitions(s.setup.Config, sources.Gaggle, definition)
	if err != nil {
		return nil, err
	}
	if request.Backlog && !sameRestartBacklog(scope, sources.Gaggle) {
		return nil, restartRefusal("restart_backlog_changed", "The backlog location differs from the retained source; claims cannot be rebound to another backlog.")
	}
	var code restartReadProvider
	if repository != nil {
		if !restartRepositoryConfigured(*repository, sources.Gaggle) {
			return nil, restartRefusal("restart_repository_changed", "The retained source repository is not an exact configured repository.")
		}
		code, err = restartProvider(*repository, sources.Repository)
		if err != nil {
			return nil, err
		}
		if err := prepareRestartBranch(ctx, s.layout.ForGaggle(plan.Source.Gaggle).RunsDir(), plan, *repository, code); err != nil {
			return nil, err
		}
	}
	var backlog restartReadProvider
	backlogRepo := restartProviderRepository(sources.BacklogIdentity)
	if request.Backlog {
		backlog, err = restartProvider(apiv1.RepoRef{Provider: sources.BacklogIdentity.Provider, Owner: sources.BacklogIdentity.Owner, Project: sources.BacklogIdentity.Project, Name: sources.BacklogIdentity.Name}, sources.Backlog)
		if err != nil {
			return nil, err
		}
	}
	var codeRepo providers.RepositoryRef
	if repository != nil {
		codeRepo = restartProviderRepository(interactiveRepositoryIdentity(*repository))
	}
	if err := verifyRestartClaims(ctx, plan.Source, claims, code, codeRepo, backlog, backlogRepo, policy); err != nil {
		return nil, err
	}
	return claims, nil
}

func restartSourceRepository(plan runner.StageRestartPlan, project apiv1.RepoRef) *apiv1.RepoRef {
	if revision := plan.SourceWorkspaceRevision; revision != nil {
		r := revision.Repository
		return &apiv1.RepoRef{Provider: r.Provider, BaseURL: r.URL, Owner: r.Owner, Project: r.Project, Name: r.Name}
	}
	if plan.Source.WorkspaceRepository != nil {
		return plan.Source.WorkspaceRepository.DeepCopy()
	}
	if project.Name == "" {
		return nil
	}
	return project.DeepCopy()
}

func interactiveRepositoryIdentity(r apiv1.RepoRef) apiv1.InteractiveRepositoryIdentity {
	return apiv1.InteractiveRepositoryIdentity{Provider: r.Provider, Owner: r.Owner, Project: r.Project, Name: r.Name}
}

func restartProviderRepository(r apiv1.InteractiveRepositoryIdentity) providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderKind(r.Provider), Owner: r.Owner, Project: r.Project, Name: r.Name}
}

func restartRepositoryConfigured(repo apiv1.RepoRef, g *apiv1.Gaggle) bool {
	for _, candidate := range append([]apiv1.RepoRef{g.Spec.Project}, g.Spec.AdditionalRepos...) {
		if interactiveRepositoryIdentity(repo) == interactiveRepositoryIdentity(candidate) && repo.BaseURL == candidate.BaseURL {
			return true
		}
	}
	return false
}

func sameRestartBacklog(scope credentialGaggleScope, current *apiv1.Gaggle) bool {
	return scope.Backlog.Provider == current.Spec.Backlog.Provider && scope.Backlog.Project == current.Spec.Backlog.Project && (scope.Backlog.Provider != apiv1.ProviderADO || (scope.Project.Provider == current.Spec.Project.Provider && scope.Project.Owner == current.Spec.Project.Owner && scope.Project.BaseURL == current.Spec.Project.BaseURL))
}

func (s *interactiveStageRestart) verifyReleasedSource(ctx context.Context, source journal.RunIdentity) error {
	manager := s.setup.WorktreesByGaggle[source.Gaggle]
	if manager == nil {
		return restartRefusal("restart_workspace_unavailable", "The source workspace manager is unavailable.")
	}
	if err := manager.AssertRunReleased(ctx, source.RunID); err != nil {
		return restartRefusal("restart_workspace_retained", err.Error())
	}
	entries, unreadable, err := recovery.ReadInventoryTolerant(ctx, filepath.Join(s.layout.Root, "recovery"), recovery.MaxInventoryEntries)
	if err != nil {
		return err
	}
	overflow, unknown, err := recovery.ReadOverflow(ctx, recoveryOverflowRoot(s.layout))
	if err != nil {
		return err
	}
	if len(unreadable)+len(unknown) != 0 {
		return restartRefusal("restart_recovery_uncertain", "Unattributed recovery state must be reconciled before restarting from a published branch.")
	}
	for _, entry := range append(entries, overflow...) {
		if entry.Record.RunID == source.RunID {
			return restartRefusal("restart_recovery_retained", "The source has retained implementation work; restore or explicitly resolve it before restarting from a published branch.")
		}
	}
	return nil
}

func prepareRestartBranch(ctx context.Context, runsDir string, plan *runner.StageRestartPlan, repo apiv1.RepoRef, provider restartReadProvider) error {
	reader, err := journal.OpenReadOnly(filepath.Join(runsDir, plan.Source.RunID))
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	branch, sha := plan.Source.WorkspaceBranch, plan.Source.WorkspaceBranchSHA
	for _, event := range events {
		if event.IsReferenceTouch() && event.ExternalRef.Kind == "branch" {
			branch, sha = event.ExternalRef.ID, event.ExternalRef.CommitSHA
		}
	}
	if branch == "" || apiv1.ValidateCommitSHA(sha) != nil {
		return restartRefusal("restart_branch_unverified", "The source has no recorded published branch and exact commit. Retained workspace adoption is required.")
	}
	plan.Continuation.SourceBranch, plan.Continuation.ExpectedSourceSHA = branch, sha
	plan.Continuation.SourceRepository = repo.DeepCopy()
	plan.Continuation.VerifySourceBranch = func(branch, sha string) error {
		current, found, err := provider.GetBranch(ctx, restartProviderRepository(interactiveRepositoryIdentity(repo)), branch)
		if err != nil {
			return err
		}
		if !found || current.SHA != sha {
			return errors.New("published source branch changed; refresh the source instead of resetting its workspace")
		}
		return nil
	}
	return plan.Continuation.VerifySourceBranch(branch, sha)
}
