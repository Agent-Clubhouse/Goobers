package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// adoChangePageSize bounds a single page of pull-request iteration changes
// fetched by PullRequestFiles.
const adoChangePageSize = 2000

// adoMaxPRDescriptionChars is Azure DevOps' hard limit on a pull request
// description. ADO rejects a POST/PATCH with a longer description with HTTP 400
// (InvalidArgumentValueException: "A description for a pull request must not be
// longer than 4000 characters."). The structured PR body has no overall cap of
// its own, so OpenPullRequest trims it to fit while preserving the run-id footer.
const adoMaxPRDescriptionChars = 4000

// OpenPullRequest opens an Azure DevOps pull request.
func (p *ADOProvider) OpenPullRequest(ctx context.Context, req PullRequestRequest) (PullRequestResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return PullRequestResult{}, err
	}
	head := strings.TrimPrefix(req.Head, "refs/heads/")
	base := strings.TrimPrefix(req.Base, "refs/heads/")
	// Idempotency: a resumed or repassed open-pr stage — or any second call for
	// the same run branch — must converge on the PR it already opened rather
	// than POSTing a duplicate. ADO rejects a second active PR for the same
	// source→target ref anyway, so find-or-create is both correct and the shape
	// the provider-neutral open-pr stage expects (mirroring the GitHub
	// provider's find-or-create).
	if existing, found, err := p.FindPullRequestByBranch(ctx, req.Repository, head, base); err != nil {
		return PullRequestResult{}, err
	} else if found {
		endpoint, err := p.repoURL(req.Repository, "pullrequests", existing.ID)
		if err != nil {
			return PullRequestResult{}, err
		}
		body := map[string]interface{}{
			"title":       req.Title,
			"description": capDescriptionWithFooter(req.Body, req.RunID, adoMaxPRDescriptionChars),
			"isDraft":     req.Draft,
		}
		var out adoPullRequest
		if err := p.do(ctx, http.MethodPatch, endpoint, body, &out); err != nil {
			return PullRequestResult{}, err
		}
		return adoPullRequestResult(out), nil
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests")
	if err != nil {
		return PullRequestResult{}, err
	}
	body := map[string]interface{}{
		"sourceRefName": "refs/heads/" + head,
		"targetRefName": "refs/heads/" + base,
		"title":         req.Title,
		"description":   capDescriptionWithFooter(req.Body, req.RunID, adoMaxPRDescriptionChars),
		"isDraft":       req.Draft,
	}
	var out adoPullRequest
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestResult{}, err
	}
	return adoPullRequestResult(out), nil
}

func adoPullRequestResult(pr adoPullRequest) PullRequestResult {
	prURL := pr.URL
	if pr.Links.Web.Href != "" {
		prURL = pr.Links.Web.Href
	}
	return PullRequestResult{ID: strconv.Itoa(pr.PullRequestID), Number: pr.PullRequestID, URL: prURL}
}

// FindPullRequestByBranch resolves the open Azure DevOps pull request whose
// source branch is head (and, when base is set, whose target is base), so
// issue-close-out can link the run's PR and open-pr can stay idempotent. Reports
// found=false (not an error) when no active PR matches.
func (p *ADOProvider) FindPullRequestByBranch(ctx context.Context, repo RepositoryRef, head, base string) (PullRequestResult, bool, error) {
	if err := requireRepo(repo); err != nil {
		return PullRequestResult{}, false, err
	}
	head = strings.TrimPrefix(head, "refs/heads/")
	if head == "" {
		return PullRequestResult{}, false, fmt.Errorf("head branch is required")
	}
	summaries, err := p.ListPullRequests(ctx, ListPullRequestsRequest{Repository: repo, Base: base, HeadPrefix: head})
	if err != nil {
		return PullRequestResult{}, false, err
	}
	for _, pr := range summaries {
		// ListPullRequests filters HeadPrefix as a prefix; require an exact
		// source-branch match so "run-1" never resolves "run-10".
		if pr.Head == head {
			return PullRequestResult{ID: pr.ID, Number: pr.Number, URL: pr.URL}, true, nil
		}
	}
	return PullRequestResult{}, false, nil
}

