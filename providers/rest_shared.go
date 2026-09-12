package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// The GitHub and Gitea REST surfaces are shape-compatible for the operations
// below, so each one lives here once, parameterized by the HTTP seam the
// calling provider satisfies (#4234). Keeping a per-provider copy meant a fix
// to one backend had no mechanism that reached the other, and the two drifted
// by omission.

// restSender is the raw request seam: one attempt plus the provider's own
// retry/rate-limit policy, with the response body still open.
type restSender interface {
	send(ctx context.Context, method, endpoint string, body interface{}) (*http.Response, error)
}

// restDoer issues a request and decodes a JSON response into out.
type restDoer interface {
	do(ctx context.Context, method, endpoint string, body, out interface{}) error
}

// restPager walks every page of a paginated collection (#139).
type restPager interface {
	getAllPages(ctx context.Context, endpoint string, onPage func([]byte) error) error
}

// restClaimReader is the seam the claim protocol needs: the full comment
// history plus the identity whose breadcrumbs are trusted.
type restClaimReader interface {
	restPager
	AuthenticatedLogin(ctx context.Context) (string, error)
}

// restMutationRecorder is the decoded-request seam plus the mutation journal
// callback shared by both REST providers.
type restMutationRecorder interface {
	restDoer
	recordExternalRef(context.Context, ExternalRef)
}

// restWorkItemMutator adds the common issue workflow used by shared updates.
// The HTTP details remain provider-owned.
type restWorkItemMutator interface {
	restMutationRecorder
	GetWorkItem(context.Context, RepositoryRef, string) (WorkItem, error)
	applyLabelChanges(context.Context, RepositoryRef, string, []string, []string) error
	postComment(context.Context, RepositoryRef, string, string) error
}

type restClaimMutationProvider interface {
	restWorkItemMutator
	restClaimReader
}

// restComment is the issue-comment payload both backends return.
type restComment struct {
	ID        int64      `json:"id"`
	Body      string     `json:"body"`
	User      githubUser `json:"user"`
	HTMLURL   string     `json:"html_url"`
	IssueURL  string     `json:"issue_url"`
	PRURL     string     `json:"pull_request_url"`
	CreatedAt *time.Time `json:"created_at"`
}

func commentMutationRef(provider ProviderKind, repo RepositoryRef, comment restComment) (ExternalRef, bool) {
	for _, rawURL := range []string{comment.PRURL, comment.IssueURL, comment.HTMLURL} {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			continue
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		for i := len(parts) - 2; i >= 0; i-- {
			if parts[i] != "issues" && parts[i] != "pulls" {
				continue
			}
			id := parts[i+1]
			if _, err := strconv.ParseUint(id, 10, 64); err != nil {
				continue
			}
			return ExternalRef{
				Provider:  provider,
				Ref:       issueRef(repo, id),
				URL:       comment.HTMLURL,
				Operation: "comment",
			}, true
		}
	}
	return ExternalRef{}, false
}

// restRepository is the repository payload both backends embed in a pull
// request's head/base branch.
type restRepository struct {
	Name    string     `json:"name"`
	HTMLURL string     `json:"html_url"`
	Owner   githubUser `json:"owner"`
}

// doStatus performs a request with the provider's transient-failure retries.
// Status codes in allowStatus are treated as success (used to tolerate a 404
// when removing a label that is not present); the response body is not decoded
// for those.
func doStatus(ctx context.Context, c restSender, method, endpoint string, body, out interface{}, allowStatus []int) error {
	resp, err := c.send(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	for _, code := range allowStatus {
		if resp.StatusCode == code {
			_ = resp.Body.Close()
			return nil
		}
	}
	return readJSONResponse(resp, method, endpoint, out)
}

// allIssueComments fetches every comment on an issue, following pagination
// (#139). Both ListComments and the claim protocol's claimWinner read the full
// comment set through here: a claim breadcrumb landing on page 2+ used to be
// invisible, so two racers each read "no claim" and both took the empty-read
// "we win" branch — a double claim on any issue with >30 comments.
func allIssueComments(ctx context.Context, c restPager, baseURL string, repo RepositoryRef, id string) ([]restComment, error) {
	endpoint, err := joinURL(baseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return nil, err
	}
	var all []restComment
	err = c.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageItems []restComment
		if err := json.Unmarshal(page, &pageItems); err != nil {
			return fmt.Errorf("decode comments page: %w", err)
		}
		all = append(all, pageItems...)
		return nil
	})
	return all, err
}

