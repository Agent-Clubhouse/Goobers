package providers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/fieldpredicate"
)

const (
	adoPullRequestPageSize         = 100
	adoPullRequestDescriptionLimit = 4000
	adoChangePageSize              = 2000
	adoCommentPageSize             = 200
	adoClaimRetries                = 4
	adoMaxTagLength                = 400
	adoClaimTagPrefix              = "goobers:claim-run:"
)

// ADOProvider implements repo, backlog, and trigger operations for Azure DevOps.
type ADOProvider struct {
	Organization string
	Project      string
	BaseURL      string
	Token        string
	Username     string
	Client       HTTPClient
	Runner       CommandRunner

	credentialSource ADOCredentialSource
	secretRegistrar  SecretRegistrar
	recorder         MutationRecorder
	rateObserver     RateLimitObserver
	maxRetries       int
	maxRateLimitWait time.Duration
	now              func() time.Time
	sleep            func(context.Context, time.Duration) error
	jitter           func(time.Duration) time.Duration

	stateMu         sync.RWMutex
	stateCategories map[string][]adoWorkItemState
}

// NewADOProvider constructs an Azure DevOps provider with optional overrides.
func NewADOProvider(organization, project, token string, opts ...func(*ADOProvider)) *ADOProvider {
	p := &ADOProvider{
		Organization:     organization,
		Project:          project,
		BaseURL:          "https://dev.azure.com",
		Token:            token,
		Username:         "goobers",
		maxRetries:       defaultRateLimitRetries,
		maxRateLimitWait: defaultRateLimitMaxWait,
		now:              time.Now,
		sleep:            contextSleep,
		jitter:           randomJitter,
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.credentialSource == nil && p.Token != "" {
		p.credentialSource = NewADOPATCredentialSource(p.Username, p.Token)
	}
	p.Client = httpClientOrDefault(p.Client)
	p.Runner = commandRunnerOrDefault(p.Runner)
	if p.now == nil {
		p.now = time.Now
	}
	if p.sleep == nil {
		p.sleep = contextSleep
	}
	if p.jitter == nil {
		p.jitter = randomJitter
	}
	if p.secretRegistrar != nil && p.Token != "" {
		p.secretRegistrar.Register([]byte(p.Token))
		p.secretRegistrar.Register([]byte(strings.TrimPrefix(basicAuth(p.Username, p.Token), "Basic ")))
	}
	return p
}

// WithADOCredentialSource configures PAT or Entra authentication independently
// of the legacy fixed-token constructor argument. An explicit source wins.
func WithADOCredentialSource(source ADOCredentialSource) func(*ADOProvider) {
	return func(p *ADOProvider) { p.credentialSource = source }
}

// SecretRegistrar receives provider credential forms that must be scrubbed.
type SecretRegistrar interface {
	Register(secret []byte)
}

// WithADOSecretRegistrar registers the raw token and encoded Basic credential
// with the scrubber used by journals and telemetry.
func WithADOSecretRegistrar(registrar SecretRegistrar) func(*ADOProvider) {
	return func(p *ADOProvider) { p.secretRegistrar = registrar }
}

// WithADOMutationRecorder records Azure DevOps mutations for the run journal.
func WithADOMutationRecorder(recorder MutationRecorder) func(*ADOProvider) {
	return func(p *ADOProvider) { p.recorder = recorder }
}

// WithADORateLimitObserver receives Azure DevOps rate-limit decisions.
func WithADORateLimitObserver(observer RateLimitObserver) func(*ADOProvider) {
	return func(p *ADOProvider) { p.rateObserver = observer }
}

// WithADOMaxRateLimitRetries overrides the retry count for rate-limited requests.
func WithADOMaxRateLimitRetries(n int) func(*ADOProvider) {
	return func(p *ADOProvider) { p.maxRetries = n }
}

// Kind returns the Azure DevOps provider kind.
func (p *ADOProvider) Kind() ProviderKind {
	return ProviderADO
}

// CloneRepository clones an Azure DevOps repository to a local destination.
func (p *ADOProvider) CloneRepository(ctx context.Context, req CloneRequest) (CloneResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return CloneResult{}, err
	}
	if req.Destination == "" {
		return CloneResult{}, fmt.Errorf("destination is required")
	}
	cloneURL := p.repositoryURL(req.Repository)
	args := []string{"clone"}
	if req.Branch != "" {
		args = append(args, "--branch", req.Branch)
	}
	args = append(args, cloneURL, req.Destination)
	var (
		out []byte
		err error
	)
	if p.credentialSource == nil {
		out, err = p.Runner.Run(ctx, "git", args...)
	} else {
		runner, ok := p.Runner.(environmentCommandRunner)
		if !ok {
			return CloneResult{}, fmt.Errorf("authenticated ADO clone requires an environment-capable command runner")
		}
		header, authErr := p.authorizationHeader(ctx)
		if authErr != nil {
			return CloneResult{}, fmt.Errorf("resolve ADO clone credential: %w", authErr)
		}
		out, err = runner.RunWithEnv(ctx, adoGitAuthEnv(header, cloneURL), "git", args...)
	}
	if err != nil {
		return CloneResult{}, fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return CloneResult{Path: req.Destination, URL: cloneURL}, nil
}

// RepositoryReachable verifies authenticated Git access without cloning.
func (p *ADOProvider) RepositoryReachable(ctx context.Context, repo RepositoryRef) error {
	if err := requireRepo(repo); err != nil {
		return err
	}
	args := []string{
		"-c", "credential.helper=",
		"-c", "credential.interactive=never",
		"ls-remote", "--heads", p.repositoryURL(repo),
	}
	if p.credentialSource == nil {
		if _, err := p.Runner.Run(ctx, "git", args...); err != nil {
			return fmt.Errorf("git ls-remote: %w", err)
		}
		return nil
	}
	runner, ok := p.Runner.(environmentCommandRunner)
	if !ok {
		return fmt.Errorf("authenticated ADO repository preflight requires an environment-capable command runner")
	}
	header, err := p.authorizationHeader(ctx)
	if err != nil {
		return fmt.Errorf("resolve ADO repository credential: %w", err)
	}
	if _, err := runner.RunWithEnv(ctx, adoGitAuthEnv(header, p.repositoryURL(repo)), "git", args...); err != nil {
		return fmt.Errorf("git ls-remote: %w", err)
	}
	return nil
}

func (p *ADOProvider) repositoryURL(repo RepositoryRef) string {
	if repo.URL != "" {
		return repo.URL
	}
	return fmt.Sprintf("%s/%s/%s/_git/%s", strings.TrimRight(p.BaseURL, "/"), url.PathEscape(p.Organization), url.PathEscape(p.project(repo)), url.PathEscape(repo.Name))
}

// CreateBranch creates an Azure DevOps branch ref.
func (p *ADOProvider) CreateBranch(ctx context.Context, req BranchRequest) (BranchResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return BranchResult{}, err
	}
	if req.Name == "" {
		return BranchResult{}, fmt.Errorf("branch name is required")
	}
	if req.BaseSHA == "" {
		return BranchResult{}, fmt.Errorf("base sha is required for ado branch creation")
	}
	endpoint, err := p.repoURL(req.Repository, "refs")
	if err != nil {
		return BranchResult{}, err
	}
	body := []map[string]string{{
		"name":        "refs/heads/" + req.Name,
		"oldObjectId": "0000000000000000000000000000000000000000",
		"newObjectId": req.BaseSHA,
	}}
	var out adoRefsResponse
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return BranchResult{}, err
	}
	if len(out.Value) == 0 {
		return BranchResult{}, fmt.Errorf("ado branch creation returned no refs")
	}
	return BranchResult{Name: strings.TrimPrefix(out.Value[0].Name, "refs/heads/"), SHA: out.Value[0].ObjectID, URL: out.Value[0].URL}, nil
}

// DeleteBranch is part of the provider contract; ADO parity lands in V1.
func (p *ADOProvider) DeleteBranch(context.Context, DeleteBranchRequest) (DeleteBranchResult, error) {
	return DeleteBranchResult{}, fmt.Errorf("ado: branch deletion lands in V1 parity (BL-033)")
}

// Commit writes file changes to an Azure DevOps branch.
func (p *ADOProvider) Commit(ctx context.Context, req CommitRequest) (CommitResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return CommitResult{}, err
	}
	if req.Branch == "" {
		return CommitResult{}, fmt.Errorf("branch is required")
	}
	if req.Message == "" {
		return CommitResult{}, fmt.Errorf("message is required")
	}
	if len(req.Files) == 0 {
		return CommitResult{}, fmt.Errorf("at least one file is required")
	}
	changes := make([]adoChange, 0, len(req.Files))
	for _, file := range req.Files {
		if file.Path == "" {
			return CommitResult{}, fmt.Errorf("file path is required")
		}
		exists, err := p.pathExists(ctx, req.Repository, req.Branch, file.Path)
		if err != nil {
			return CommitResult{}, err
		}
		changeType, err := normalizeCommitChange(file.ChangeType, exists)
		if err != nil {
			return CommitResult{}, fmt.Errorf("%s: %w", file.Path, err)
		}
		change := adoChange{
			ChangeType: string(changeType),
			Item:       map[string]string{"path": "/" + strings.TrimPrefix(file.Path, "/")},
		}
		if changeType != CommitChangeDelete {
			change.NewContent = &adoNewContent{Content: file.Content, ContentType: "rawtext"}
		}
		changes = append(changes, change)
	}
	endpoint, err := p.repoURL(req.Repository, "pushes")
	if err != nil {
		return CommitResult{}, err
	}
	oldObjectID := req.BaseSHA
	if oldObjectID == "" {
		var err error
		oldObjectID, err = p.branchSHA(ctx, req.Repository, req.Branch)
		if err != nil {
			return CommitResult{}, err
		}
	}
	body := adoPushRequest{
		RefUpdates: []adoRefUpdate{{Name: "refs/heads/" + req.Branch, OldObjectID: oldObjectID}},
		Commits:    []adoCommit{{Comment: req.Message, Changes: changes}},
	}
	var out adoPushResponse
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return CommitResult{}, err
	}
	commitID := ""
	if len(out.Commits) > 0 {
		commitID = out.Commits[0].CommitID
	}
	return CommitResult{SHA: commitID, URL: out.URL}, nil
}