// RequestReview requests Azure DevOps reviewers for a pull request.
func (p *ADOProvider) RequestReview(ctx context.Context, req ReviewRequest) error {
	if err := requireRepo(req.Repository); err != nil {
		return err
	}
	if req.PullID == "" {
		return errPullIDRequired
	}
	for _, reviewer := range req.Reviewers {
		endpoint, err := p.repoURL(req.Repository, "pullrequests", req.PullID, "reviewers", reviewer)
		if err != nil {
			return err
		}
		if err := p.do(ctx, http.MethodPut, endpoint, map[string]int{"vote": 0}, nil); err != nil {
			return err
		}
	}
	return nil
}

// adoLabelNames maps ADO PR labels to their bare names.
func adoLabelNames(labels []adoLabel) []string {
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return names
}

// PollPullRequest reports an Azure DevOps pull request's review decision and
// combined check state. The check state is derived from the repository's
// blocking branch-policy evaluations (build validation, status checks, required
// reviewers) so a gate can drive the CI-poll/repass loop against ADO's own
// policy engine — the source of truth for whether a PR is correct (#772).
func (p *ADOProvider) PollPullRequest(ctx context.Context, req PullRequestPollRequest) (PullRequestPollResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return PullRequestPollResult{}, err
	}
	if req.PullID == "" {
		return PullRequestPollResult{}, errPullIDRequired
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests", req.PullID)
	if err != nil {
		return PullRequestPollResult{}, err
	}
	var pr adoPullRequestDetail
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return PullRequestPollResult{}, err
	}
	prURL := pr.URL
	if pr.Links.Web.Href != "" {
		prURL = pr.Links.Web.Href
	}
	result := PullRequestPollResult{
		Number:             pr.PullRequestID,
		Title:              pr.Title,
		Author:             adoIdentityName(pr.CreatedBy),
		RequestedReviewers: adoRequestedReviewerNames(pr.Reviewers),
		State:              adoPullRequestState(pr.Status),
		Merged:             strings.EqualFold(pr.Status, "completed"),
		Draft:              pr.IsDraft,
		HeadBranch:         strings.TrimPrefix(pr.SourceRefName, "refs/heads/"),
		BaseBranch:         strings.TrimPrefix(pr.TargetRefName, "refs/heads/"),
		HeadSHA:            pr.LastMergeSourceCommit.CommitID,
		BaseSHA:            pr.LastMergeTargetCommit.CommitID,
		Body:               pr.Description,
		ReviewDecision:     adoReviewDecision(pr.Reviewers),
		URL:                prURL,
		Integrity:          apiintegrity.Unapproved,
	}
	projectName := pr.Repository.Project.Name
	if projectName == "" {
		projectName = p.project(req.Repository)
	}
	checkState, checks, err := p.pollPullRequestPolicies(ctx, projectName, pr.Repository.Project.ID, req.PullID, stringSet(req.HumanPolicyConfigurationIDs))
	if err != nil {
		return PullRequestPollResult{}, err
	}
	result.CheckState = checkState
	result.Checks = checks
	return result, nil
}

// policyEvaluations fetches the branch-policy evaluations for a pull request via
// the ADO Policy Evaluations API.
func (p *ADOProvider) policyEvaluations(ctx context.Context, projectName, projectID, pullID string) ([]adoPolicyEvaluation, error) {
	if projectID == "" {
		return nil, fmt.Errorf("ado pull request %s: policy evaluation requires the project id", pullID)
	}
	endpoint, err := joinURL(p.BaseURL, p.Organization, projectName, "_apis", "policy", "evaluations")
	if err != nil {
		return nil, err
	}
	artifactID := fmt.Sprintf("vstfs:///CodeReview/CodeReviewId/%s/%s", projectID, pullID)
	endpoint, err = addQuery(endpoint, url.Values{
		"scopeId":    []string{projectID},
		"artifactId": []string{artifactID},
		// The Policy Evaluations API is still published under preview: ADO
		// rejects a plain "7.1" with VssInvalidPreviewVersionException and
		// requires the -preview suffix. (git/pullrequests and wit use "7.1".)
		"api-version": []string{"7.1-preview.1"},
	})
	if err != nil {
		return nil, err
	}
	var resp adoPolicyEvaluationsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Value, nil
}