// claimWinner reads trusted issue comments and returns the run id of the recognized
// claimer in the current epoch. Only the authenticated provider identity can change
// epoch state; issue comments from other users are untrusted. A matching release
// breadcrumb ends an epoch, so stale winner and losing-racer breadcrumbs cannot
// block the next owner.
func claimWinner(ctx context.Context, c restClaimReader, baseURL string, repo RepositoryRef, id string) (string, bool, error) {
	markerAuthor, err := c.AuthenticatedLogin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("resolve claim marker author: %w", err)
	}
	raw, err := allIssueComments(ctx, c, baseURL, repo, id)
	if err != nil {
		return "", false, err
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].ID < raw[j].ID })
	winner := ""
	for _, comment := range raw {
		if !strings.EqualFold(comment.User.Login, markerAuthor) {
			continue
		}
		if releasedBy := claimReleaseRunID(comment.Body); releasedBy != "" {
			if winner == releasedBy {
				winner = ""
			}
			continue
		}
		if winner == "" {
			winner = claimRunID(comment.Body)
		}
	}
	if winner == "" {
		return "", false, nil
	}
	return winner, true, nil
}

// postAttributedComment appends an issue comment carrying the run's
// attribution marker for the named action.
func postAttributedComment(ctx context.Context, c restDoer, baseURL string, attribution Attribution, repo RepositoryRef, id, body, action string) error {
	body, err := withAttribution(body, attribution, action)
	if err != nil {
		return err
	}
	endpoint, err := joinURL(baseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return err
	}
	var ref restComment
	if err := c.do(ctx, http.MethodPost, endpoint, map[string]string{"body": body}, &ref); err != nil {
		return err
	}
	switch typed := c.(type) {
	case *GitHubProvider:
		typed.recordExternalRef(ctx, ExternalRef{
			Provider:  ProviderGitHub,
			Ref:       issueRef(repo, id),
			URL:       ref.HTMLURL,
			Operation: "comment",
		})
	case *GiteaProvider:
		typed.recordExternalRef(ctx, ExternalRef{
			Provider:  ProviderGitea,
			Ref:       issueRef(repo, id),
			URL:       ref.HTMLURL,
			Operation: "comment",
		})
	}
	return nil
}

