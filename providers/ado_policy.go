package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Well-known Azure DevOps branch-policy type ids (configuration.type.id). They
// are the same on every organization, so merge readiness classifies an
// evaluation by type id rather than by its localizable display name (design
// ado-parity-dsl-2-0.md §5.1, ADO-N19).
const (
	adoPolicyTypeBuild               = "0609b952-1397-4640-95ec-e00a01b2c241"
	adoPolicyTypeStatus              = "cbdc66da-9728-4af8-aada-9a5a32e4a226"
	adoPolicyTypeMinimumReviewers    = "fa4e907d-c16b-4a4c-9dfa-4906e5d171dd"
	adoPolicyTypeRequiredReviewers   = "fd2167ab-b0be-447a-8ec8-39368250530e"
	adoPolicyTypeCommentRequirements = "c6a1889d-b943-4856-b76f-9e46bb6b0df2"
	adoPolicyTypeWorkItemLinking     = "40e92b44-2fe1-4dd6-b3d8-74a9c21d0c6e"
)

// adoPolicyKind is what an evaluation means to Goobers.
type adoPolicyKind int

const (
	// adoPolicyOther is any type without a specific meaning. It gates CI
	// exactly as every policy did before classification existed.
	adoPolicyOther adoPolicyKind = iota
	// adoPolicyCI is a build or status policy: it drives CI state.
	adoPolicyCI
	// adoPolicyReviewer is a minimum- or required-reviewer policy: an unmet
	// one is a wait on a human, never CI pending and never a remediation
	// trigger.
	adoPolicyReviewer
	// adoPolicyCommentResolution rejected means unresolved threads.
	adoPolicyCommentResolution
	// adoPolicyWorkItemLinking rejected means the pull request has no linked
	// work item.
	adoPolicyWorkItemLinking
)

func adoPolicyKindOf(typeID string) adoPolicyKind {
	switch strings.ToLower(strings.TrimSpace(typeID)) {
	case adoPolicyTypeBuild, adoPolicyTypeStatus:
		return adoPolicyCI
	case adoPolicyTypeMinimumReviewers, adoPolicyTypeRequiredReviewers:
		return adoPolicyReviewer
	case adoPolicyTypeCommentRequirements:
		return adoPolicyCommentResolution
	case adoPolicyTypeWorkItemLinking:
		return adoPolicyWorkItemLinking
	default:
		return adoPolicyOther
	}
}

// adoEvaluationBlocks reports whether ev is an enabled, blocking evaluation,
// the only kind that can hold a completion.
func adoEvaluationBlocks(ev adoPolicyEvaluation) bool {
	return ev.Configuration.IsEnabled && ev.Configuration.IsBlocking
}

// adoEvaluationSatisfied reports whether ev no longer holds a completion.
func adoEvaluationSatisfied(ev adoPolicyEvaluation) bool {
	switch strings.ToLower(ev.Status) {
	case "approved", "notapplicable":
		return true
	default:
		return false
	}
}

// adoEvaluationState maps one evaluation to a check state. A broken build or
// status policy is a CI failure; for any other kind "broken" keeps its
// previous meaning (not reported).
func adoEvaluationState(ev adoPolicyEvaluation, kind adoPolicyKind) CheckState {
	if kind == adoPolicyCI && strings.EqualFold(ev.Status, "broken") {
		return CheckStateFailing
	}
	return adoPolicyCheckState(ev.Status)
}

// adoPolicyGate reduces the gating evaluations of one pull request to a
// single CI state.
type adoPolicyGate struct {
	failing, pending, passing, sawGate, sawReviewer bool
}

func (g *adoPolicyGate) observe(state CheckState) {
	g.sawGate = true
	switch state {
	case CheckStateFailing:
		g.failing = true
	case CheckStatePending:
		g.pending = true
	case CheckStatePassing:
		g.passing = true
	}
}

// result is failing when any gate failed and passing once every gate passed.
// With no gating policy at all it stays pending (fail-closed), except when
// ADO did evaluate reviewer policies for the pull request: then the branch
// has no CI to wait for, and the reviewer wait is reported per check as
// AwaitingHuman instead of pinning CI to pending forever.
func (g adoPolicyGate) result() CheckState {
	switch {
	case g.failing:
		return CheckStateFailing
	case g.sawGate && g.passing && !g.pending:
		return CheckStatePassing
	case !g.sawGate && g.sawReviewer:
		return CheckStatePassing
	default:
		return CheckStatePending
	}
}