// pollPullRequestPolicies reads the blocking branch-policy evaluations for a
// pull request and reduces them to a single provider-neutral check state plus
// per-policy detail.
//
// The returned CheckState gates on the set of policies the agent loop can act
// on — every required blocking policy EXCEPT those whose configuration id is in
// humanOnly. Human/merge-time policies (merge strategy, proof-of-presence,
// required/minimum reviewers, comment resolution) can never be satisfied by
// re-implementing; reducing on them would peg the state to failing forever and
// starve the fix loop, so a loop declares their configuration ids as human-only
// and they are recorded in the detail list for transparency but never drive the
// gate. When no gating policy has concluded green yet (none applies, or one is
// still queued/running) the state is pending — fail-closed: correctness is
// unproven until a gating policy passes.
func (p *ADOProvider) pollPullRequestPolicies(ctx context.Context, projectName, projectID, pullID string, humanOnly map[string]bool) (CheckState, []CheckDetail, error) {
	evals, err := p.policyEvaluations(ctx, projectName, projectID, pullID)
	if err != nil {
		return "", nil, err
	}
	checks := make([]CheckDetail, 0, len(evals))
	gateFailing, gatePending, gatePassing, sawGate := false, false, false, false
	for _, ev := range evals {
		if !ev.Configuration.IsEnabled || !ev.Configuration.IsBlocking {
			continue
		}
		state := adoPolicyCheckState(ev.Status)
		if state == "" {
			continue
		}
		checks = append(checks, CheckDetail{
			Name:       adoPolicyName(ev),
			State:      state,
			Conclusion: ev.Status,
		})
		if humanOnly[ev.Configuration.ID.String()] {
			continue
		}
		sawGate = true
		switch state {
		case CheckStateFailing:
			gateFailing = true
		case CheckStatePending:
			gatePending = true
		case CheckStatePassing:
			gatePassing = true
		}
	}
	switch {
	case gateFailing:
		return CheckStateFailing, checks, nil
	case sawGate && gatePassing && !gatePending:
		return CheckStatePassing, checks, nil
	default:
		return CheckStatePending, checks, nil
	}
}

// ClosePullRequest abandons an Azure DevOps pull request — the ADO equivalent of
// closing a pull request unmerged. A completed pull request is reported as
// merged (#772). Only used for terminal-failure cleanup; the happy path leaves
// completion to a human.
func (p *ADOProvider) ClosePullRequest(ctx context.Context, req ClosePullRequestRequest) (ClosePullRequestResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return ClosePullRequestResult{}, err
	}
	if req.PullID == "" {
		return ClosePullRequestResult{}, errPullIDRequired
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests", req.PullID)
	if err != nil {
		return ClosePullRequestResult{}, err
	}
	var out adoPullRequest
	if err := p.do(ctx, http.MethodPatch, endpoint, map[string]interface{}{"status": "abandoned"}, &out); err != nil {
		return ClosePullRequestResult{}, err
	}
	number := out.PullRequestID
	if number == 0 {
		number, _ = strconv.Atoi(req.PullID)
	}
	return ClosePullRequestResult{
		Number: number,
		Merged: strings.EqualFold(out.Status, "completed"),
		State:  adoPullRequestState(out.Status),
	}, nil
}

// PublishPullRequestStatus posts an Azure DevOps pull-request status so a
// status-check branch policy can gate on goobers-supplied evidence — a reviewer
// verdict or local-CI result — making ADO's policy engine the source of truth
// for PR correctness (#772).
func (p *ADOProvider) PublishPullRequestStatus(ctx context.Context, req PullRequestStatusRequest) (PullRequestStatusResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return PullRequestStatusResult{}, err
	}
	if req.PullID == "" {
		return PullRequestStatusResult{}, errPullIDRequired
	}
	if req.Name == "" {
		return PullRequestStatusResult{}, fmt.Errorf("status name is required")
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests", req.PullID, "statuses")
	if err != nil {
		return PullRequestStatusResult{}, err
	}
	genre := req.Genre
	if genre == "" {
		genre = adoDefaultStatusGenre
	}
	body := map[string]interface{}{
		"state":       adoStatusState(req.State),
		"description": req.Description,
		"context": map[string]string{
			"genre": genre,
			"name":  req.Name,
		},
	}
	if req.TargetURL != "" {
		body["targetUrl"] = req.TargetURL
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestStatusResult{}, err
	}
	return PullRequestStatusResult{ID: out.ID}, nil
}