// OpenPullRequest opens or updates the active Azure DevOps pull request for the source and target.
func (p *ADOProvider) OpenPullRequest(ctx context.Context, req PullRequestRequest) (PullRequestResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return PullRequestResult{}, err
	}
	sourceRefName := "refs/heads/" + strings.TrimPrefix(req.Head, "refs/heads/")
	targetRefName := "refs/heads/" + strings.TrimPrefix(req.Base, "refs/heads/")
	existing, ok, err := p.findActivePullRequest(ctx, req.Repository, sourceRefName, targetRefName)
	if err != nil {
		return PullRequestResult{}, err
	}
	if ok {
		return p.updatePullRequest(ctx, req, existing)
	}

	endpoint, err := p.repoURL(req.Repository, "pullrequests")
	if err != nil {
		return PullRequestResult{}, err
	}
	body := map[string]interface{}{
		"sourceRefName": sourceRefName,
		"targetRefName": targetRefName,
		"title":         req.Title,
		"description":   adoPullRequestDescription(req.Body, req.RunID),
		"isDraft":       req.Draft,
	}
	var out adoPullRequest
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestResult{}, err
	}
	p.recordPullRequestMutation(ctx, req.Repository, out, "open", req.RunID, map[string]FieldDigest{
		"title":       {After: digestString(req.Title)},
		"description": {After: digestString(body["description"].(string))},
		"state":       {After: digestString("open")},
	})
	return adoPullRequestResult(out), nil
}

func (p *ADOProvider) findActivePullRequest(ctx context.Context, repo RepositoryRef, sourceRefName, targetRefName string) (adoPullRequest, bool, error) {
	endpoint, err := p.repoURL(repo, "pullrequests")
	if err != nil {
		return adoPullRequest{}, false, err
	}
	endpoint, err = addQuery(endpoint, url.Values{
		"searchCriteria.status":        []string{"active"},
		"searchCriteria.sourceRefName": []string{sourceRefName},
		"searchCriteria.targetRefName": []string{targetRefName},
		"searchCriteria.includeLinks":  []string{"true"},
		"$top":                         []string{"1"},
	})
	if err != nil {
		return adoPullRequest{}, false, err
	}
	var out adoPullRequestsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return adoPullRequest{}, false, err
	}
	if len(out.Value) == 0 {
		return adoPullRequest{}, false, nil
	}
	return out.Value[0], true, nil
}

func (p *ADOProvider) updatePullRequest(ctx context.Context, req PullRequestRequest, existing adoPullRequest) (PullRequestResult, error) {
	endpoint, err := p.repoURL(req.Repository, "pullrequests", strconv.Itoa(existing.PullRequestID))
	if err != nil {
		return PullRequestResult{}, err
	}
	var current adoPullRequest
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &current); err != nil {
		return PullRequestResult{}, err
	}
	body := map[string]interface{}{
		"title":       req.Title,
		"description": adoPullRequestDescription(req.Body, req.RunID),
		"isDraft":     req.Draft,
	}
	var out adoPullRequest
	if err := p.do(ctx, http.MethodPatch, endpoint, body, &out); err != nil {
		return PullRequestResult{}, err
	}
	if out.PullRequestID == 0 {
		out.PullRequestID = existing.PullRequestID
	}
	if out.URL == "" {
		out.URL = current.URL
		if out.URL == "" {
			out.URL = existing.URL
		}
	}
	if out.Links.Web.Href == "" {
		out.Links.Web.Href = current.Links.Web.Href
		if out.Links.Web.Href == "" {
			out.Links.Web.Href = existing.Links.Web.Href
		}
	}
	p.recordPullRequestMutation(ctx, req.Repository, out, "update", req.RunID, map[string]FieldDigest{
		"title": {
			Before: digestString(current.Title),
			After:  digestString(req.Title),
		},
		"description": {
			Before: digestString(current.Description),
			After:  digestString(body["description"].(string)),
		},
	})
	return adoPullRequestResult(out), nil
}

func adoPullRequestDescription(body, runID string) string {
	description := withRunIDFooter(body, runID)
	if utf16CodeUnits(description) <= adoPullRequestDescriptionLimit {
		return description
	}
	if runID == "" {
		return truncateUTF16(description, adoPullRequestDescriptionLimit)
	}
	suffix := "\n\n---\n" + runFooter(runID)
	suffixUnits := utf16CodeUnits(suffix)
	if suffixUnits >= adoPullRequestDescriptionLimit {
		return truncateUTF16(suffix, adoPullRequestDescriptionLimit)
	}
	return truncateUTF16(body, adoPullRequestDescriptionLimit-suffixUnits) + suffix
}

func utf16CodeUnits(value string) int {
	units := 0
	for _, r := range value {
		units++
		if r > 0xffff {
			units++
		}
	}
	return units
}

func truncateUTF16(value string, maxUnits int) string {
	if maxUnits <= 0 {
		return ""
	}
	units := 0
	for index, r := range value {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > maxUnits {
			return value[:index]
		}
		units += width
	}
	return value
}

func adoPullRequestResult(pr adoPullRequest) PullRequestResult {
	prURL := pr.URL
	if pr.Links.Web.Href != "" {
		prURL = pr.Links.Web.Href
	}
	return PullRequestResult{ID: strconv.Itoa(pr.PullRequestID), Number: pr.PullRequestID, URL: prURL}
}

// RequestReview requests Azure DevOps reviewers for a pull request.
func (p *ADOProvider) RequestReview(ctx context.Context, req ReviewRequest) error {
	if err := requireRepo(req.Repository); err != nil {
		return err
	}
	if req.PullID == "" {
		return fmt.Errorf("pull id is required")
	}
	for _, reviewer := range req.Reviewers {
		endpoint, err := p.repoURL(req.Repository, "pullrequests", req.PullID, "reviewers", reviewer)
		if err != nil {
			return err
		}
		if err := p.do(ctx, http.MethodPut, endpoint, map[string]int{"vote": 0}, nil); err != nil {
			return err
		}
		p.recordPullRequestMutation(ctx, req.Repository, adoPullRequest{PullRequestID: pullRequestNumber(req.PullID)}, "review", "", map[string]FieldDigest{
			"reviewer": {After: digestString(reviewer)},
		})
	}
	return nil
}

// PollPullRequest reports Azure DevOps review votes and builds in the provider-neutral result.
func (p *ADOProvider) PollPullRequest(ctx context.Context, req PullRequestPollRequest) (PullRequestPollResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return PullRequestPollResult{}, err
	}
	if req.PullID == "" {
		return PullRequestPollResult{}, fmt.Errorf("pull id is required")
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests", req.PullID)
	if err != nil {
		return PullRequestPollResult{}, err
	}
	var pr adoPullRequest
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return PullRequestPollResult{}, err
	}

	repositoryID := pr.Repository.ID
	if repositoryID == "" {
		repositoryID = req.Repository.ID
	}
	if repositoryID == "" {
		repositoryID = req.Repository.Name
	}
	checkState, checks, err := p.pullRequestBuildState(
		ctx,
		req.Repository,
		req.PullID,
		repositoryID,
		pr.LastMergeSourceCommit.CommitID,
	)
	if err != nil {
		return PullRequestPollResult{}, err
	}
	var comments []PullRequestComment
	if req.CommentsSince != nil {
		comments, err = p.pullRequestCommentsSince(ctx, req.Repository, req.PullID, *req.CommentsSince)
		if err != nil {
			return PullRequestPollResult{}, err
		}
	}
	reviewDecision, requestedChanges := adoReviewDecision(pr.Reviewers)
	state, merged := adoPullRequestPollState(pr.Status)
	prURL := pr.URL
	if pr.Links.Web.Href != "" {
		prURL = pr.Links.Web.Href
	}
	headRepository := req.Repository
	headRepository.Provider = ProviderADO
	if headRepository.Owner == "" {
		headRepository.Owner = p.Organization
	}
	if headRepository.Project == "" {
		headRepository.Project = p.project(req.Repository)
	}

	var mergedAt *time.Time
	if merged {
		mergedAt = pr.ClosedDate
	}
	return PullRequestPollResult{
		Number:           pr.PullRequestID,
		Title:            pr.Title,
		State:            state,
		Merged:           merged,
		MergedAt:         mergedAt,
		Mergeable:        adoMergeable(pr.MergeStatus),
		MergeableState:   strings.ToLower(pr.MergeStatus),
		Draft:            pr.IsDraft,
		HeadBranch:       strings.TrimPrefix(pr.SourceRefName, "refs/heads/"),
		HeadRepository:   &headRepository,
		HeadSHA:          pr.LastMergeSourceCommit.CommitID,
		BaseSHA:          pr.LastMergeTargetCommit.CommitID,
		BaseBranch:       strings.TrimPrefix(pr.TargetRefName, "refs/heads/"),
		Body:             pr.Description,
		ReviewDecision:   reviewDecision,
		RequestedChanges: requestedChanges,
		CheckState:       checkState,
		Checks:           checks,
		CommentsSince:    comments,
		URL:              prURL,
	}, nil
}

