// Package workspacebranch owns the explicit selected-revision writable transition.
package workspacebranch

import (
	"reflect"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/providers"
)

// Backend operation kinds are static deterministic task selectors.
const (
	KindEstablish = "workspace-branch-establish"
	KindPublish   = "workspace-branch-publish"
)

// BackendKind selects trusted operations from static task configuration only.
func BackendKind(inputs map[string]string) bool {
	return inputs["kind"] == KindEstablish || inputs["kind"] == KindPublish
}

// ValidateKind prevents inputsFrom or an experiment from selecting a privileged
// backend operation that the pinned task did not explicitly declare.
func ValidateKind(task apiv1.Task, resolved map[string]interface{}) error {
	kind, _ := resolved["kind"].(string)
	if kind != KindEstablish && kind != KindPublish && !BackendKind(task.Inputs) {
		return nil
	}
	if task.Type != apiv1.TaskDeterministic || task.Inputs["kind"] != kind {
		return &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized, Message: "workspace branch operations must be selected by static deterministic task configuration"}
	}
	return nil
}

// Expected derives ownership exclusively from configured base and run identity.
func Expected(base apiv1.RepoRef, revision *apiv1.WorkspaceRevision, namespace, workflow, runID string) (*apiv1.WorkspaceBranchBinding, error) {
	if revision == nil || workflow == "" || runID == "" {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "branch establishment requires selected revision and run identity"}
	}
	identity := apiv1.RepositoryIdentity{Provider: base.Provider, Owner: base.Owner, Project: base.Project, Name: base.Name}
	if base.BaseURL != "" {
		identity.URL = strings.TrimSuffix(base.BaseURL, "/") + "/" + base.Owner + "/" + base.Name
		if base.Provider == apiv1.ProviderADO {
			identity.URL = strings.TrimSuffix(base.BaseURL, "/") + "/" + base.Owner + "/" + base.Project + "/_git/" + base.Name
		}
	}
	binding := &apiv1.WorkspaceBranchBinding{
		Repository: identity, Ref: "refs/heads/" + providers.BranchNameIn(namespace, workflow, runID),
		StartingSHA: revision.CommitSHA,
	}
	if err := binding.Validate(); err != nil {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: err.Error()}
	}
	if _, sameBase := workspacerevision.Resolve(*revision, base, nil); sameBase == nil &&
		strings.TrimPrefix(revision.SourceRef, "refs/heads/") == strings.TrimPrefix(binding.Ref, "refs/heads/") {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeConflict, Message: "owned target collides with the selected source branch"}
	}
	return binding, nil
}

// Accept validates a trusted establishment result before journaling or rebinding.
func Accept(current, candidate *apiv1.WorkspaceBranchBinding, revision *apiv1.WorkspaceRevision, base apiv1.RepoRef, namespace, workflow, runID string, producer, success bool) (*apiv1.WorkspaceBranchBinding, error) {
	if candidate == nil {
		return current.DeepCopy(), nil
	}
	if !producer {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized, Message: "only the explicit backend establishment stage may establish branch ownership"}
	}
	if !success {
		return current.DeepCopy(), nil
	}
	expected, err := Expected(base, revision, namespace, workflow, runID)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(candidate, expected) || current != nil && !reflect.DeepEqual(current, candidate) {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeConflict, Message: "remote workspace branch ownership cannot change"}
	}
	return candidate.DeepCopy(), nil
}

// ValidateResult also refuses scalar rebinding once remote ownership exists.
func ValidateResult(current *apiv1.WorkspaceBranchBinding, revision *apiv1.WorkspaceRevision, base apiv1.RepoRef, namespace, workflow, runID string, task apiv1.Task, result apiv1.ResultEnvelope) (*apiv1.WorkspaceBranchBinding, error) {
	binding, err := Accept(current, result.WorkspaceBranchBinding, revision, base, namespace, workflow, runID,
		task.Type == apiv1.TaskDeterministic && task.Inputs["kind"] == KindEstablish, result.Status == apiv1.ResultSuccess)
	if err != nil {
		return nil, err
	}
	if result.WorkspaceBranchTip != "" {
		if binding == nil || task.Type != apiv1.TaskDeterministic || task.Inputs["kind"] != KindPublish {
			return nil, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized, Message: "only the trusted publication stage may acknowledge a remote tip"}
		}
		if err := apiv1.ValidateCommitSHA(result.WorkspaceBranchTip); err != nil {
			return nil, &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "invalid published workspace tip", Cause: err}
		}
	}
	if binding != nil && result.Status == apiv1.ResultSuccess {
		if value, exists := result.Outputs["workspaceBranch"]; exists && value != strings.TrimPrefix(binding.Ref, "refs/heads/") {
			return nil, &workspacerevision.Error{Code: workspacerevision.CodeConflict, Message: "workspaceBranch cannot redirect remote ownership"}
		}
	}
	return binding, nil
}