// reducePolicyEvaluations classifies a pull request's evaluations (design
// ado-parity-dsl-2-0.md §5.1). Build, status and unclassified policies gate
// CI unless listed in humanOnly. Reviewer policies never gate CI: an unmet
// one is reported as AwaitingHuman.
func (p *ADOProvider) reducePolicyEvaluations(evals []adoPolicyEvaluation, projectName string, humanOnly map[string]bool) (CheckState, []CheckDetail) {
	checks := make([]CheckDetail, 0, len(evals))
	var gate adoPolicyGate
	for _, ev := range evals {
		if !adoEvaluationBlocks(ev) {
			continue
		}
		kind := adoPolicyKindOf(ev.Configuration.Type.ID)
		state := adoEvaluationState(ev, kind)
		if state == "" {
			continue
		}
		checks = append(checks, p.adoPolicyCheckDetail(ev, kind, state, projectName))
		switch {
		case kind == adoPolicyReviewer:
			gate.sawReviewer = true
		case humanOnly[ev.Configuration.ID.String()]:
		default:
			gate.observe(state)
		}
	}
	return gate.result(), checks
}

// adoPolicyCheckDetail builds the per-policy detail, carrying the meaning of
// the policy type: the build link of a build policy, the human wait of a
// reviewer policy, and the reason a comment or work-item policy was rejected.
func (p *ADOProvider) adoPolicyCheckDetail(ev adoPolicyEvaluation, kind adoPolicyKind, state CheckState, projectName string) CheckDetail {
	detail := CheckDetail{Name: adoPolicyName(ev), State: state, Conclusion: ev.Status}
	switch kind {
	case adoPolicyCI:
		detail.URL = p.buildResultsURL(projectName, ev.Context.BuildID.String())
	case adoPolicyReviewer:
		if state != CheckStatePassing {
			detail.State = CheckStatePending
			detail.AwaitingHuman = true
			detail.Summary = "awaiting human approval"
		}
	case adoPolicyCommentResolution:
		if state == CheckStateFailing {
			detail.Summary = "unresolved comment threads"
		}
	case adoPolicyWorkItemLinking:
		if state == CheckStateFailing {
			detail.Summary = "no linked work item"
		}
	}
	return detail
}

// buildResultsURL is the web link of the build a build policy evaluated
// (context.buildId), or "" when the evaluation names no build.
func (p *ADOProvider) buildResultsURL(projectName, buildID string) string {
	if buildID == "" || projectName == "" {
		return ""
	}
	base := strings.TrimSuffix(p.BaseURL, "/")
	if base == "" {
		base = "https://dev.azure.com"
	}
	return base + "/" + url.PathEscape(p.Organization) + "/" + url.PathEscape(projectName) +
		"/_build/results?buildId=" + url.QueryEscape(buildID)
}

// adoEvaluationsMergePolicy decides enqueue versus direct completion from a
// pull request's evaluations: any enabled, blocking evaluation that is not
// approved or notApplicable means auto-complete. decided is false when there
// is no enabled, blocking evaluation at all, which can mean a policy added
// after the pull request has not been evaluated yet (live probe §2); the
// caller then falls back to the configuration scan.
func adoEvaluationsMergePolicy(evals []adoPolicyEvaluation) (policy MergePolicy, decided bool) {
	for _, ev := range evals {
		if !adoEvaluationBlocks(ev) {
			continue
		}
		decided = true
		if !adoEvaluationSatisfied(ev) {
			return MergePolicyMergeQueue, true
		}
	}
	if !decided {
		return "", false
	}
	return MergePolicyDirect, true
}

// adoEvaluationsAwaitHuman reports whether the only unmet enabled, blocking
// evaluations are reviewer policies (design §5.1: the PR stays active with
// auto-complete armed until a person approves, live probe F4).
func adoEvaluationsAwaitHuman(evals []adoPolicyEvaluation) bool {
	waiting := false
	for _, ev := range evals {
		if !adoEvaluationBlocks(ev) || adoEvaluationSatisfied(ev) {
			continue
		}
		if adoPolicyKindOf(ev.Configuration.Type.ID) != adoPolicyReviewer {
			return false
		}
		waiting = true
	}
	return waiting
}

