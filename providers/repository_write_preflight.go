package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// RepoWriteFailureCapability names the specific repository-write capability
// that failed during a non-mutating preflight check (#4414), so a caller
// (the implementation-claim gate, status/diagnose) can report exactly what
// is broken instead of a generic failure. The four values mirror the states
// the issue requires be distinguished: unreachable/unauthorized,
// authenticated-but-no-push, branch-policy-blocked, and
// introspection-unavailable — the last is reported explicitly rather than
// inferred as success, since an unreadable ruleset is not the same fact as
// no ruleset.
type RepoWriteFailureCapability string

const (
	// RepoWriteFailureUnauthorized means the repository was unreachable or
	// the credential was rejected outright (HTTP 401/403/404 fetching the
	// repository itself).
	RepoWriteFailureUnauthorized RepoWriteFailureCapability = "repo.push.unauthorized"
	// RepoWriteFailureNoPushPermission means the credential authenticated
	// successfully but GitHub reports it lacks push permission on the
	// repository.
	RepoWriteFailureNoPushPermission RepoWriteFailureCapability = "repo.push.no-permission"
	// RepoWriteFailureBranchPolicy means a branch_name_pattern ruleset rule
	// actually denies the checked branch name, evaluated by its own
	// operator/pattern/negate parameters.
	RepoWriteFailureBranchPolicy RepoWriteFailureCapability = "repo.push.branch-policy"
	// RepoWriteFailurePolicyIntrospectionUnavailable means GitHub answered
	// but did not expose the information this check needs (push permission
	// or branch ruleset evaluation) for this credential or plan — reported
	// explicitly rather than inferred as either a pass or a denial.
	RepoWriteFailurePolicyIntrospectionUnavailable RepoWriteFailureCapability = "repo.push.policy-introspection-unavailable"
)

// RepositoryWritePreflightResult reports whether the credential presently
// configured for a repository can push a given branch, without mutating any
// repository state. OK is true only when no known failure was detected;
// every non-OK result names its FailureCapability — a zero-value result
// (OK false, FailureCapability empty) is never returned.
type RepositoryWritePreflightResult struct {
	OK                bool
	FailureCapability RepoWriteFailureCapability
	Detail            string
}

// RepositoryWritePreflighter is implemented by providers that can check
// repository-write viability ahead of a real push (#4414). It is optional,
// like MergePolicyDetector and the other per-capability interfaces in this
// package (§3.3): a provider that does not implement it is dispatched
// through Dispatcher.PreflightRepositoryWrite, which fails closed with
// ErrUnsupported rather than silently reporting success for a check it
// cannot actually perform.
type RepositoryWritePreflighter interface {
	PreflightRepositoryWrite(ctx context.Context, repo RepositoryRef, branch string) (RepositoryWritePreflightResult, error)
}