// ClosePullRequest abandons an active PR and reports completed Azure DevOps PRs as merged.
func (p *ADOProvider) ClosePullRequest(ctx context.Context, req ClosePullRequestRequest) (ClosePullRequestResult, error) {
	if err := requireRepo(req.Repository); err != nil {
		return ClosePullRequestResult{}, err
	}
	if req.PullID == "" {
		return ClosePullRequestResult{}, fmt.Errorf("pull id is required")
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests", req.PullID)
	if err != nil {
		return ClosePullRequestResult{}, err
	}
	var pr adoPullRequest
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return ClosePullRequestResult{}, err
	}

	state, merged := adoPullRequestState(pr.Status)
	switch strings.ToLower(pr.Status) {
	case "completed", "abandoned":
	case "active":
		beforeStatus := pr.Status
		if err := p.do(ctx, http.MethodPatch, endpoint, map[string]string{"status": "abandoned"}, &pr); err != nil {
			return ClosePullRequestResult{}, err
		}
		state, merged = adoPullRequestState(pr.Status)
		if state != "closed" && state != "merged" {
			return ClosePullRequestResult{}, fmt.Errorf("ado pull request %s abandon returned status %q", req.PullID, pr.Status)
		}
		p.recordPullRequestMutation(ctx, req.Repository, pr, "close", "", map[string]FieldDigest{
			"state": {
				Before: digestString(beforeStatus),
				After:  digestString(pr.Status),
			},
		})
	default:
		return ClosePullRequestResult{}, fmt.Errorf("ado pull request %s has unsupported status %q", req.PullID, pr.Status)
	}
	if req.Comment != "" {
		commentsEndpoint, err := p.repoURL(req.Repository, "pullrequests", req.PullID, "threads")
		if err != nil {
			return ClosePullRequestResult{}, err
		}
		body := adoPullRequestThreadRequest{
			Comments: []adoPullRequestThreadComment{{
				ParentCommentID: 0,
				Content:         req.Comment,
				CommentType:     1,
			}},
			Status: 1,
		}
		if err := p.do(ctx, http.MethodPost, commentsEndpoint, body, nil); err != nil {
			return ClosePullRequestResult{}, err
		}
		p.recordPullRequestMutation(ctx, req.Repository, pr, "comment", "", map[string]FieldDigest{
			"comment": {After: digestString(req.Comment)},
		})
	}
	number := pr.PullRequestID
	if number == 0 {
		number, err = strconv.Atoi(req.PullID)
		if err != nil {
			return ClosePullRequestResult{}, fmt.Errorf("ado pull request returned no id and pull id %q is not numeric", req.PullID)
		}
	}
	return ClosePullRequestResult{Number: number, Merged: merged, State: state}, nil
}

// MergePullRequest is not yet implemented for Azure DevOps: see PollPullRequest.
func (p *ADOProvider) MergePullRequest(ctx context.Context, req MergePullRequestRequest) (MergePullRequestResult, error) {
	return MergePullRequestResult{}, fmt.Errorf("ado: pull request merge lands in V1 parity (BL-033)")
}

// DetectMergePolicy is not yet implemented for Azure DevOps (issue #758):
// merge-policy abstraction parity is scoped to V1 (BL-033) alongside the
// rest of ADO's pull-request surface; the GitHub provider is the V0
// workload (#13).
func (p *ADOProvider) DetectMergePolicy(ctx context.Context, req RepoMergePolicyRequest) (RepoMergePolicyResult, error) {
	return RepoMergePolicyResult{}, fmt.Errorf("ado: merge policy detection lands in V1 parity (BL-033)")
}

// EnqueuePullRequest is not yet implemented for Azure DevOps: see DetectMergePolicy.
func (p *ADOProvider) EnqueuePullRequest(ctx context.Context, req EnqueuePullRequestRequest) (EnqueuePullRequestResult, error) {
	return EnqueuePullRequestResult{}, fmt.Errorf("ado: pull request enqueue lands in V1 parity (BL-033)")
}

// PollMergeQueueEntry is not yet implemented for Azure DevOps: see DetectMergePolicy.
func (p *ADOProvider) PollMergeQueueEntry(ctx context.Context, req PollMergeQueueEntryRequest) (PollMergeQueueEntryResult, error) {
	return PollMergeQueueEntryResult{}, fmt.Errorf("ado: merge queue entry polling lands in V1 parity (BL-033)")
}

