package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Read-only Azure DevOps access checks for `goobers validate --check-repos`
// (docs/design/ado-parity-dsl-2-0.md §7.3, ADO-N34). Every call here reads:
// the one POST (permissionevaluationbatch) evaluates permissions and changes
// nothing. All of them address the configured organization base URL only, so
// they add no host (SEC-048).

// adoGitRepositoriesNamespace is the security namespace ID of Azure DevOps Git
// repositories. It is the same in every organization.
const adoGitRepositoriesNamespace = "2e9eb7ed-3c0a-47d4-87c1-0ffdd275fd87"

// ADOGitPermission is one Git repositories security-namespace permission. The
// value is its bit in that namespace.
type ADOGitPermission int

// The Git repositories permissions `validate --check-repos` evaluates.
const (
	ADOGitForcePush               ADOGitPermission = 8
	ADOGitContribute              ADOGitPermission = 4
	ADOGitCreateBranch            ADOGitPermission = 16
	ADOGitPolicyExempt            ADOGitPermission = 128
	ADOGitPullRequestContribute   ADOGitPermission = 16384
	ADOGitPullRequestBypassPolicy ADOGitPermission = 32768
)

// String returns the permission's Azure DevOps name.
func (p ADOGitPermission) String() string {
	switch p {
	case ADOGitContribute:
		return "Contribute"
	case ADOGitForcePush:
		return "ForcePush"
	case ADOGitCreateBranch:
		return "CreateBranch"
	case ADOGitPolicyExempt:
		return "PolicyExempt"
	case ADOGitPullRequestContribute:
		return "PullRequestContribute"
	case ADOGitPullRequestBypassPolicy:
		return "PullRequestBypassPolicy"
	default:
		return fmt.Sprintf("GitPermission(%d)", int(p))
	}
}

// ADORepositoryIDs holds the GUIDs Azure DevOps security tokens and policy
// scopes use for a repository.
type ADORepositoryIDs struct {
	ProjectID     string
	RepositoryID  string
	DefaultBranch string
}

// ADOBranchPolicy is a policy configuration named in an access report.
type ADOBranchPolicy struct {
	ID       int
	TypeName string
	RefName  string
}

type adoRepositoryResponse struct {
	ID            string `json:"id"`
	DefaultBranch string `json:"defaultBranch"`
	Project       struct {
		ID string `json:"id"`
	} `json:"project"`
}

type adoPermissionEvaluation struct {
	SecurityNamespaceID string `json:"securityNamespaceId"`
	Token               string `json:"token"`
	Permissions         int    `json:"permissions"`
	Value               *bool  `json:"value,omitempty"`
}

type adoPermissionEvaluationBatch struct {
	AlwaysAllowAdministrators bool                      `json:"alwaysAllowAdministrators"`
	Evaluations               []adoPermissionEvaluation `json:"evaluations"`
}

// RepositoryIDs reads the project and repository GUIDs of repo.
func (p *ADOProvider) RepositoryIDs(ctx context.Context, repo RepositoryRef) (ADORepositoryIDs, error) {
	if err := requireRepo(repo); err != nil {
		return ADORepositoryIDs{}, err
	}
	endpoint, err := p.repoURL(repo)
	if err != nil {
		return ADORepositoryIDs{}, err
	}
	var out adoRepositoryResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return ADORepositoryIDs{}, err
	}
	ids := ADORepositoryIDs{
		ProjectID:     strings.TrimSpace(out.Project.ID),
		RepositoryID:  strings.TrimSpace(out.ID),
		DefaultBranch: out.DefaultBranch,
	}
	if ids.ProjectID == "" || ids.RepositoryID == "" {
		return ADORepositoryIDs{}, fmt.Errorf("ADO repository %s has no project or repository id", repo.Name)
	}
	return ids, nil
}

