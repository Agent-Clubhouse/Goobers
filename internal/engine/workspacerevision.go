package engine

import (
	"errors"
	"fmt"
	"strings"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func initializeWorkspaceAuthority(in *RunInput) (string, error) {
	var branch string
	if in.WorkspaceBranchBinding != nil {
		if _, err := workspacebranch.Accept(nil, in.WorkspaceBranchBinding, in.WorkspaceRevision, in.RepoRef,
			in.BranchNamespace, in.WorkflowName, in.RunID, true, true); err != nil {
			return "", err
		}
		in.WorkspaceBranchBinding = in.WorkspaceBranchBinding.DeepCopy()
		branch = strings.TrimPrefix(in.WorkspaceBranchBinding.Ref, "refs/heads/")
	}
	if in.WorkspaceRevision != nil {
		if _, err := workspacerevision.Resolve(*in.WorkspaceRevision, in.RepoRef, in.AdditionalRepos); err != nil {
			return "", err
		}
		in.WorkspaceRevision = in.WorkspaceRevision.DeepCopy()
	}
	return branch, nil
}

// Called after runTask accepts and journals controls, before recording continuity.
// Keep the ownership acknowledgment here so replay schedules the same commands.
func recordWorkspaceAuthority(ctx workflow.Context, in *RunInput, result apiv1.ResultEnvelope, branch string, rec *runJournal) (string, error) {
	if result.WorkspaceRevision != nil {
		in.WorkspaceRevision = result.WorkspaceRevision.DeepCopy()
	}
	if result.WorkspaceBranchBinding != nil {
		in.WorkspaceBranchBinding = result.WorkspaceBranchBinding.DeepCopy()
		branch = strings.TrimPrefix(result.WorkspaceBranchBinding.Ref, "refs/heads/")
		if err := rec.emitPending(ctx); err != nil {
			return "", err
		}
	}
	return branch, nil
}

// Legacy scalar rebinding applies only after this stage's continuity is recorded:
// its commits belong to the branch it was handed, not the next stage's binding.
func nextWorkspaceBranch(task apiv1.Task, result apiv1.ResultEnvelope, namespace, current string) (string, error) {
	if result.Status == apiv1.ResultFailure && task.ContinueOnError {
		return current, nil
	}
	branch, err := selectedWorkspaceBranch(task, result, namespace)
	if err != nil {
		return "", fmt.Errorf("stage %q selected workspace branch: %w", task.Name, err)
	}
	if branch == "" {
		return current, nil
	}
	return branch, nil
}

// Validate before stage.finished: rejected or unsuccessful controls must never
// become accepted journal authority, including when continueOnError is enabled.
func acceptWorkspaceRevision(in RunInput, task apiv1.Task, result apiv1.ResultEnvelope) (apiv1.ResultEnvelope, error) {
	if _, err := workspacebranch.ValidateResult(in.WorkspaceBranchBinding, in.WorkspaceRevision, in.RepoRef,
		in.BranchNamespace, in.WorkflowName, in.RunID, task, result); err != nil {
		return apiv1.ResultEnvelope{}, classifySeamError(err)
	}
	if result.Status != apiv1.ResultSuccess {
		result.WorkspaceBranchBinding = nil
		result.WorkspaceBranchTip = ""
	}
	if result.WorkspaceRevision == nil {
		return result, nil
	}
	selected, err := workspacerevision.Accept(in.WorkspaceRevision, result.WorkspaceRevision,
		task.Type == apiv1.TaskDeterministic, result.Status == apiv1.ResultSuccess)
	if err == nil && result.Status == apiv1.ResultSuccess {
		_, err = workspacerevision.Resolve(*selected, in.RepoRef, in.AdditionalRepos)
	}
	if err != nil {
		return apiv1.ResultEnvelope{}, classifySeamError(err)
	}
	result.WorkspaceRevision = nil
	if result.Status == apiv1.ResultSuccess {
		result.WorkspaceRevision = selected.DeepCopy()
	}
	return result, nil
}

func workspaceRevisionErrorCode(err error) string {
	var revisionErr *workspacerevision.Error
	if errors.As(err, &revisionErr) {
		return revisionErr.Code
	}

	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		switch appErr.Type() {
		case workspacerevision.CodeInvalid, workspacerevision.CodeUnauthorized,
			workspacerevision.CodeConflict, workspacerevision.CodeAcquisition,
			workspacerevision.CodeObjectType, workspacerevision.CodeSHAMismatch:
			return appErr.Type()
		}
	}
	return ""
}

func validateRevisionPublication(in RunInput, task apiv1.Task, result stageActivityResult) error {
	if in.WorkspaceRevision == nil || task.EffectiveWorkspace() != apiv1.WorkspaceRepoReadOnly {
		return nil
	}
	if result.WorkspaceDelta != "" || result.WorkspaceDeltaBase != "" ||
		result.WorkspaceDeltaTip != "" || result.WorkspaceDeltaUnchanged {
		return classifySeamError(&workspacerevision.Error{Code: workspacerevision.CodeInvalid,
			Message: "selected read-only workspaces cannot publish workspace delta continuity"})
	}
	return nil
}

func ensureSelectedRevisionJournal(ctx workflow.Context, in RunInput, mode apiv1.WorkspaceMode, rec *runJournal) error {
	if in.WorkspaceBranchBinding == nil && (in.WorkspaceRevision == nil || mode != apiv1.WorkspaceRepoReadOnly) {
		return nil
	}
	if !in.LiveJournal {
		return classifySeamError(&workspacerevision.Error{Code: workspacerevision.CodeInvalid,
			Message: "selected-revision pod checkout requires live journaling; the credential server authorizes only durably accepted stage.finished controls"})
	}
	// The credential server reads the product journal, not pending projection
	// state. Await the writer's acknowledgment before scheduling the pod.
	return rec.emitPending(ctx)
}

func invocationCheckoutCones(in RunInput, readonly bool) map[string][]string {
	repo := in.RepoRef
	if readonly && in.WorkspaceRevision != nil {
		if selected, err := workspacerevision.Resolve(*in.WorkspaceRevision, in.RepoRef, in.AdditionalRepos); err == nil {
			repo = selected
		}
	}
	if repo.Checkout == nil || len(repo.Checkout.Sparse) == 0 {
		return nil
	}
	return map[string][]string{"": append([]string(nil), repo.Checkout.Sparse...)}
}

func checkoutFromEnvelope(env apiv1.InvocationEnvelope) *apiv1.CheckoutSpec {
	if cones := env.CheckoutCones[""]; len(cones) != 0 {
		return &apiv1.CheckoutSpec{Sparse: append([]string(nil), cones...)}
	}
	return nil
}