// ListPullRequests lists active Azure DevOps pull requests matching the
// provider-neutral base and head-prefix filters.
func (p *ADOProvider) ListPullRequests(ctx context.Context, req ListPullRequestsRequest) ([]PullRequestSummary, error) {
	if err := requireRepo(req.Repository); err != nil {
		return nil, err
	}
	if req.Assignee != "" {
		return nil, ErrUnsupported{Provider: ProviderADO, Capability: CapPRQueryAssignee}
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests")
	if err != nil {
		return nil, err
	}
	values := url.Values{
		"searchCriteria.status":       []string{"active"},
		"searchCriteria.includeLinks": []string{"true"},
		// ADO omits PR labels from the list response unless explicitly asked;
		// without this the label-based remediation selector sees nothing
		// (verified against the live API).
		"includeLabels": []string{"true"},
		"$top":          []string{strconv.Itoa(adoPullRequestPageSize)},
	}
	if req.Base != "" {
		values.Set("searchCriteria.targetRefName", "refs/heads/"+strings.TrimPrefix(req.Base, "refs/heads/"))
	}

	var prs []adoPullRequest
	for skip := 0; ; skip += adoPullRequestPageSize {
		values.Set("$skip", strconv.Itoa(skip))
		pageEndpoint, err := addQuery(endpoint, values)
		if err != nil {
			return nil, err
		}
		var page adoPullRequestsResponse
		if err := p.do(ctx, http.MethodGet, pageEndpoint, nil, &page); err != nil {
			return nil, err
		}
		prs = append(prs, page.Value...)
		if len(page.Value) < adoPullRequestPageSize {
			break
		}
	}

	headPrefix := strings.TrimPrefix(req.HeadPrefix, "refs/heads/")
	out := make([]PullRequestSummary, 0, len(prs))
	for _, pr := range prs {
		head := strings.TrimPrefix(pr.SourceRefName, "refs/heads/")
		if headPrefix != "" && !strings.HasPrefix(head, headPrefix) {
			continue
		}
		author := adoIdentityName(pr.CreatedBy)
		requestedReviewers := adoRequestedReviewerNames(pr.Reviewers)
		if !req.MatchesIdentityFields(author, nil, requestedReviewers) {
			continue
		}
		labels := adoLabelNames(pr.Labels)
		prURL := pr.URL
		if pr.Links.Web.Href != "" {
			prURL = pr.Links.Web.Href
		}
		out = append(out, PullRequestSummary{
			ID:                 strconv.Itoa(pr.PullRequestID),
			Number:             pr.PullRequestID,
			URL:                prURL,
			Author:             author,
			RequestedReviewers: requestedReviewers,
			Head:               head,
			Base:               strings.TrimPrefix(pr.TargetRefName, "refs/heads/"),
			HeadSHA:            pr.LastMergeSourceCommit.CommitID,
			BaseSHA:            pr.LastMergeTargetCommit.CommitID,
			Draft:              pr.IsDraft,
			Labels:             labels,
			CheckState:         CheckStatePending,
			UpdatedAt:          pr.CreationDate,
			Integrity:          apiintegrity.Unapproved,
		})
	}
	return out, nil
}

// PullRequestFiles lists the cumulative changes in the latest pull request
// iteration, relative to the common source/target commit.
func (p *ADOProvider) PullRequestFiles(ctx context.Context, repo RepositoryRef, pullID string) ([]ChangedFile, error) {
	if err := requireRepo(repo); err != nil {
		return nil, err
	}
	if pullID == "" {
		return nil, errPullIDRequired
	}
	iterationsEndpoint, err := p.repoURL(repo, "pullrequests", pullID, "iterations")
	if err != nil {
		return nil, err
	}
	var iterations adoPullRequestIterationsResponse
	if err := p.do(ctx, http.MethodGet, iterationsEndpoint, nil, &iterations); err != nil {
		return nil, err
	}
	latestIteration := 0
	for _, iteration := range iterations.Value {
		if iteration.ID > latestIteration {
			latestIteration = iteration.ID
		}
	}
	if latestIteration == 0 {
		return nil, fmt.Errorf("ado pull request %s returned no iterations", pullID)
	}

	changesEndpoint, err := p.repoURL(repo, "pullrequests", pullID, "iterations", strconv.Itoa(latestIteration), "changes")
	if err != nil {
		return nil, err
	}
	files := make([]ChangedFile, 0)
	top, skip := adoChangePageSize, 0
	for {
		pageEndpoint, err := addQuery(changesEndpoint, url.Values{
			"$top":  []string{strconv.Itoa(top)},
			"$skip": []string{strconv.Itoa(skip)},
		})
		if err != nil {
			return nil, err
		}
		var page adoPullRequestIterationChanges
		if err := p.do(ctx, http.MethodGet, pageEndpoint, nil, &page); err != nil {
			return nil, err
		}
		for _, change := range page.ChangeEntries {
			if change.Item.Path == "" {
				return nil, fmt.Errorf("ado pull request %s iteration %d returned a change without a path", pullID, latestIteration)
			}
			files = append(files, ChangedFile{
				Path:      strings.TrimPrefix(change.Item.Path, "/"),
				Status:    adoChangedFileStatus(change.ChangeType),
				Integrity: apiintegrity.Unapproved,
			})
		}
		if page.NextSkip == 0 {
			break
		}
		if page.NextSkip <= skip {
			return nil, fmt.Errorf("ado pull request %s returned invalid change pagination offset %d", pullID, page.NextSkip)
		}
		skip = page.NextSkip
		if page.NextTop > 0 {
			top = page.NextTop
		}
	}
	return files, nil
}

type adoPullRequest struct {
	PullRequestID         int           `json:"pullRequestId"`
	URL                   string        `json:"url"`
	Status                string        `json:"status"`
	Title                 string        `json:"title"`
	CreatedBy             adoIdentity   `json:"createdBy"`
	Reviewers             []adoReviewer `json:"reviewers"`
	CreationDate          time.Time     `json:"creationDate"`
	SourceRefName         string        `json:"sourceRefName"`
	TargetRefName         string        `json:"targetRefName"`
	IsDraft               bool          `json:"isDraft"`
	Labels                []adoLabel    `json:"labels"`
	LastMergeSourceCommit adoCommitRef  `json:"lastMergeSourceCommit"`
	LastMergeTargetCommit adoCommitRef  `json:"lastMergeTargetCommit"`
	Links                 adoPRLinks    `json:"_links"`
}

type adoPullRequestsResponse struct {
	Value []adoPullRequest `json:"value"`
}

// adoPullRequestDetail extends adoPullRequest with the fields a single-PR GET
// returns that a list does not: description, reviewers (for review-decision
// mapping), and the repository/project identity needed to key policy
// evaluations.
type adoPullRequestDetail struct {
	adoPullRequest
	Description string        `json:"description"`
	Repository  adoRepository `json:"repository"`
	// MergeStatus/MergeID/LastMergeCommit/CompletionOptions/
	// AutoCompleteSetBy back the landing surfaces (CONF-3 #2076, design
	// doc §4): MergeStatus is the completion job's own outcome
	// ("succeeded"/"conflicts"/"failure"/"rejectedByPolicy"/"queued"/
	// "notSet"), LastMergeCommit is the landed commit once MergeStatus is
	// "succeeded", and AutoCompleteSetBy is present iff auto-complete is
	// currently armed (nil once ADO clears it — completion or eviction).
	MergeStatus       string                `json:"mergeStatus,omitempty"`
	MergeID           string                `json:"mergeId,omitempty"`
	LastMergeCommit   adoCommitRef          `json:"lastMergeCommit"`
	CompletionOptions *adoCompletionOptions `json:"completionOptions,omitempty"`
	AutoCompleteSetBy *adoIdentity          `json:"autoCompleteSetBy,omitempty"`
}

type adoReviewer struct {
	Vote        int    `json:"vote"`
	UniqueName  string `json:"uniqueName"`
	DisplayName string `json:"displayName"`
	IsRequired  bool   `json:"isRequired"`
}

func adoIdentityName(identity adoIdentity) string {
	if identity.UniqueName != "" {
		return identity.UniqueName
	}
	return identity.DisplayName
}

func adoRequestedReviewerNames(reviewers []adoReviewer) []string {
	names := make([]string, 0, len(reviewers))
	for _, reviewer := range reviewers {
		if reviewer.Vote == 0 {
			names = append(names, adoIdentityName(adoIdentity{
				UniqueName:  reviewer.UniqueName,
				DisplayName: reviewer.DisplayName,
			}))
		}
	}
	return names
}

type adoRepository struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Project adoProject `json:"project"`
}

type adoProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type adoPolicyEvaluationsResponse struct {
	Value []adoPolicyEvaluation `json:"value"`
}

type adoPolicyEvaluation struct {
	Status        string `json:"status"`
	Configuration struct {
		// ID is the branch-policy configuration id — the stable, generic
		// identity a loop uses to declare a policy human-only (see
		// PullRequestPollRequest.HumanPolicyConfigurationIDs). It is provider
		// identity, not policy detail.
		ID         json.Number `json:"id"`
		IsEnabled  bool        `json:"isEnabled"`
		IsBlocking bool        `json:"isBlocking"`
		Type       struct {
			DisplayName string `json:"displayName"`
		} `json:"type"`
	} `json:"configuration"`
}

// stringSet builds a lookup set from a slice, ignoring empty entries.
func stringSet(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]bool, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			set[v] = true
		}
	}
	return set
}

const adoDefaultStatusGenre = "goobers"

// adoPullRequestState maps an Azure DevOps pull-request status to the
// provider-neutral open/merged/closed vocabulary the PR-lifecycle gates read.
func adoPullRequestState(status string) string {
	switch strings.ToLower(status) {
	case "completed":
		return "merged"
	case "abandoned":
		return "closed"
	default:
		return "open"
	}
}

// adoPolicyCheckState maps an Azure DevOps policy-evaluation status to a
// provider-neutral check state. An empty return means the evaluation is not
// applicable and should be ignored.
func adoPolicyCheckState(status string) CheckState {
	switch strings.ToLower(status) {
	case "approved":
		return CheckStatePassing
	case "rejected":
		return CheckStateFailing
	case "queued", "running":
		return CheckStatePending
	default:
		return ""
	}
}