// pullRequestComments lists a pull request's issue comments, optionally
// bounded to those updated since a watermark.
func pullRequestComments(ctx context.Context, c restPager, baseURL string, repo RepositoryRef, pullID string, since *time.Time) ([]PullRequestComment, error) {
	endpoint, err := joinURL(baseURL, "repos", repo.Owner, repo.Name, "issues", pullID, "comments")
	if err != nil {
		return nil, err
	}
	if since != nil {
		endpoint, err = addQuery(endpoint, url.Values{"since": []string{since.UTC().Format(time.RFC3339)}})
		if err != nil {
			return nil, err
		}
	}
	comments := make([]PullRequestComment, 0)
	if err := c.getAllPages(ctx, endpoint, func(page []byte) error {
		var raw []githubIssueComment
		if err := json.Unmarshal(page, &raw); err != nil {
			return fmt.Errorf("decode pull request comments page: %w", err)
		}
		for _, comment := range raw {
			comments = append(comments, PullRequestComment{
				ID: comment.ID, Author: comment.User.Login, Body: comment.Body, URL: comment.HTMLURL,
				CreatedAt: comment.CreatedAt, Integrity: apiintegrity.Unapproved,
			})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return comments, nil
}

// normalizeCombinedStatusState maps a combined commit-status state onto the
// provider-neutral check state.
func normalizeCombinedStatusState(state string) CheckState {
	switch strings.ToLower(state) {
	case "success":
		return CheckStatePassing
	case "failure", "error":
		return CheckStateFailing
	default:
		return CheckStatePending
	}
}

// repositoryRef projects a REST repository payload onto a RepositoryRef for
// the given backend.
func repositoryRef(kind ProviderKind, repo *restRepository) *RepositoryRef {
	if repo == nil {
		return nil
	}
	return &RepositoryRef{
		Provider: kind,
		Owner:    repo.Owner.Login,
		Name:     repo.Name,
		URL:      repo.HTMLURL,
	}
}

func updateRESTComment(ctx context.Context, c restMutationRecorder, kind ProviderKind, baseURL string, attribution Attribution, repo RepositoryRef, commentID, body string) error {
	if err := requireOwnerRepo(repo); err != nil {
		return err
	}
	if commentID == "" {
		return fmt.Errorf("comment id is required")
	}
	body, err := withAttribution(body, attribution, "comment-update")
	if err != nil {
		return err
	}
	endpoint, err := joinURL(baseURL, "repos", repo.Owner, repo.Name, "issues", "comments", commentID)
	if err != nil {
		return err
	}
	var comment restComment
	if err := c.do(ctx, http.MethodPatch, endpoint, map[string]string{"body": body}, &comment); err != nil {
		return err
	}
	if ref, ok := commentMutationRef(kind, repo, comment); ok {
		c.recordExternalRef(ctx, ref)
	}
	return nil
}

func createRESTWorkItemComment(ctx context.Context, c restMutationRecorder, kind ProviderKind, baseURL string, attribution Attribution, repo RepositoryRef, id, body string, mapComment func(restComment) Comment) (Comment, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return Comment{}, err
	}
	if id == "" {
		return Comment{}, errIssueIDRequired
	}
	body, err := withAttribution(body, attribution, "comment")
	if err != nil {
		return Comment{}, err
	}
	endpoint, err := joinURL(baseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return Comment{}, err
	}
	var comment restComment
	if err := c.do(ctx, http.MethodPost, endpoint, map[string]string{"body": body}, &comment); err != nil {
		return Comment{}, err
	}
	c.recordExternalRef(ctx, ExternalRef{Provider: kind, Ref: issueRef(repo, id), URL: comment.HTMLURL, Operation: "comment"})
	return mapComment(comment), nil
}

func updateRESTWorkItem(ctx context.Context, c restWorkItemMutator, kind ProviderKind, baseURL string, req UpdateWorkItemRequest) (WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	if req.ID == "" {
		return WorkItem{}, errIssueIDRequired
	}
	if req.Milestone != nil && *req.Milestone <= 0 {
		return WorkItem{}, fmt.Errorf("milestone number must be positive")
	}
	before, err := c.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	if req.ExpectedRevision != "" {
		if err := checkWorkItemRevision(before, req.ExpectedRevision); err != nil {
			return WorkItem{}, err
		}
	}

	fields := map[string]FieldDigest{}
	patch := map[string]interface{}{}
	if req.Title != nil {
		patch["title"] = *req.Title
		fields["title"] = FieldDigest{Before: digestString(before.Title), After: digestString(*req.Title)}
	}
	if req.Body != nil {
		patch["body"] = *req.Body
		fields["body"] = FieldDigest{Before: digestString(before.Body), After: digestString(*req.Body)}
	}
	if req.Assignee != nil {
		assignees := []string{}
		if *req.Assignee != "" {
			assignees = append(assignees, *req.Assignee)
		}
		patch["assignees"] = assignees
		fields["assignee"] = FieldDigest{Before: digestString(before.Assignee), After: digestString(*req.Assignee)}
	}
	if req.Milestone != nil {
		milestoneBefore := ""
		if before.Parent != nil && before.Parent.Type == "milestone" {
			milestoneBefore = before.Parent.ID
		}
		milestoneAfter := strconv.Itoa(*req.Milestone)
		patch["milestone"] = *req.Milestone
		fields["milestone"] = FieldDigest{Before: digestString(milestoneBefore), After: digestString(milestoneAfter)}
	}
	if req.State != "" {
		state := strings.ToLower(req.State)
		if state != "open" && state != "closed" {
			return WorkItem{}, fmt.Errorf("unsupported state %q (want open or closed)", req.State)
		}
		patch["state"] = state
		fields["state"] = FieldDigest{Before: digestString(before.State), After: digestString(state)}
	}
	if len(patch) > 0 {
		endpoint, err := joinURL(baseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.ID)
		if err != nil {
			return WorkItem{}, err
		}
		if err := c.do(ctx, http.MethodPatch, endpoint, patch, nil); err != nil {
			return WorkItem{}, err
		}
	}
	if req.Comment != "" {
		if err := c.postComment(ctx, req.Repository, req.ID, req.Comment); err != nil {
			return WorkItem{}, err
		}
		fields["comment"] = FieldDigest{After: digestString(req.Comment)}
	}
	if labelsChanged(req) {
		if err := c.applyLabelChanges(ctx, req.Repository, req.ID, req.AddLabels, req.RemoveLabels); err != nil {
			return WorkItem{}, err
		}
		after := applyLabelSet(before.Labels, req.AddLabels, req.RemoveLabels)
		fields["labels"] = FieldDigest{Before: digestLabels(before.Labels), After: digestLabels(after)}
	}
	final, err := c.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	if len(fields) > 0 {
		c.recordExternalRef(ctx, ExternalRef{Provider: kind, Ref: issueRef(req.Repository, req.ID), URL: final.URL, Operation: updateOperation(req), Fields: fields})
	}
	return final, nil
}

func releaseRESTWorkItemClaim(ctx context.Context, c restClaimMutationProvider, kind ProviderKind, baseURL string, attribution Attribution, req ClaimWorkItemRequest) (WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	if req.ID == "" {
		return WorkItem{}, errIssueIDRequired
	}
	if req.RunID == "" {
		return WorkItem{}, fmt.Errorf("run id is required to release an item")
	}
	label := req.ClaimLabel
	if label == "" {
		label = LabelClaimed
	}
	winner, claimed, err := claimWinner(ctx, c, baseURL, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	if claimed && winner != req.RunID && !req.LedgerAuthorized {
		return WorkItem{}, fmt.Errorf("provider claim is held by run %q", winner)
	}
	before, err := c.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	releasedRunID := req.RunID
	if claimed {
		releasedRunID = winner
		if err := postAttributedComment(ctx, c, baseURL, attribution, req.Repository, req.ID, claimReleaseBreadcrumb(winner), "claim-release"); err != nil {
			return WorkItem{}, err
		}
	}
	if before.HasLabel(label) {
		if err := c.applyLabelChanges(ctx, req.Repository, req.ID, nil, []string{label}); err != nil {
			return WorkItem{}, err
		}
	}
	final, err := c.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	c.recordExternalRef(ctx, ExternalRef{
		Provider: kind, Ref: issueRef(req.Repository, req.ID), URL: final.URL, Operation: "claim-release", Outcome: "success", RunID: req.RunID,
		Fields: map[string]FieldDigest{
			"claim":  {Before: digestString("run=" + releasedRunID), After: digestString("released")},
			"labels": {Before: digestLabels(before.Labels), After: digestLabels(final.Labels)},
		},
	})
	return final, nil
}

type restMarkerIssue struct {
	Body          string
	IsPullRequest bool
}

func findRESTWorkItemsByMarker[T any](ctx context.Context, c restPager, baseURL string, repo RepositoryRef, marker string, query url.Values, mapIssue func(T) WorkItem, issueMeta func(T) restMarkerIssue) ([]WorkItem, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if strings.TrimSpace(marker) == "" || strings.ContainsAny(marker, "\r\n") {
		return nil, fmt.Errorf("single-line work item marker is required")
	}
	endpoint, err := joinURL(baseURL, "repos", repo.Owner, repo.Name, "issues")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, query)
	if err != nil {
		return nil, err
	}
	var matches []WorkItem
	if err := c.getAllPages(ctx, endpoint, func(page []byte) error {
		var issues []T
		if err := json.Unmarshal(page, &issues); err != nil {
			return fmt.Errorf("decode issues page: %w", err)
		}
		for _, issue := range issues {
			meta := issueMeta(issue)
			if !meta.IsPullRequest && containsExactLine(meta.Body, marker) {
				matches = append(matches, mapIssue(issue))
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return matches, nil
}

type restClosedPull struct {
	Number  int    `json:"number"`
	Merged  bool   `json:"merged"`
	HTMLURL string `json:"html_url"`
}

func closeRESTPullRequest(ctx context.Context, c restMutationRecorder, kind ProviderKind, baseURL string, attribution Attribution, req ClosePullRequestRequest) (ClosePullRequestResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return ClosePullRequestResult{}, err
	}
	if req.PullID == "" {
		return ClosePullRequestResult{}, errPullIDRequired
	}
	endpoint, err := joinURL(baseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID)
	if err != nil {
		return ClosePullRequestResult{}, err
	}
	var out restClosedPull
	if err := c.do(ctx, http.MethodPatch, endpoint, map[string]string{"state": "closed"}, &out); err != nil {
		return ClosePullRequestResult{}, err
	}
	if req.Comment != "" {
		if err := postAttributedComment(ctx, c, baseURL, attribution, req.Repository, req.PullID, req.Comment, "pull-request-close"); err != nil {
			return ClosePullRequestResult{}, err
		}
	}
	state, operation := "closed", "close"
	if out.Merged {
		state, operation = "merged", "merge"
	}
	fields := map[string]FieldDigest{"state": {After: digestString(state)}}
	if req.Comment != "" {
		fields["comment"] = FieldDigest{After: digestString(req.Comment)}
	}
	c.recordExternalRef(ctx, ExternalRef{Provider: kind, Ref: issueRef(req.Repository, req.PullID), URL: out.HTMLURL, Operation: operation, Fields: fields})
	return ClosePullRequestResult{Number: out.Number, Merged: out.Merged, State: state}, nil
}

func restPullRequestFiles(ctx context.Context, c restPager, baseURL string, repo RepositoryRef, pullID string, includePatch bool) ([]ChangedFile, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if pullID == "" {
		return nil, errPullIDRequired
	}
	endpoint, err := joinURL(baseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "files")
	if err != nil {
		return nil, err
	}
	var files []githubPullRequestFile
	if err := c.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageOut []githubPullRequestFile
		if err := json.Unmarshal(page, &pageOut); err != nil {
			return fmt.Errorf("decode pull files page: %w", err)
		}
		files = append(files, pageOut...)
		return nil
	}); err != nil {
		return nil, err
	}
	out := make([]ChangedFile, 0, len(files))
	for _, f := range files {
		changed := ChangedFile{Path: f.Filename, PreviousPath: f.PreviousFilename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, Integrity: apiintegrity.Unapproved}
		if includePatch {
			changed.Patch = f.Patch
		}
		out = append(out, changed)
	}
	return out, nil
}

type restReviewResponse struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

func submitRESTPullRequestReview(ctx context.Context, c restMutationRecorder, kind ProviderKind, baseURL string, attribution Attribution, req PullRequestReviewRequest) (PullRequestReviewResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestReviewResult{}, err
	}
	if req.PullID == "" {
		return PullRequestReviewResult{}, errPullIDRequired
	}
	if req.CommitSHA == "" {
		return PullRequestReviewResult{}, fmt.Errorf("commit sha is required")
	}
	if req.Body == "" {
		return PullRequestReviewResult{}, fmt.Errorf("review body is required")
	}
	reviewBody, err := withAttribution(req.Body, attribution, "pull-request-review")
	if err != nil {
		return PullRequestReviewResult{}, err
	}
	events := map[ReviewDecision]string{ReviewDecisionChangesRequested: "REQUEST_CHANGES", ReviewDecisionComment: "COMMENT"}
	switch kind {
	case ProviderGitHub:
		events[ReviewDecisionApproved] = "APPROVE"
	case ProviderGitea:
		events[ReviewDecisionApproved] = "APPROVED"
	default:
		return PullRequestReviewResult{}, fmt.Errorf("unsupported REST review provider %q", kind)
	}
	event, ok := events[req.Decision]
	if !ok {
		return PullRequestReviewResult{}, fmt.Errorf("unsupported review decision %q", req.Decision)
	}
	endpoint, err := joinURL(baseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "reviews")
	if err != nil {
		return PullRequestReviewResult{}, err
	}
	var out restReviewResponse
	if err := c.do(ctx, http.MethodPost, endpoint, map[string]string{"body": reviewBody, "commit_id": req.CommitSHA, "event": event}, &out); err != nil {
		return PullRequestReviewResult{}, err
	}
	c.recordExternalRef(ctx, ExternalRef{
		Provider: kind, Ref: issueRef(req.Repository, req.PullID), URL: out.HTMLURL, Operation: "review",
		Fields: map[string]FieldDigest{
			"body": {After: digestString(req.Body)}, "commitSha": {After: digestString(req.CommitSHA)}, "decision": {After: digestString(string(req.Decision))},
		},
	})
	return PullRequestReviewResult{ID: out.ID, URL: out.HTMLURL, CommitSHA: req.CommitSHA, Decision: req.Decision}, nil
}