// EvaluateGitPermissions evaluates, for the identity the provider
// authenticates as, each Git repositories permission on the repository ids
// names. The security token is repoV2/<projectId>/<repositoryId>. Project
// administrators are not treated as holding every permission, so the result
// is the identity's effective access control.
func (p *ADOProvider) EvaluateGitPermissions(ctx context.Context, ids ADORepositoryIDs, permissions []ADOGitPermission) (map[ADOGitPermission]bool, error) {
	if ids.ProjectID == "" || ids.RepositoryID == "" {
		return nil, fmt.Errorf("project and repository ids are required to evaluate ADO permissions")
	}
	endpoint, err := joinURL(p.BaseURL, p.Organization, "_apis", "security", "permissionevaluationbatch")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"api-version": []string{"7.1"}})
	if err != nil {
		return nil, err
	}
	token := "repoV2/" + ids.ProjectID + "/" + ids.RepositoryID
	request := adoPermissionEvaluationBatch{}
	for _, permission := range permissions {
		request.Evaluations = append(request.Evaluations, adoPermissionEvaluation{
			SecurityNamespaceID: adoGitRepositoriesNamespace,
			Token:               token,
			Permissions:         int(permission),
		})
	}
	var response adoPermissionEvaluationBatch
	if err := p.do(ctx, http.MethodPost, endpoint, request, &response); err != nil {
		return nil, err
	}
	return adoPermissionResults(response, permissions)
}

// adoPermissionResults maps the evaluations ADO returned back to the requested
// permissions. An evaluation with no value, or a requested permission missing
// from the response, is an error: the caller must not read "unknown" as
// "denied".
func adoPermissionResults(response adoPermissionEvaluationBatch, permissions []ADOGitPermission) (map[ADOGitPermission]bool, error) {
	results := make(map[ADOGitPermission]bool, len(permissions))
	for _, evaluation := range response.Evaluations {
		if evaluation.Value == nil {
			continue
		}
		results[ADOGitPermission(evaluation.Permissions)] = *evaluation.Value
	}
	for _, permission := range permissions {
		if _, ok := results[permission]; !ok {
			return nil, fmt.Errorf("ADO permission evaluation returned no value for %s", permission)
		}
	}
	return results, nil
}

// BlanketPrefixPolicies returns the enabled, blocking policy configurations
// of repo's project that apply to the repository with repositoryID through a
// Prefix scope covering all of refs/heads/. Such a policy makes every branch
// PR-only, so Goobers cannot push a run branch (live probe F8). Scopes that
// name no ref, or name a narrower folder or one branch, are not reported.
func (p *ADOProvider) BlanketPrefixPolicies(ctx context.Context, repo RepositoryRef, repositoryID string) ([]ADOBranchPolicy, error) {
	configs, err := p.branchPolicyConfigurations(ctx, repo)
	if err != nil {
		return nil, err
	}
	var blanket []ADOBranchPolicy
	for _, c := range configs {
		if !c.IsEnabled || !c.IsBlocking || c.IsDeleted {
			continue
		}
		for _, scope := range c.Settings.Scope {
			if adoScopeIsBlanketPrefix(scope, repositoryID) {
				blanket = append(blanket, ADOBranchPolicy{ID: c.ID, TypeName: c.Type.DisplayName, RefName: scope.RefName})
				break
			}
		}
	}
	sort.SliceStable(blanket, func(i, j int) bool { return blanket[i].ID < blanket[j].ID })
	return blanket, nil
}

// adoScopeIsBlanketPrefix reports whether scope is a Prefix scope on the
// repository (or on every repository) whose folder contains refs/heads/.
func adoScopeIsBlanketPrefix(scope adoPolicyScope, repositoryID string) bool {
	if !strings.EqualFold(scope.MatchKind, "prefix") {
		return false
	}
	if scope.RepositoryID != "" && repositoryID != "" && !strings.EqualFold(scope.RepositoryID, repositoryID) {
		return false
	}
	folder := strings.TrimSuffix(strings.TrimSpace(scope.RefName), "/")
	return folder == "refs" || folder == "refs/heads"
}

// WorkItemTypeStates returns the state names of one work item type in the
// project repo addresses, with their categories.
func (p *ADOProvider) WorkItemTypeStates(ctx context.Context, repo RepositoryRef, itemType string) ([]ADOWorkItemState, error) {
	states, err := p.adoWorkItemStateCategories(ctx, repo, itemType)
	if err != nil {
		return nil, err
	}
	out := make([]ADOWorkItemState, 0, len(states))
	for _, state := range states {
		out = append(out, ADOWorkItemState(state))
	}
	return out, nil
}

// ADOWorkItemState is one state of an Azure Boards work item type.
type ADOWorkItemState struct {
	Name     string `json:"name"`
	Category string `json:"category"`
}

// DefaultCreateType returns the work item type Goobers creates in the project
// repo addresses when no type is configured: the default type of the
// Requirement category (ADO-N27).
func (p *ADOProvider) DefaultCreateType(ctx context.Context, repo RepositoryRef) (string, error) {
	return p.adoDefaultRequirementType(ctx, p.project(repo))
}