func adoPolicyName(ev adoPolicyEvaluation) string {
	if ev.Configuration.Type.DisplayName != "" {
		return ev.Configuration.Type.DisplayName
	}
	return "branch policy"
}

// adoReviewDecision reduces Azure DevOps reviewer votes to a provider-neutral
// review decision. A negative vote (waiting/reject) requests changes; a vote of
// approved-with/approved (>= 5) approves; otherwise the review is pending.
func adoReviewDecision(reviewers []adoReviewer) ReviewDecision {
	approved := false
	for _, r := range reviewers {
		switch {
		case r.Vote < 0:
			return ReviewDecisionChangesRequested
		case r.Vote >= 5:
			approved = true
		}
	}
	if approved {
		return ReviewDecisionApproved
	}
	return ReviewDecisionPending
}

// adoStatusState maps a provider-neutral check state to an Azure DevOps
// pull-request status state string.
func adoStatusState(state CheckState) string {
	switch state {
	case CheckStatePassing:
		return "succeeded"
	case CheckStateFailing:
		return "failed"
	default:
		return "pending"
	}
}

type adoLabel struct {
	Name string `json:"name"`
}

type adoCommitRef struct {
	CommitID string `json:"commitId"`
}

type adoPRLinks struct {
	Web struct {
		Href string `json:"href"`
	} `json:"web"`
}

type adoPullRequestIterationsResponse struct {
	Value []struct {
		ID int `json:"id"`
	} `json:"value"`
}

type adoPullRequestIterationChanges struct {
	ChangeEntries []struct {
		ChangeType string `json:"changeType"`
		Item       struct {
			Path string `json:"path"`
		} `json:"item"`
	} `json:"changeEntries"`
	NextSkip int `json:"nextSkip"`
	NextTop  int `json:"nextTop"`
}

func adoChangedFileStatus(changeType string) string {
	switch strings.ToLower(changeType) {
	case "add":
		return "added"
	case "delete":
		return "removed"
	case "rename", "sourcerename", "targetrename":
		return "renamed"
	default:
		return "modified"
	}
}