// pullRequestEvaluations reads the policy evaluations of the pull request
// detail describes, resolving the project from the detail itself.
func (p *ADOProvider) pullRequestEvaluations(ctx context.Context, repo RepositoryRef, pullID string, detail adoPullRequestDetail) ([]adoPolicyEvaluation, error) {
	projectName := detail.Repository.Project.Name
	if projectName == "" {
		projectName = p.project(repo)
	}
	return p.policyEvaluations(ctx, projectName, detail.Repository.Project.ID, pullID)
}

// mergePolicyFromEvaluations is DetectMergePolicy's per-PR path.
func (p *ADOProvider) mergePolicyFromEvaluations(ctx context.Context, repo RepositoryRef, pullID string) (MergePolicy, bool, error) {
	detail, err := p.getPullRequestDetail(ctx, repo, pullID)
	if err != nil {
		return "", false, err
	}
	evals, err := p.pullRequestEvaluations(ctx, repo, pullID, detail)
	if err != nil {
		return "", false, err
	}
	policy, decided := adoEvaluationsMergePolicy(evals)
	return policy, decided, nil
}

// autoCompleteAwaitingHuman reports whether an auto-complete-armed pull
// request is held only by reviewer policies. Without a project id the
// evaluations cannot be addressed, so no human wait is claimed.
func (p *ADOProvider) autoCompleteAwaitingHuman(ctx context.Context, repo RepositoryRef, pullID string, detail adoPullRequestDetail) (bool, error) {
	if detail.Repository.Project.ID == "" {
		return false, nil
	}
	evals, err := p.pullRequestEvaluations(ctx, repo, pullID, detail)
	if err != nil {
		return false, err
	}
	return adoEvaluationsAwaitHuman(evals), nil
}

// adoConfigurationGatesRef reports whether an enabled, blocking, non-deleted
// configuration applies to targetRef in repoID. A configuration with no scope
// at all is repo-wide.
func adoConfigurationGatesRef(c adoPolicyConfiguration, repoID, targetRef string) bool {
	if !c.IsEnabled || !c.IsBlocking || c.IsDeleted {
		return false
	}
	if len(c.Settings.Scope) == 0 {
		return true
	}
	for _, scope := range c.Settings.Scope {
		if adoScopeMatches(scope, repoID, targetRef) {
			return true
		}
	}
	return false
}

// adoScopeMatches applies one policy scope to targetRef. An Exact scope
// matches the ref itself; a Prefix scope matches by ref folder, so
// "refs/heads/release/" (or "refs/heads/release") covers
// "refs/heads/release/1.0" but not "refs/heads/released" (live probe F7). A
// scope without a ref name covers every ref of its repository.
func adoScopeMatches(scope adoPolicyScope, repoID, targetRef string) bool {
	if scope.RepositoryID != "" && repoID != "" && !strings.EqualFold(scope.RepositoryID, repoID) {
		return false
	}
	if scope.RefName == "" {
		return true
	}
	if strings.EqualFold(scope.MatchKind, "prefix") {
		folder := strings.TrimSuffix(scope.RefName, "/")
		return targetRef == folder || strings.HasPrefix(targetRef, folder+"/")
	}
	return scope.RefName == targetRef
}

// branchPolicyConfigurations fetches every policy configuration for repo's
// project, following x-ms-continuationtoken across pages. DetectMergePolicy
// filters the result client-side to the scopes that apply to a branch.
func (p *ADOProvider) branchPolicyConfigurations(ctx context.Context, repo RepositoryRef) ([]adoPolicyConfiguration, error) {
	base, err := joinURL(p.BaseURL, p.Organization, p.project(repo), "_apis", "policy", "configurations")
	if err != nil {
		return nil, err
	}
	var all []adoPolicyConfiguration
	seen := map[string]bool{}
	continuation := ""
	for {
		values := url.Values{"api-version": []string{"7.1"}}
		if continuation != "" {
			values.Set("continuationToken", continuation)
		}
		endpoint, err := addQuery(base, values)
		if err != nil {
			return nil, err
		}
		resp, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
		if err != nil {
			return nil, err
		}
		var page adoPolicyConfigurationsResponse
		if err := readJSONResponse(resp, http.MethodGet, endpoint, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
		next := strings.TrimSpace(resp.Header.Get("x-ms-continuationtoken"))
		if next == "" {
			return all, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("ado policy configurations: continuation token repeated")
		}
		seen[next] = true
		continuation = next
	}
}