// ListPullRequests lists active Azure DevOps pull requests matching the
// provider-neutral base and head-prefix filters.
func (p *ADOProvider) ListPullRequests(ctx context.Context, req ListPullRequestsRequest) ([]PullRequestSummary, error) {
	if err := requireRepo(req.Repository); err != nil {
		return nil, err
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests")
	if err != nil {
		return nil, err
	}
	values := url.Values{
		"searchCriteria.status":       []string{"active"},
		"searchCriteria.includeLinks": []string{"true"},
		"$top":                        []string{strconv.Itoa(adoPullRequestPageSize)},
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
		labels := make([]string, 0, len(pr.Labels))
		for _, label := range pr.Labels {
			labels = append(labels, label.Name)
		}
		prURL := pr.URL
		if pr.Links.Web.Href != "" {
			prURL = pr.Links.Web.Href
		}
		out = append(out, PullRequestSummary{
			ID:         strconv.Itoa(pr.PullRequestID),
			Number:     pr.PullRequestID,
			URL:        prURL,
			Head:       head,
			Base:       strings.TrimPrefix(pr.TargetRefName, "refs/heads/"),
			HeadSHA:    pr.LastMergeSourceCommit.CommitID,
			BaseSHA:    pr.LastMergeTargetCommit.CommitID,
			Draft:      pr.IsDraft,
			Labels:     labels,
			CheckState: CheckStatePending,
			UpdatedAt:  pr.CreationDate,
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
		return nil, fmt.Errorf("pull id is required")
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
				Path:   strings.TrimPrefix(change.Item.Path, "/"),
				Status: adoChangedFileStatus(change.ChangeType),
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

// CompareCommits is not yet implemented for Azure DevOps: see PullRequestFiles.
func (p *ADOProvider) CompareCommits(ctx context.Context, repo RepositoryRef, base, head string) (CompareResult, error) {
	return CompareResult{}, fmt.Errorf("ado: commit comparison lands in V1 parity (BL-033)")
}

// ListWorkItems lists Azure Boards work items as unified work items.
func (p *ADOProvider) ListWorkItems(ctx context.Context, req ListWorkItemsRequest) ([]WorkItem, error) {
	project := p.project(req.Repository)
	if err := p.requireWorkItemScope(project); err != nil {
		return nil, err
	}
	if err := validateADOTags(req.Labels); err != nil {
		return nil, err
	}
	query := "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project"
	requestedState := strings.ToLower(strings.TrimSpace(req.State))
	switch requestedState {
	case "", "all":
	case "open", "closed":
		// Common states are filtered after reading each item's process-specific
		// state category; custom processes may name Completed states arbitrarily.
	default:
		query += fmt.Sprintf(" AND [System.State] = '%s'", escapeWIQLString(req.State))
	}
	if req.Assignee != "" {
		query += fmt.Sprintf(" AND [System.AssignedTo] = '%s'", escapeWIQLString(req.Assignee))
	}
	if req.UpdatedSince != nil {
		query += fmt.Sprintf(
			" AND [System.ChangedDate] >= '%s'",
			req.UpdatedSince.UTC().Format(time.RFC3339),
		)
	}
	for _, label := range uniqueStrings(req.Labels) {
		query += fmt.Sprintf(" AND [System.Tags] CONTAINS '%s'", escapeWIQLString(label))
	}
	if req.Cursor != "" {
		afterID, err := strconv.Atoi(req.Cursor)
		if err != nil || afterID < 0 {
			return nil, fmt.Errorf("invalid ADO work-item cursor %q", req.Cursor)
		}
		query += fmt.Sprintf(" AND [System.Id] > %d", afterID)
	}
	if req.OldestFirst || req.PageInfo != nil || req.Cursor != "" {
		// WIQL without ORDER BY leaves result order unspecified — the same
		// Limit-truncation starvation hazard as GitHub's newest-first default
		// (#532). System.Id ascends with creation order, so this is FIFO.
		query += " ORDER BY [System.Id] ASC"
	}
	endpoint, err := p.workURL(project, "wiql")
	if err != nil {
		return nil, err
	}
	boundedScan := req.Cursor != "" || req.PageInfo != nil
	if boundedScan && req.Limit > 0 {
		endpoint, err = addQuery(endpoint, url.Values{"$top": []string{strconv.Itoa(req.Limit)}})
		if err != nil {
			return nil, err
		}
	}
	var wiql adoWIQLResponse
	if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{"query": query}, &wiql); err != nil {
		return nil, err
	}
	refs := wiql.WorkItems
	if boundedScan && req.Limit > 0 {
		refs = refs[:min(req.Limit, len(refs))]
	}
	if req.PageInfo != nil {
		req.PageInfo.CandidateCount = len(refs)
		req.PageInfo.HasNext = req.Limit > 0 && len(refs) == req.Limit
		req.PageInfo.NextCursor = ""
		if req.PageInfo.HasNext && len(refs) > 0 {
			req.PageInfo.NextCursor = strconv.Itoa(refs[len(refs)-1].ID)
		}
	}
	items := make([]WorkItem, 0, len(refs))
	for _, ref := range refs {
		item, err := p.GetWorkItem(ctx, req.Repository, strconv.Itoa(ref.ID))
		if err != nil {
			return nil, err
		}
		if (requestedState == "open" || requestedState == "closed") && item.State != requestedState {
			continue
		}
		matched, err := req.MatchesLabelPredicate(item.Labels)
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
		}
		matched, err = req.MatchesFieldPredicate(item.Fields)
		if err != nil {
			return nil, err
		}
		if hasAllLabels(item.Labels, req.Labels) && matched {
			items = append(items, item)
			if !boundedScan && req.Limit > 0 && len(items) >= req.Limit {
				break
			}
		}
	}
	return items, nil
}

// GetWorkItem reads an Azure Boards item as a unified work item.
func (p *ADOProvider) GetWorkItem(ctx context.Context, repo RepositoryRef, id string) (WorkItem, error) {
	if err := p.requireWorkItemScope(p.project(repo)); err != nil {
		return WorkItem{}, err
	}
	if err := validateADOWorkItemID(id); err != nil {
		return WorkItem{}, err
	}
	endpoint, err := p.workURL(p.project(repo), "workitems", id)
	if err != nil {
		return WorkItem{}, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"$expand": []string{"Relations"}})
	if err != nil {
		return WorkItem{}, err
	}
	var out adoWorkItem
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return WorkItem{}, err
	}
	return p.mapADOWorkItem(ctx, repo, out)
}

// CreateWorkItem creates an Azure Boards work item.
func (p *ADOProvider) CreateWorkItem(ctx context.Context, req CreateWorkItemRequest) (WorkItem, error) {
	project := p.project(req.Repository)
	if err := p.requireWorkItemScope(project); err != nil {
		return WorkItem{}, err
	}
	if strings.TrimSpace(req.Title) == "" {
		return WorkItem{}, fmt.Errorf("work item title is required")
	}
	if err := validateADOTags(req.Labels); err != nil {
		return WorkItem{}, err
	}
	itemType := req.Type
	if itemType == "" {
		itemType = "Issue"
	}
	itemBody := withRunIDFooter(req.Body, req.RunID)
	if req.RunID != "" {
		existing, found, err := p.findRunItem(ctx, req.Repository, req.RunID)
		if err != nil {
			return WorkItem{}, err
		}
		if found {
			return existing, nil
		}
	}
	endpoint, err := p.workURL(project, "workitems", "$"+itemType)
	if err != nil {
		return WorkItem{}, err
	}
	labels := replaceStatusLabel(req.Labels, req.Status)
	patch := []adoPatchOperation{
		{Op: "add", Path: "/fields/System.Title", Value: req.Title},
		{Op: "add", Path: "/fields/System.Description", Value: itemBody},
	}
	if len(labels) > 0 {
		patch = append(patch, adoPatchOperation{Op: "add", Path: "/fields/System.Tags", Value: strings.Join(labels, "; ")})
	}
	if req.Assignee != "" {
		patch = append(patch, adoPatchOperation{Op: "add", Path: "/fields/System.AssignedTo", Value: req.Assignee})
	}
	var out adoWorkItem
	if err := p.doPatch(ctx, http.MethodPost, endpoint, patch, &out); err != nil {
		return WorkItem{}, err
	}
	return p.mapADOWorkItem(ctx, req.Repository, out)
}

func (p *ADOProvider) findRunItem(ctx context.Context, repo RepositoryRef, runID string) (WorkItem, bool, error) {
	endpoint, err := p.workURL(p.project(repo), "wiql")
	if err != nil {
		return WorkItem{}, false, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"$top": []string{"20"}})
	if err != nil {
		return WorkItem{}, false, err
	}
	footer := runFooter(runID)
	query := fmt.Sprintf(
		"SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project AND [System.Description] CONTAINS WORDS '%s' ORDER BY [System.Id] ASC",
		escapeWIQLString(footer),
	)
	var result adoWIQLResponse
	if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{"query": query}, &result); err != nil {
		return WorkItem{}, false, err
	}
	for _, ref := range result.WorkItems {
		item, err := p.GetWorkItem(ctx, repo, strconv.Itoa(ref.ID))
		if err != nil {
			return WorkItem{}, false, err
		}
		if strings.Contains(item.Body, footer) {
			return item, true, nil
		}
	}
	return WorkItem{}, false, nil
}

