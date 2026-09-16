package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/worktree"
)

func workspaceBranchCleanupEvidence(events []journal.Event, machine *workflow.Machine, base apiv1.RepoRef, additional []apiv1.RepoRef, namespace, runID string) (*apiv1.WorkspaceBranchBinding, string, error) {
	binding, err := runner.RestoredWorkspaceBranchBinding(events, machine, base, additional, namespace, runID)
	if err != nil || binding == nil {
		return binding, "", err
	}
	expected := binding.StartingSHA
	for _, event := range events {
		if event.Type != journal.EventStageFinished || event.Status != string(apiv1.ResultSuccess) {
			continue
		}
		task, ok := machine.Task(event.Stage)
		if ok && task.Inputs["kind"] == workspacebranch.KindPublish {
			// An old publication without typed acknowledgment is ambiguous.
			// Never use a fresh remote observation to manufacture its lease.
			expected = event.WorkspaceBranchTip
		}
	}
	return binding, expected, nil
}

func finalizeOwnedWorkspaceBranch(l instance.Layout, cfg *instance.Config, base apiv1.RepoRef, registrar terminalSecretRegistry, stores credentials.StoreResolver, runID string, annotate terminalAnnotator) (bool, error) {
	reader, err := journal.OpenRead(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		return true, err
	}
	events, err := reader.Events()
	if err != nil {
		return true, err
	}
	record := func(result worktree.RemoteBranchCleanupResult, binding *apiv1.WorkspaceBranchBinding, cause error) (bool, error) {
		detail := map[string]any{"kind": "workspace_branch_cleanup", "outcome": string(result.Outcome),
			"expectedSha": result.ExpectedSHA, "observedSha": result.ObservedSHA}
		if binding != nil {
			detail["repository"], detail["ref"] = binding.Repository, binding.Ref
		}
		auditErr := annotate.Append(journal.Event{Type: journal.EventRunnerAnnotation, RunID: runID, Runner: detail})
		return true, errors.Join(scrubTerminalError(registrar, cause), auditErr)
	}
	invalid := worktree.RemoteBranchCleanupResult{Outcome: worktree.CleanupOwnershipInvalid}
	identity, err := reader.Identity()
	if err != nil {
		return record(invalid, nil, err)
	}
	machine, err := pinnedWorkspaceCleanupWorkflow(reader, identity, events)
	if err != nil {
		return record(invalid, nil, err)
	}
	if machine == nil {
		for _, event := range events {
			if event.WorkspaceBranchBinding != nil || event.WorkspaceBranchTip != "" {
				return record(invalid, event.WorkspaceBranchBinding, errors.New("workspace ownership has no pinned establishment operation"))
			}
		}
		return false, nil
	}
	set, report, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		return record(invalid, nil, errors.Join(err, errors.New(validationIssueSummary(report))))
	}
	gaggle := configuredGaggle(set, identity.Gaggle)
	if gaggle == nil || base.Provider == "" {
		return record(invalid, nil, errors.New("owned workspace gaggle or configured base is unavailable"))
	}
	binding, expected, err := workspaceBranchCleanupEvidence(events, machine, base, gaggle.Spec.AdditionalRepos,
		branchNamespacesByGaggle(set)[identity.Gaggle], runID)
	if err != nil {
		return record(invalid, binding, err)
	}
	if binding == nil || expected == "" {
		return record(worktree.RemoteBranchCleanupResult{Outcome: worktree.CleanupOwnershipMissing}, binding, nil)
	}
	owner := base.Owner
	if base.Provider == apiv1.ProviderADO {
		owner += "/" + base.Project
	}
	resolver, grants, err := buildCredentials(cfg, stores, owner, base.Name, nil, registrar)
	if err != nil {
		return record(invalid, binding, err)
	}
	injector, err := credentials.NewInjector(resolver, grants, registrar)
	if err != nil {
		return record(invalid, binding, err)
	}
	executor := workspaceBranchExecutor{input: deterministicExecutorInput{Config: cfg}, injector: injector}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	access, err := executor.access(ctx, base, string(capability.RepoPush), true)
	if err != nil {
		return record(worktree.RemoteBranchCleanupResult{Outcome: worktree.CleanupFailed, ExpectedSHA: expected}, binding, err)
	}
	clone := repoCloneURL
	if clone == nil {
		clone = runner.DefaultRepoCloneURL
	}
	target, err := clone(base)
	if err != nil {
		return record(invalid, binding, err)
	}
	result, err := worktree.CleanupRemoteBranch(ctx, worktree.RemoteBranchOptions{
		TargetURL: target, Binding: *binding, TargetWrite: access,
	}, expected)
	return record(result, binding, err)
}

func pinnedWorkspaceCleanupWorkflow(reader *journal.Reader, identity journal.RunIdentity, events []journal.Event) (*workflow.Machine, error) {
	relevant := false
	for _, event := range events {
		relevant = relevant || event.WorkspaceRevision != nil || event.WorkspaceBranchBinding != nil || event.WorkspaceBranchTip != ""
	}
	if !relevant {
		pinned := false
		for _, input := range identity.Inputs {
			pinned = pinned || input.Name == journal.PinnedWorkflowDefinitionInputName
		}
		if !pinned {
			return nil, nil
		}
	}
	machine, err := runner.PinnedWorkflowMachine(reader, identity)
	if err != nil {
		return nil, err
	}
	for _, task := range machine.Def.Spec.Tasks {
		if workspacebranch.BackendKind(task.Inputs) {
			return machine, nil
		}
	}
	return nil, nil
}
