package dispatcher

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/workspacerevision"
)

// WorkspaceCheckout carries configured transport and policy separately from
// selected identity. A pod must never derive a route from WorkspaceRevision.
type WorkspaceCheckout struct {
	Repository   apiv1.RepoRef `json:"repository"`
	PartialClone bool          `json:"partialClone"`
}

func selectedWorkspaceCheckout(cfg Config, attempt Attempt) (*WorkspaceCheckout, error) {
	if binding := attempt.WorkspaceBranchBinding; binding != nil && apiv1.WorkspaceMode(attempt.Workspace).IsWritableRepo() {
		if _, err := workspacebranch.Accept(nil, binding, &apiv1.WorkspaceRevision{CommitSHA: binding.StartingSHA},
			attempt.WorkspaceRepository, attempt.BranchNamespace, attempt.Workflow, attempt.RunID, true, true); err != nil {
			return nil, err
		}
		if binding.Ref != "refs/heads/"+attempt.WorkspaceBranch || attempt.SyncBase {
			return nil, &workspacerevision.Error{Code: workspacerevision.CodeConflict, Message: "owned workspace cannot change branch or synchronize base"}
		}
		repo := attempt.WorkspaceRepository
		repo.Checkout = attempt.Checkout
		return &WorkspaceCheckout{Repository: repo, PartialClone: attempt.PartialClone}, nil
	}
	if attempt.WorkspaceRevision == nil {
		return nil, nil
	}
	if apiv1.WorkspaceMode(attempt.Workspace) != apiv1.WorkspaceRepoReadOnly {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeInvalid,
			Message: "selected revision requires repo-readonly"}
	}
	if attempt.WorkspaceBranch != "" || attempt.WorkspaceDelta != "" || attempt.SyncBase {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeConflict,
			Message: "selected revision cannot use branch, delta or syncBase"}
	}
	source, err := workspacerevision.Resolve(*attempt.WorkspaceRevision, attempt.WorkspaceRepository, cfg.WorkspaceRepositories[attempt.Gaggle])
	if err != nil {
		return nil, err
	}
	// The driver pins source checkout policy alongside the invocation. Do not
	// replace it with a later configuration reload while retrying the same SHA.
	source.Checkout = nil
	if attempt.Checkout != nil {
		policy := *attempt.Checkout
		policy.Sparse = append([]string(nil), policy.Sparse...)
		source.Checkout = &policy
	}
	return &WorkspaceCheckout{Repository: source, PartialClone: attempt.PartialClone}, nil
}

func workspaceRevisionEnv(cfg Config, attempt Attempt) []corev1.EnvVar {
	// Render entrypoints validate before calling stageEnv.
	checkout, _ := selectedWorkspaceCheckout(cfg, attempt)
	binding := ""
	if attempt.WorkspaceBranchBinding != nil {
		data, _ := json.Marshal(attempt.WorkspaceBranchBinding)
		binding = string(data)
	}
	if checkout == nil {
		return []corev1.EnvVar{
			{Name: EnvWorkspaceBranchBinding, Value: literalPodEnv(binding)},
			{Name: EnvWorkspaceRevision, Value: ""},
			{Name: EnvWorkspaceCheckout, Value: ""},
		}
	}
	revision, _ := json.Marshal(attempt.WorkspaceRevision)
	transport, _ := json.Marshal(checkout)
	if attempt.WorkspaceBranchBinding != nil && apiv1.WorkspaceMode(attempt.Workspace).IsWritableRepo() {
		return []corev1.EnvVar{
			{Name: EnvWorkspaceBranchBinding, Value: literalPodEnv(binding)},
			{Name: EnvWorkspaceRevision, Value: ""},
			{Name: EnvWorkspaceCheckout, Value: literalPodEnv(string(transport))},
			{Name: EnvCheckoutCapability, Value: ""},
		}
	}
	return []corev1.EnvVar{
		{Name: EnvWorkspaceBranchBinding, Value: literalPodEnv(binding)},
		{Name: EnvWorkspaceRevision, Value: literalPodEnv(string(revision))},
		{Name: EnvWorkspaceCheckout, Value: literalPodEnv(string(transport))},
		// Explicit empty shadows image/EnvFrom writable provisioning controls.
		{Name: EnvWorkspaceBranch, Value: ""},
		{Name: EnvWorkspaceDelta, Value: ""},
		{Name: EnvStageSyncBase, Value: ""},
		{Name: EnvCheckoutCapability, Value: ""},
	}
}