// UpdateWorkItemStatus mirrors Goobers processing status to Azure Boards tags.
func (p *ADOProvider) UpdateWorkItemStatus(ctx context.Context, req UpdateWorkItemStatusRequest) (WorkItem, error) {
	if err := p.requireWorkItemScope(p.project(req.Repository)); err != nil {
		return WorkItem{}, err
	}
	if err := validateADOWorkItemID(req.ID); err != nil {
		return WorkItem{}, err
	}
	if req.Status == "" {
		return WorkItem{}, fmt.Errorf("work item status is required")
	}
	current, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	raw, err := rawADOWorkItem(current)
	if err != nil {
		return WorkItem{}, err
	}
	labels := replaceStatusLabel(adoRawTags(raw), req.Status)
	patch := []adoPatchOperation{
		{Op: "test", Path: "/rev", Value: raw.Rev},
		adoTagPatch(labels),
	}
	if (req.Status == WorkItemStatusDone || req.Status == WorkItemStatusClosed) && current.State != "closed" {
		state, stateErr := p.resolveCommonWorkItemState(ctx, req.Repository, current.Type, "closed")
		if stateErr != nil {
			return WorkItem{}, stateErr
		}
		patch = append(patch, adoPatchOperation{Op: "add", Path: "/fields/System.State", Value: state})
	}
	endpoint, err := p.workURL(p.project(req.Repository), "workitems", req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	var out adoWorkItem
	if err := p.doPatch(ctx, http.MethodPatch, endpoint, patch, &out); err != nil {
		return WorkItem{}, err
	}
	updated, err := p.mapADOWorkItem(ctx, req.Repository, out)
	if err != nil {
		return WorkItem{}, err
	}
	if req.Comment != "" {
		if err := p.postWorkItemComment(ctx, req.Repository, req.ID, req.Comment); err != nil {
			return updated, fmt.Errorf("work item status update committed; post comment: %w", err)
		}
	}
	return updated, nil
}

// ListComments returns Azure Boards work-item comments, oldest first.
func (p *ADOProvider) ListComments(ctx context.Context, repo RepositoryRef, id string) ([]Comment, error) {
	project := p.project(repo)
	if err := p.requireWorkItemScope(project); err != nil {
		return nil, err
	}
	if err := validateADOWorkItemID(id); err != nil {
		return nil, err
	}
	base, err := p.workURLVersion(project, "7.1-preview.4", "workItems", id, "comments")
	if err != nil {
		return nil, err
	}
	var comments []Comment
	continuation := ""
	for {
		values := url.Values{
			"$top":  []string{strconv.Itoa(adoCommentPageSize)},
			"order": []string{"asc"},
		}
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
		var page adoCommentsResponse
		if err := readJSONResponse(resp, http.MethodGet, endpoint, &page); err != nil {
			return nil, err
		}
		next := strings.TrimSpace(page.ContinuationToken)
		if next == "" {
			next = strings.TrimSpace(resp.Header.Get("x-ms-continuationtoken"))
		}
		for _, comment := range page.Comments {
			comments = append(comments, mapADOComment(comment))
		}
		if next == "" {
			return comments, nil
		}
		continuation = next
	}
}

// UpdateWorkItem edits Azure Boards fields, tags, state, and comments.
func (p *ADOProvider) UpdateWorkItem(ctx context.Context, req UpdateWorkItemRequest) (WorkItem, error) {
	if err := p.requireWorkItemScope(p.project(req.Repository)); err != nil {
		return WorkItem{}, err
	}
	if err := validateADOWorkItemID(req.ID); err != nil {
		return WorkItem{}, err
	}
	if err := validateADOTags(append(append([]string{}, req.AddLabels...), req.RemoveLabels...)); err != nil {
		return WorkItem{}, err
	}
	if req.Milestone != nil {
		return WorkItem{}, fmt.Errorf("ADO work items do not support numeric milestones; use an Azure Boards iteration")
	}
	state := strings.ToLower(strings.TrimSpace(req.State))
	if state != "" && state != "open" && state != "closed" {
		return WorkItem{}, fmt.Errorf("unsupported state %q (want open or closed)", req.State)
	}

	current, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	raw, err := rawADOWorkItem(current)
	if err != nil {
		return WorkItem{}, err
	}
	patch := []adoPatchOperation{{Op: "test", Path: "/rev", Value: raw.Rev}}
	if req.Title != nil {
		patch = append(patch, adoPatchOperation{Op: "add", Path: "/fields/System.Title", Value: *req.Title})
	}
	if req.Body != nil {
		patch = append(patch, adoPatchOperation{Op: "add", Path: "/fields/System.Description", Value: *req.Body})
	}
	if labelsChanged(req) {
		labels := applyLabelSet(adoRawTags(raw), req.AddLabels, req.RemoveLabels)
		patch = append(patch, adoTagPatch(labels))
	}
	if state != "" && state != current.State {
		nativeState, stateErr := p.resolveCommonWorkItemState(ctx, req.Repository, current.Type, state)
		if stateErr != nil {
			return WorkItem{}, stateErr
		}
		patch = append(patch, adoPatchOperation{Op: "add", Path: "/fields/System.State", Value: nativeState})
	}

	updated := current
	mutated := false
	if len(patch) > 1 {
		endpoint, err := p.workURL(p.project(req.Repository), "workitems", req.ID)
		if err != nil {
			return WorkItem{}, err
		}
		var out adoWorkItem
		if err := p.doPatch(ctx, http.MethodPatch, endpoint, patch, &out); err != nil {
			return WorkItem{}, err
		}
		updated, err = p.mapADOWorkItem(ctx, req.Repository, out)
		if err != nil {
			return WorkItem{}, err
		}
		mutated = true
	}
	if req.Comment != "" {
		if err := p.postWorkItemComment(ctx, req.Repository, req.ID, req.Comment); err != nil {
			if mutated {
				return updated, fmt.Errorf("work item update committed; post comment: %w", err)
			}
			return updated, fmt.Errorf("post work item comment: %w", err)
		}
	}
	return updated, nil
}

// ClaimWorkItem atomically adds the visible claim tag and an internal owner tag.
// The /rev test makes concurrent read-modify-write attempts settle on one winner.
func (p *ADOProvider) ClaimWorkItem(ctx context.Context, req ClaimWorkItemRequest) (ClaimResult, error) {
	if err := p.requireWorkItemScope(p.project(req.Repository)); err != nil {
		return ClaimResult{}, err
	}
	if err := validateADOWorkItemID(req.ID); err != nil {
		return ClaimResult{}, err
	}
	if strings.TrimSpace(req.RunID) == "" {
		return ClaimResult{}, fmt.Errorf("run id is required to claim an item")
	}
	label := req.ClaimLabel
	if label == "" {
		label = LabelClaimed
	}
	if err := validateADOTags([]string{label}); err != nil {
		return ClaimResult{}, err
	}
	ownerTag, err := adoClaimTag(req.RunID)
	if err != nil {
		return ClaimResult{}, err
	}

	var conflict error
	for range adoClaimRetries {
		current, getErr := p.GetWorkItem(ctx, req.Repository, req.ID)
		if getErr != nil {
			return ClaimResult{}, getErr
		}
		raw, rawErr := rawADOWorkItem(current)
		if rawErr != nil {
			return ClaimResult{}, rawErr
		}
		winner, claimed, ownerErr := adoClaimOwner(adoRawTags(raw))
		if ownerErr != nil {
			return ClaimResult{}, ownerErr
		}
		if claimed {
			return ClaimResult{Claimed: winner == req.RunID, ClaimedBy: winner, Item: current}, nil
		}

		labels := applyLabelSet(adoRawTags(raw), []string{label, ownerTag}, nil)
		patch := []adoPatchOperation{
			{Op: "test", Path: "/rev", Value: raw.Rev},
			adoTagPatch(labels),
		}
		endpoint, endpointErr := p.workURL(p.project(req.Repository), "workitems", req.ID)
		if endpointErr != nil {
			return ClaimResult{}, endpointErr
		}
		var out adoWorkItem
		if patchErr := p.doPatch(ctx, http.MethodPatch, endpoint, patch, &out); patchErr != nil {
			if isADORevisionConflict(patchErr) {
				conflict = patchErr
				continue
			}
			return ClaimResult{}, patchErr
		}
		item, mapErr := p.mapADOWorkItem(ctx, req.Repository, out)
		if mapErr != nil {
			return ClaimResult{}, mapErr
		}
		return ClaimResult{Claimed: true, ClaimedBy: req.RunID, Item: item}, nil
	}
	return ClaimResult{}, fmt.Errorf("claim work item %s after revision conflicts: %w", req.ID, conflict)
}

// ReleaseWorkItemClaim removes an ADO claim marker owned by the requesting run.
func (p *ADOProvider) ReleaseWorkItemClaim(ctx context.Context, req ClaimWorkItemRequest) (WorkItem, error) {
	if err := p.requireWorkItemScope(p.project(req.Repository)); err != nil {
		return WorkItem{}, err
	}
	if err := validateADOWorkItemID(req.ID); err != nil {
		return WorkItem{}, err
	}
	if strings.TrimSpace(req.RunID) == "" {
		return WorkItem{}, fmt.Errorf("run id is required to release an item")
	}
	label := req.ClaimLabel
	if label == "" {
		label = LabelClaimed
	}

	var conflict error
	for range adoClaimRetries {
		current, getErr := p.GetWorkItem(ctx, req.Repository, req.ID)
		if getErr != nil {
			return WorkItem{}, getErr
		}
		raw, rawErr := rawADOWorkItem(current)
		if rawErr != nil {
			return WorkItem{}, rawErr
		}
		winner, claimed, ownerErr := adoClaimOwner(adoRawTags(raw))
		if ownerErr != nil {
			return WorkItem{}, ownerErr
		}
		if !claimed {
			return current, nil
		}
		if winner != req.RunID && !req.LedgerAuthorized {
			return WorkItem{}, fmt.Errorf("provider claim is held by run %q", winner)
		}
		ownerTag, tagErr := adoClaimTag(winner)
		if tagErr != nil {
			return WorkItem{}, tagErr
		}
		labels := applyLabelSet(adoRawTags(raw), nil, []string{label, ownerTag})
		patch := []adoPatchOperation{
			{Op: "test", Path: "/rev", Value: raw.Rev},
			adoTagPatch(labels),
		}
		endpoint, endpointErr := p.workURL(p.project(req.Repository), "workitems", req.ID)
		if endpointErr != nil {
			return WorkItem{}, endpointErr
		}
		var out adoWorkItem
		if patchErr := p.doPatch(ctx, http.MethodPatch, endpoint, patch, &out); patchErr != nil {
			if isADORevisionConflict(patchErr) {
				conflict = patchErr
				continue
			}
			return WorkItem{}, patchErr
		}
		return p.mapADOWorkItem(ctx, req.Repository, out)
	}
	return WorkItem{}, fmt.Errorf("release work item %s after revision conflicts: %w", req.ID, conflict)
}

// Subscribe emits Azure Boards backlog item availability events.
func (p *ADOProvider) Subscribe(ctx context.Context, sub TriggerSubscription) (<-chan WorkItemEvent, error) {
	if sub.Kind != TriggerPolling {
		return nil, fmt.Errorf("ado provider supports polling subscriptions in-process; service hook delivery is configured externally")
	}
	interval := sub.PollInterval
	if interval <= 0 {
		interval = time.Minute
	}
	events := make(chan WorkItemEvent, 1)
	go func() {
		defer close(events)
		seen := map[string]time.Time{}
		for {
			items, err := p.ListWorkItems(ctx, ListWorkItemsRequest{Repository: sub.Repository, State: "New", Limit: 100})
			if err == nil {
				for _, item := range items {
					if !shouldEmitWorkItem(seen, item) {
						continue
					}
					select {
					case <-ctx.Done():
						return
					case events <- WorkItemEvent{Provider: ProviderADO, Kind: TriggerPolling, Item: item, Action: "available"}:
					}
				}
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return events, nil
}

func (p *ADOProvider) pathExists(ctx context.Context, repo RepositoryRef, branch, path string) (bool, error) {
	endpoint, err := p.repoURL(repo, "items")
	if err != nil {
		return false, err
	}
	endpoint, err = addQuery(endpoint, url.Values{
		"path":                          []string{"/" + strings.TrimPrefix(path, "/")},
		"versionDescriptor.versionType": []string{"branch"},
		"versionDescriptor.version":     []string{branch},
		"includeContentMetadata":        []string{"false"},
	})
	if err != nil {
		return false, err
	}
	resp, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, fmt.Errorf("GET %s failed: status %d", endpoint, resp.StatusCode)
	}
	return true, nil
}

func (p *ADOProvider) branchSHA(ctx context.Context, repo RepositoryRef, branch string) (string, error) {
	endpoint, err := p.repoURL(repo, "refs")
	if err != nil {
		return "", err
	}
	endpoint, err = addQuery(endpoint, url.Values{"filter": []string{"heads/" + strings.TrimPrefix(branch, "refs/heads/")}})
	if err != nil {
		return "", err
	}
	var out adoRefsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return "", err
	}
	if len(out.Value) == 0 || out.Value[0].ObjectID == "" {
		return "", fmt.Errorf("ado branch %q not found", branch)
	}
	return out.Value[0].ObjectID, nil
}

func (p *ADOProvider) repoURL(repo RepositoryRef, elems ...string) (string, error) {
	repoID := repo.ID
	if repoID == "" {
		repoID = repo.Name
	}
	parts := []string{p.Organization, p.project(repo), "_apis", "git", "repositories", repoID}
	parts = append(parts, elems...)
	endpoint, err := joinURL(p.BaseURL, parts...)
	if err != nil {
		return "", err
	}
	return addQuery(endpoint, url.Values{"api-version": []string{"7.1"}})
}

func (p *ADOProvider) workURL(project string, elems ...string) (string, error) {
	return p.workURLVersion(project, "7.1", elems...)
}

func (p *ADOProvider) workURLVersion(project, version string, elems ...string) (string, error) {
	parts := []string{p.Organization, project, "_apis", "wit"}
	parts = append(parts, elems...)
	endpoint, err := joinURL(p.BaseURL, parts...)
	if err != nil {
		return "", err
	}
	return addQuery(endpoint, url.Values{"api-version": []string{version}})
}

func (p *ADOProvider) buildURL(repo RepositoryRef, elems ...string) (string, error) {
	parts := []string{p.Organization, p.project(repo), "_apis", "build"}
	parts = append(parts, elems...)
	endpoint, err := joinURL(p.BaseURL, parts...)
	if err != nil {
		return "", err
	}
	return addQuery(endpoint, url.Values{"api-version": []string{"7.1"}})
}

func (p *ADOProvider) pullRequestBuildState(ctx context.Context, repo RepositoryRef, pullID, repositoryID, headSHA string) (CheckState, []CheckDetail, error) {
	endpoint, err := p.buildURL(repo, "builds")
	if err != nil {
		return "", nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{
		"$top":           []string{"100"},
		"branchName":     []string{"refs/pull/" + pullID + "/merge"},
		"queryOrder":     []string{"queueTimeDescending"},
		"reasonFilter":   []string{"pullRequest"},
		"repositoryId":   []string{repositoryID},
		"repositoryType": []string{"TfsGit"},
	})
	if err != nil {
		return "", nil, err
	}
	builds, err := p.listPullRequestBuilds(ctx, endpoint)
	if err != nil {
		return "", nil, err
	}

	checks := make([]CheckDetail, 0, len(builds))
	seenDefinitions := make(map[string]bool, len(builds))
	for _, build := range builds {
		sourceSHA := build.TriggerInfo["pr.sourceSha"]
		if sourceSHA == "" || headSHA == "" || !strings.EqualFold(sourceSHA, headSHA) {
			continue
		}
		key := strconv.Itoa(build.Definition.ID)
		if build.Definition.ID == 0 {
			key = build.Definition.Name
		}
		if key != "" && seenDefinitions[key] {
			continue
		}
		if key != "" {
			seenDefinitions[key] = true
		}
		name := build.Definition.Name
		if name == "" {
			name = build.BuildNumber
		}
		if name == "" {
			name = fmt.Sprintf("build %d", build.ID)
		}
		buildURL := build.URL
		if build.Links.Web.Href != "" {
			buildURL = build.Links.Web.Href
		}
		conclusion := build.Result
		if conclusion == "" {
			conclusion = build.Status
		}
		checks = append(checks, CheckDetail{
			Name:       name,
			State:      adoBuildState(build.Status, build.Result),
			Conclusion: conclusion,
			URL:        buildURL,
			Summary:    build.BuildNumber,
		})
	}
	return combinedCheckState(checks), checks, nil
}

func (p *ADOProvider) listPullRequestBuilds(ctx context.Context, endpoint string) ([]adoBuild, error) {
	base := endpoint
	var builds []adoBuild
	for {
		resp, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
		if err != nil {
			return nil, err
		}
		var page adoBuildsResponse
		if err := readJSONResponse(resp, http.MethodGet, endpoint, &page); err != nil {
			return nil, err
		}
		builds = append(builds, page.Value...)
		continuation := strings.TrimSpace(resp.Header.Get("x-ms-continuationtoken"))
		if continuation == "" {
			return builds, nil
		}
		endpoint, err = addQuery(base, url.Values{"continuationToken": []string{continuation}})
		if err != nil {
			return nil, err
		}
	}
}

func (p *ADOProvider) pullRequestCommentsSince(ctx context.Context, repo RepositoryRef, pullID string, since time.Time) ([]PullRequestComment, error) {
	endpoint, err := p.repoURL(repo, "pullrequests", pullID, "threads")
	if err != nil {
		return nil, err
	}
	var out adoPullRequestThreadsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	comments := make([]PullRequestComment, 0)
	for _, thread := range out.Value {
		for _, comment := range thread.Comments {
			if comment.IsDeleted || comment.Content == "" || !comment.PublishedDate.After(since) {
				continue
			}
			author := comment.Author.UniqueName
			if author == "" {
				author = comment.Author.DisplayName
			}
			id := int64(comment.ID)
			if thread.ID > 0 {
				id = thread.ID<<32 | int64(uint32(comment.ID))
			}
			comments = append(comments, PullRequestComment{
				ID:        id,
				Author:    author,
				Body:      comment.Content,
				CreatedAt: comment.PublishedDate,
			})
		}
	}
	return comments, nil
}

func pullRequestNumber(pullID string) int {
	number, _ := strconv.Atoi(pullID)
	return number
}

func adoReviewDecision(reviewers []adoReviewer) (ReviewDecision, int) {
	requestedChanges := 0
	approved := false
	for _, reviewer := range reviewers {
		switch {
		case reviewer.Vote < 0:
			requestedChanges++
		case reviewer.Vote > 0:
			approved = true
		}
	}
	switch {
	case requestedChanges > 0:
		return ReviewDecisionChangesRequested, requestedChanges
	case approved:
		return ReviewDecisionApproved, 0
	default:
		return ReviewDecisionPending, 0
	}
}

func adoBuildState(status, result string) CheckState {
	if !strings.EqualFold(status, "completed") {
		return CheckStatePending
	}
	switch strings.ToLower(result) {
	case "succeeded":
		return CheckStatePassing
	case "partiallysucceeded", "failed", "canceled":
		return CheckStateFailing
	default:
		return CheckStatePending
	}
}

func combinedCheckState(checks []CheckDetail) CheckState {
	pending := false
	for _, check := range checks {
		switch check.State {
		case CheckStateFailing:
			return CheckStateFailing
		case CheckStatePending:
			pending = true
		}
	}
	if pending || len(checks) == 0 {
		return CheckStatePending
	}
	return CheckStatePassing
}

func adoPullRequestState(status string) (string, bool) {
	switch strings.ToLower(status) {
	case "active":
		return "open", false
	case "abandoned":
		return "closed", false
	case "completed":
		return "merged", true
	default:
		return strings.ToLower(status), false
	}
}

func adoPullRequestPollState(status string) (string, bool) {
	switch strings.ToLower(status) {
	case "completed":
		return "closed", true
	case "abandoned":
		return "closed", false
	case "active":
		return "open", false
	default:
		return strings.ToLower(status), false
	}
}

func adoMergeable(status string) *bool {
	var mergeable bool
	switch strings.ToLower(status) {
	case "succeeded":
		mergeable = true
	case "conflicts", "failure", "rejectedbypolicy":
	default:
		return nil
	}
	return &mergeable
}

func (p *ADOProvider) project(repo RepositoryRef) string {
	if repo.Project != "" {
		return repo.Project
	}
	return p.Project
}

func (p *ADOProvider) do(ctx context.Context, method, endpoint string, body interface{}, out interface{}) error {
	resp, err := p.send(ctx, method, endpoint, body, "")
	if err != nil {
		return err
	}
	return readJSONResponse(resp, method, endpoint, out)
}

func (p *ADOProvider) doPatch(ctx context.Context, method, endpoint string, body interface{}, out interface{}) error {
	resp, err := p.send(ctx, method, endpoint, body, "application/json-patch+json")
	if err != nil {
		return err
	}
	return readJSONResponse(resp, method, endpoint, out)
}

func (p *ADOProvider) send(ctx context.Context, method, endpoint string, body interface{}, contentType string) (*http.Response, error) {
	maxWait := p.maxRateLimitWait
	if maxWait <= 0 {
		maxWait = defaultRateLimitMaxWait
	}
	var waited time.Duration
	rateAttempt := 0
	authRetried := false
	for {
		req, err := newJSONRequest(ctx, method, endpoint, body)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		header, err := p.authorizationHeader(ctx)
		if err != nil {
			return nil, err
		}
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := httpClientOrDefault(p.Client).Do(req)
		if err != nil {
			return nil, fmt.Errorf("send request: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized && !authRetried && p.invalidateCredential() {
			_ = resp.Body.Close()
			authRetried = true
			continue
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		wait, ev := p.rateLimitPlan(resp, endpoint, rateAttempt)
		if rateAttempt >= p.maxRetries || wait > maxWait-waited {
			ev.Outcome = RateLimitOutcomeExhausted
			p.observeRateLimit(ctx, ev)
			return resp, nil
		}
		_ = resp.Body.Close()
		if err := p.sleep(ctx, wait); err != nil {
			ev.Outcome = RateLimitOutcomeCanceled
			p.observeRateLimit(ctx, ev)
			return nil, err
		}
		ev.Outcome = RateLimitOutcomeRetry
		p.observeRateLimit(ctx, ev)
		waited += wait
		rateAttempt++
	}
}

func (p *ADOProvider) authorizationHeader(ctx context.Context) (string, error) {
	if p.credentialSource == nil {
		return "", nil
	}
	credential, err := p.credentialSource.Credential(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve ADO credential: %w", err)
	}
	header, err := credential.authorizationHeader()
	if err != nil {
		return "", err
	}
	if p.secretRegistrar != nil {
		p.secretRegistrar.Register([]byte(credential.Secret))
		p.secretRegistrar.Register([]byte(strings.TrimSpace(strings.TrimPrefix(header, "Basic "))))
	}
	return header, nil
}

func (p *ADOProvider) invalidateCredential() bool {
	source, ok := p.credentialSource.(refreshableADOCredentialSource)
	if !ok {
		return false
	}
	source.Invalidate()
	return true
}

func (p *ADOProvider) rateLimitPlan(resp *http.Response, endpoint string, attempt int) (time.Duration, RateLimitEvent) {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	serverDelay, directed := retryAfterDelay(raw, p.now())
	wait := serverDelay
	if !directed || serverDelay <= 0 {
		wait = fallbackBackoff(attempt, p.jitter)
	} else {
		wait += rateLimitResetSlack
	}
	return wait, RateLimitEvent{
		Provider:      ProviderADO,
		Scope:         rateLimitScope(endpoint),
		Delay:         wait,
		Endpoint:      endpoint,
		Status:        resp.StatusCode,
		RetryAfter:    serverDelay,
		RetryAfterRaw: raw,
		Attempt:       attempt,
	}
}

func (p *ADOProvider) observeRateLimit(ctx context.Context, ev RateLimitEvent) {
	if p.rateObserver != nil {
		p.rateObserver.ObserveRateLimit(ctx, ev)
	}
}

func (p *ADOProvider) recordPullRequestMutation(ctx context.Context, repo RepositoryRef, pr adoPullRequest, operation, runID string, fields map[string]FieldDigest) {
	if p.recorder == nil {
		return
	}
	repoName := repo.Name
	if repoName == "" {
		repoName = repo.ID
	}
	organization := repo.Owner
	if organization == "" {
		organization = p.Organization
	}
	p.recorder.RecordExternalRef(ctx, ExternalRef{
		Provider:  ProviderADO,
		Ref:       fmt.Sprintf("%s/%s/%s#%d", organization, p.project(repo), repoName, pr.PullRequestID),
		URL:       adoPullRequestURL(pr),
		Operation: operation,
		Fields:    fields,
		RunID:     runID,
	})
}

func adoPullRequestURL(pr adoPullRequest) string {
	if pr.Links.Web.Href != "" {
		return pr.Links.Web.Href
	}
	return pr.URL
}

type adoWIQLResponse struct {
	WorkItems []struct {
		ID int `json:"id"`
	} `json:"workItems"`
}

type adoWorkItem struct {
	ID        int                    `json:"id"`
	Rev       int                    `json:"rev"`
	URL       string                 `json:"url"`
	Fields    map[string]interface{} `json:"fields"`
	Relations []adoRelation          `json:"relations"`
}

type adoCommentsResponse struct {
	Comments          []adoComment `json:"comments"`
	ContinuationToken string       `json:"continuationToken"`
}

type adoComment struct {
	ID          int         `json:"id"`
	CommentID   int         `json:"commentId"`
	Text        string      `json:"text"`
	CreatedBy   adoIdentity `json:"createdBy"`
	CreatedDate string      `json:"createdDate"`
	URL         string      `json:"url"`
}

type adoRelation struct {
	Rel        string                 `json:"rel"`
	URL        string                 `json:"url"`
	Attributes map[string]interface{} `json:"attributes"`
}

type adoPatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

type adoRefsResponse struct {
	Value []struct {
		Name     string `json:"name"`
		ObjectID string `json:"objectId"`
		URL      string `json:"url"`
	} `json:"value"`
}

type adoRefUpdate struct {
	Name        string `json:"name"`
	OldObjectID string `json:"oldObjectId"`
}

type adoPushRequest struct {
	RefUpdates []adoRefUpdate `json:"refUpdates"`
	Commits    []adoCommit    `json:"commits"`
}

type adoCommit struct {
	Comment string      `json:"comment"`
	Changes []adoChange `json:"changes"`
}

type adoChange struct {
	ChangeType string            `json:"changeType"`
	Item       map[string]string `json:"item"`
	NewContent *adoNewContent    `json:"newContent,omitempty"`
}

type adoNewContent struct {
	Content     string `json:"content"`
	ContentType string `json:"contentType"`
}

type adoPushResponse struct {
	URL     string `json:"url"`
	Commits []struct {
		CommitID string `json:"commitId"`
	} `json:"commits"`
}

type adoPullRequest struct {
	PullRequestID         int           `json:"pullRequestId"`
	URL                   string        `json:"url"`
	Status                string        `json:"status"`
	Title                 string        `json:"title"`
	Description           string        `json:"description"`
	CreatedBy             adoIdentity   `json:"createdBy"`
	CreationDate          time.Time     `json:"creationDate"`
	ClosedDate            *time.Time    `json:"closedDate"`
	SourceRefName         string        `json:"sourceRefName"`
	TargetRefName         string        `json:"targetRefName"`
	IsDraft               bool          `json:"isDraft"`
	MergeStatus           string        `json:"mergeStatus"`
	Reviewers             []adoReviewer `json:"reviewers"`
	Repository            adoRepository `json:"repository"`
	Labels                []adoLabel    `json:"labels"`
	LastMergeSourceCommit adoCommitRef  `json:"lastMergeSourceCommit"`
	LastMergeTargetCommit adoCommitRef  `json:"lastMergeTargetCommit"`
	Links                 adoPRLinks    `json:"_links"`
}

type adoPullRequestsResponse struct {
	Value []adoPullRequest `json:"value"`
}

type adoIdentity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
}

type adoReviewer struct {
	Vote int `json:"vote"`
}

type adoRepository struct {
	ID string `json:"id"`
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

type adoBuildsResponse struct {
	Value []adoBuild `json:"value"`
}

type adoBuild struct {
	ID          int    `json:"id"`
	BuildNumber string `json:"buildNumber"`
	Status      string `json:"status"`
	Result      string `json:"result"`
	URL         string `json:"url"`
	Definition  struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"definition"`
	TriggerInfo map[string]string `json:"triggerInfo"`
	Links       adoPRLinks        `json:"_links"`
}

type adoPullRequestThreadRequest struct {
	Comments []adoPullRequestThreadComment `json:"comments"`
	Status   int                           `json:"status"`
}

type adoPullRequestThreadComment struct {
	ParentCommentID int    `json:"parentCommentId"`
	Content         string `json:"content"`
	CommentType     int    `json:"commentType"`
}

type adoPullRequestThreadsResponse struct {
	Value []adoPullRequestThread `json:"value"`
}

type adoPullRequestThread struct {
	ID       int64                           `json:"id"`
	Comments []adoPullRequestResponseComment `json:"comments"`
}

type adoPullRequestResponseComment struct {
	ID            int         `json:"id"`
	Author        adoIdentity `json:"author"`
	Content       string      `json:"content"`
	PublishedDate time.Time   `json:"publishedDate"`
	IsDeleted     bool        `json:"isDeleted"`
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

func (p *ADOProvider) mapADOWorkItem(ctx context.Context, repo RepositoryRef, item adoWorkItem) (WorkItem, error) {
	nativeState := stringField(item.Fields, "System.State")
	itemType := stringField(item.Fields, "System.WorkItemType")
	categories, err := p.adoWorkItemStateCategories(ctx, repo, itemType)
	if err != nil {
		return WorkItem{}, err
	}
	definition, found := findADOWorkItemState(categories, nativeState)
	if !found {
		return WorkItem{}, fmt.Errorf("ADO work item type %q has unknown state %q", itemType, nativeState)
	}
	state, status, err := commonADOStateCategory(definition.Category)
	if err != nil {
		return WorkItem{}, fmt.Errorf("map ADO work item type %q state %q: %w", itemType, nativeState, err)
	}
	return mapADOWorkItemState(item, state, status), nil
}

func mapADOWorkItemState(item adoWorkItem, state string, status WorkItemStatus) WorkItem {
	labels := adoVisibleLabels(adoRawTags(item))
	parent, links, hierarchy := adoHierarchy(item.Relations)
	updated := timeField(item.Fields, "System.ChangedDate")
	return WorkItem{
		Provider:   ProviderADO,
		ID:         strconv.Itoa(item.ID),
		ExternalID: strconv.Itoa(item.Rev),
		Type:       stringField(item.Fields, "System.WorkItemType"),
		Title:      stringField(item.Fields, "System.Title"),
		Body:       stringField(item.Fields, "System.Description"),
		Labels:     labels,
		State:      state,
		Status:     statusFromLabels(labels, string(status)),
		Assignee:   stringField(item.Fields, "System.AssignedTo"),
		Links:      links,
		Parent:     parent,
		Hierarchy:  hierarchy,
		URL:        item.URL,
		CreatedAt:  timeField(item.Fields, "System.CreatedDate"),
		UpdatedAt:  updated,
		Fields:     adoWorkItemFields(item),
		Raw:        item,
	}
}

func mapADOComment(comment adoComment) Comment {
	author := comment.CreatedBy.DisplayName
	if author == "" {
		author = comment.CreatedBy.UniqueName
	}
	id := comment.CommentID
	if id == 0 {
		id = comment.ID
	}
	var createdAt *time.Time
	if parsed, err := time.Parse(time.RFC3339Nano, comment.CreatedDate); err == nil {
		createdAt = &parsed
	}
	return Comment{
		ID:         strconv.Itoa(id),
		Author:     author,
		AuthorType: "user",
		Body:       comment.Text,
		CreatedAt:  createdAt,
		URL:        comment.URL,
	}
}

func adoWorkItemFields(item adoWorkItem) fieldpredicate.Fields {
	fields := fieldpredicate.Fields{
		"System.Id":  int64(item.ID),
		"System.Rev": int64(item.Rev),
	}
	for name, value := range item.Fields {
		switch value.(type) {
		case string, bool,
			int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64,
			float32, float64:
			fields[name] = value
		}
	}
	return fields
}

func adoLabels(tags string) []string {
	if tags == "" {
		return nil
	}
	return uniqueStrings(strings.Split(tags, ";"))
}

func adoVisibleLabels(labels []string) []string {
	visible := make([]string, 0, len(labels))
	for _, label := range labels {
		if !strings.HasPrefix(label, adoClaimTagPrefix) {
			visible = append(visible, label)
		}
	}
	return visible
}

func adoHierarchy(relations []adoRelation) (*WorkItemRef, []Link, map[string]interface{}) {
	links := make([]Link, 0, len(relations))
	hierarchy := make(map[string]interface{})
	var parent *WorkItemRef
	for _, relation := range relations {
		links = append(links, Link{Rel: relation.Rel, URL: relation.URL})
		if relation.Rel == "System.LinkTypes.Hierarchy-Reverse" {
			parent = &WorkItemRef{Provider: ProviderADO, ID: lastPathSegment(relation.URL), URL: relation.URL, Type: "parent"}
			hierarchy["parent"] = relation
		}
	}
	return parent, links, hierarchy
}

func lastPathSegment(value string) string {
	trimmed := strings.TrimRight(value, "/")
	index := strings.LastIndex(trimmed, "/")
	if index == -1 {
		return trimmed
	}
	return trimmed[index+1:]
}

func stringField(fields map[string]interface{}, key string) string {
	value, ok := fields[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]interface{}:
		if display, ok := typed["displayName"].(string); ok {
			return display
		}
	}
	return fmt.Sprint(value)
}

func timeField(fields map[string]interface{}, key string) *time.Time {
	value := stringField(fields, key)
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	return &parsed
}

func (p *ADOProvider) requireWorkItemScope(project string) error {
	if strings.TrimSpace(p.Organization) == "" {
		return fmt.Errorf("ADO organization is required")
	}
	if strings.TrimSpace(project) == "" {
		return fmt.Errorf("ADO project is required")
	}
	return nil
}

func validateADOWorkItemID(id string) error {
	n, err := strconv.Atoi(id)
	if err != nil || n <= 0 {
		return fmt.Errorf("ADO work item id must be a positive integer")
	}
	return nil
}

func validateADOTags(tags []string) error {
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return fmt.Errorf("ADO work item tag must not be empty")
		}
		if len(tag) > adoMaxTagLength {
			return fmt.Errorf("ADO work item tag exceeds %d characters", adoMaxTagLength)
		}
		if strings.ContainsAny(tag, ",;") {
			return fmt.Errorf("ADO work item tag %q contains a comma or semicolon", tag)
		}
	}
	return nil
}

func escapeWIQLString(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "'", "''")
}

type adoWorkItemStatesResponse struct {
	Value []adoWorkItemState `json:"value"`
}

type adoWorkItemState struct {
	Name     string `json:"name"`
	Category string `json:"category"`
}

func (p *ADOProvider) resolveCommonWorkItemState(ctx context.Context, repo RepositoryRef, itemType, state string) (string, error) {
	categoriesByState, err := p.adoWorkItemStateCategories(ctx, repo, itemType)
	if err != nil {
		return "", err
	}
	categories := []string{"Proposed", "InProgress", "Resolved"}
	if state == "closed" {
		categories = []string{"Completed"}
	}
	for _, category := range categories {
		for _, candidate := range categoriesByState {
			if strings.EqualFold(candidate.Category, category) {
				return candidate.Name, nil
			}
		}
	}
	return "", fmt.Errorf("ADO work item type %q has no state category for %q", itemType, state)
}

func (p *ADOProvider) adoWorkItemStateCategories(ctx context.Context, repo RepositoryRef, itemType string) ([]adoWorkItemState, error) {
	if strings.TrimSpace(itemType) == "" {
		return nil, fmt.Errorf("ADO work item type is required to resolve state categories")
	}
	key := p.project(repo) + "\x00" + strings.ToLower(itemType)
	p.stateMu.RLock()
	cached := p.stateCategories[key]
	p.stateMu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	endpoint, err := p.workURL(p.project(repo), "workitemtypes", itemType, "states")
	if err != nil {
		return nil, err
	}
	var response adoWorkItemStatesResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return nil, err
	}
	resolved := response.Value
	p.stateMu.Lock()
	if p.stateCategories == nil {
		p.stateCategories = make(map[string][]adoWorkItemState)
	}
	if cached = p.stateCategories[key]; cached == nil {
		p.stateCategories[key] = resolved
		cached = resolved
	}
	p.stateMu.Unlock()
	return cached, nil
}

func findADOWorkItemState(states []adoWorkItemState, name string) (adoWorkItemState, bool) {
	for _, state := range states {
		if strings.EqualFold(state.Name, name) {
			return state, true
		}
	}
	return adoWorkItemState{}, false
}

func commonADOStateCategory(category string) (string, WorkItemStatus, error) {
	switch strings.ToLower(category) {
	case "completed", "removed":
		return "closed", WorkItemStatusDone, nil
	case "inprogress", "resolved":
		return "open", WorkItemStatusInProgress, nil
	case "proposed":
		return "open", WorkItemStatusOpen, nil
	default:
		return "", "", fmt.Errorf("unsupported state category %q", category)
	}
}

func rawADOWorkItem(item WorkItem) (adoWorkItem, error) {
	raw, ok := item.Raw.(adoWorkItem)
	if !ok || raw.Rev <= 0 {
		return adoWorkItem{}, fmt.Errorf("ADO work item %s is missing revision metadata", item.ID)
	}
	return raw, nil
}

func adoRawTags(item adoWorkItem) []string {
	return adoLabels(stringField(item.Fields, "System.Tags"))
}

func adoTagPatch(tags []string) adoPatchOperation {
	return adoPatchOperation{Op: "add", Path: "/fields/System.Tags", Value: strings.Join(uniqueStrings(tags), "; ")}
}

func (p *ADOProvider) postWorkItemComment(ctx context.Context, repo RepositoryRef, id, text string) error {
	endpoint, err := p.workURLVersion(p.project(repo), "7.1-preview.4", "workItems", id, "comments")
	if err != nil {
		return err
	}
	return p.do(ctx, http.MethodPost, endpoint, map[string]string{"text": text}, nil)
}

func adoClaimTag(runID string) (string, error) {
	tag := adoClaimTagPrefix + base64.RawURLEncoding.EncodeToString([]byte(runID))
	if err := validateADOTags([]string{tag}); err != nil {
		return "", fmt.Errorf("encode ADO claim owner: %w", err)
	}
	return tag, nil
}

func adoClaimOwner(tags []string) (string, bool, error) {
	owner := ""
	for _, tag := range tags {
		if !strings.HasPrefix(tag, adoClaimTagPrefix) {
			continue
		}
		encoded := strings.TrimPrefix(tag, adoClaimTagPrefix)
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(decoded) == 0 {
			return "", false, fmt.Errorf("invalid ADO claim owner tag")
		}
		if owner != "" && owner != string(decoded) {
			return "", false, fmt.Errorf("ADO work item has multiple claim owners")
		}
		owner = string(decoded)
	}
	return owner, owner != "", nil
}

func isADORevisionConflict(err error) bool {
	var responseErr *providerResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	if responseErr.statusCode == http.StatusConflict || responseErr.statusCode == http.StatusPreconditionFailed {
		return true
	}
	body := strings.ToLower(responseErr.body)
	return responseErr.statusCode == http.StatusBadRequest &&
		strings.Contains(body, "/rev") &&
		(strings.Contains(body, "vs403351") || strings.Contains(body, "test operation"))
}

func hasAllLabels(itemLabels, required []string) bool {
	if len(required) == 0 {
		return true
	}
	item := make(map[string]struct{}, len(itemLabels))
	for _, label := range itemLabels {
		item[label] = struct{}{}
	}
	for _, label := range required {
		if _, ok := item[label]; !ok {
			return false
		}
	}
	return true
}