// githubRepoWritePermissionDetail is the subset of GET .../repos/{owner}/
// {repo} PreflightRepositoryWrite reads: GitHub includes `permissions` only
// for an authenticated request, reporting the caller's own admin/push/pull
// grant on the exact repository — read directly, never inferred from
// reachability alone.
type githubRepoWritePermissionDetail struct {
	Permissions *struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

// githubBranchNamePatternParameters is the Parameters shape of a
// "branch_name_pattern" branch rule (GitHub rulesets). GitHub's "rules for a
// branch" endpoint already filters its response to rules that structurally
// apply to the given branch (by ref/include-exclude target), but a
// branch_name_pattern rule's own match condition is a separate, additional
// gate that endpoint does not pre-evaluate: the rule denies the branch only
// when Pattern actually matches under Operator (or, with Negate, only when
// it does NOT match) — a prior attempt at this issue treated the rule's
// mere presence in the response as an unconditional denial, which falsely
// rejected every repository with a permissive branch-name rule.
type githubBranchNamePatternParameters struct {
	Operator string `json:"operator"`
	Pattern  string `json:"pattern"`
	Negate   bool   `json:"negate"`
	Name     string `json:"name"`
}

// branchNamePatternRuleBlocks reports whether params denies branch under
// GitHub's documented branch_name_pattern semantics: the rule denies the
// branch names its pattern matches, or — with Negate set — denies precisely
// the branch names that do NOT match.
func branchNamePatternRuleBlocks(params githubBranchNamePatternParameters, branch string) bool {
	matches := branchNamePatternMatches(params.Operator, params.Pattern, branch)
	if params.Negate {
		return !matches
	}
	return matches
}

// branchNamePatternMatches evaluates one GitHub branch_name_pattern
// operator. An unrecognized operator matches nothing rather than
// everything: the previous defect's failure mode was refusing too much, and
// a future GitHub operator this code does not yet know should not silently
// start refusing every branch either.
func branchNamePatternMatches(operator, pattern, branch string) bool {
	switch operator {
	case "starts_with":
		return strings.HasPrefix(branch, pattern)
	case "ends_with":
		return strings.HasSuffix(branch, pattern)
	case "contains":
		return strings.Contains(branch, pattern)
	case "regex":
		re, err := regexp.Compile(pattern)
		if err != nil {
			return false
		}
		return re.MatchString(branch)
	default:
		return false
	}
}

// describeBranchNamePattern renders params for a preflight failure's Detail
// text: the rule's own configured name when set, else its operator/pattern.
func describeBranchNamePattern(params githubBranchNamePatternParameters) string {
	if params.Name != "" {
		return params.Name
	}
	return fmt.Sprintf("%s %q", params.Operator, params.Pattern)
}

// PreflightRepositoryWrite checks, without mutating any repository state,
// whether p's configured credential can push branch to repo (#4414): a run
// that only discovers a bad credential or a blocking branch ruleset at
// `push-branch`, after spending implementation/review/CI resources, is the
// problem this exists to catch before an issue is claimed. It performs at
// most two reads: GET /repos/{owner}/{repo} (reachability, auth, and push
// permission) and GET /repos/{owner}/{repo}/rules/branches/{branch} (branch
// ruleset policy for the exact generated branch name) — the same "rules for
// a branch" endpoint DetectMergePolicy and GetRepoPolicy already use.
func (p *GitHubProvider) PreflightRepositoryWrite(ctx context.Context, repo RepositoryRef, branch string) (RepositoryWritePreflightResult, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return RepositoryWritePreflightResult{}, err
	}
	if strings.TrimSpace(branch) == "" {
		return RepositoryWritePreflightResult{}, fmt.Errorf("branch is required")
	}

	repoEndpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name)
	if err != nil {
		return RepositoryWritePreflightResult{}, err
	}
	var detail githubRepoWritePermissionDetail
	if err := p.do(ctx, http.MethodGet, repoEndpoint, nil, &detail); err != nil {
		var respErr *providerResponseError
		if errors.As(err, &respErr) &&
			(respErr.statusCode == http.StatusUnauthorized || respErr.statusCode == http.StatusNotFound || respErr.statusCode == http.StatusForbidden) {
			return RepositoryWritePreflightResult{
				FailureCapability: RepoWriteFailureUnauthorized,
				Detail:            fmt.Sprintf("repository unreachable or credential unauthorized (HTTP %d)", respErr.statusCode),
			}, nil
		}
		return RepositoryWritePreflightResult{}, err
	}
	if detail.Permissions == nil {
		// GitHub omits `permissions` for some authenticated request shapes
		// rather than reporting push:false — that is unavailable
		// introspection, not a denial, and must be reported as such rather
		// than inferred either way.
		return RepositoryWritePreflightResult{
			FailureCapability: RepoWriteFailurePolicyIntrospectionUnavailable,
			Detail:            "repository push permission is not reported for this credential",
		}, nil
	}
	if !detail.Permissions.Push {
		return RepositoryWritePreflightResult{
			FailureCapability: RepoWriteFailureNoPushPermission,
			Detail:            "credential is authenticated but lacks push permission on this repository",
		}, nil
	}

	rulesEndpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "rules", "branches", branch)
	if err != nil {
		return RepositoryWritePreflightResult{}, err
	}
	var rules []githubBranchRule
	if err := p.do(ctx, http.MethodGet, rulesEndpoint, nil, &rules); err != nil {
		var respErr *providerResponseError
		if errors.As(err, &respErr) && respErr.statusCode == http.StatusForbidden {
			// Plan-gated (the same entitlement gap DetectMergePolicy
			// degrades around), but this preflight's job is to say what it
			// does NOT know rather than assume no ruleset blocks the branch.
			return RepositoryWritePreflightResult{
				FailureCapability: RepoWriteFailurePolicyIntrospectionUnavailable,
				Detail:            "branch ruleset evaluation is unavailable for this credential or plan",
			}, nil
		}
		return RepositoryWritePreflightResult{}, err
	}
	for _, rule := range rules {
		if rule.Type != "branch_name_pattern" {
			continue
		}
		var params githubBranchNamePatternParameters
		if len(rule.Parameters) > 0 {
			if err := json.Unmarshal(rule.Parameters, &params); err != nil {
				return RepositoryWritePreflightResult{}, fmt.Errorf("decode branch_name_pattern rule: %w", err)
			}
		}
		if branchNamePatternRuleBlocks(params, branch) {
			return RepositoryWritePreflightResult{
				FailureCapability: RepoWriteFailureBranchPolicy,
				Detail:            fmt.Sprintf("branch ruleset %q denies branch %q", describeBranchNamePattern(params), branch),
			}, nil
		}
	}
	return RepositoryWritePreflightResult{OK: true}, nil
}
