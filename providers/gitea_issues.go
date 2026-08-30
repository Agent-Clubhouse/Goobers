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
	"github.com/goobers/goobers/internal/fieldpredicate"
)

// ListWorkItems lists Gitea issues as unified work items. type=issues is
// critical (Gitea's issue list includes pull requests otherwise); PageInfo is
// filled from the x-total-count response header; OldestFirst is a client-side
// ascending created_at sort within the fetched window (Gitea has no server-side
// sort param for issues).
func (p *GiteaProvider) ListWorkItems(ctx context.Context, req ListWorkItemsRequest) ([]WorkItem, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	values := url.Values{"type": []string{"issues"}, "state": []string{"all"}}
	if req.State != "" {
		values.Set("state", req.State)
	}
	if len(req.Labels) > 0 {
		values.Set("labels", strings.Join(req.Labels, ","))
	}
	if req.UpdatedSince != nil {
		values.Set("since", req.UpdatedSince.UTC().Format(time.RFC3339))
	}
	pageSize := 30
	if req.Limit > 0 {
		pageSize = min(req.Limit, 50)
	}

	callerPaged := req.Page > 0 || req.Cursor != "" || req.PageInfo != nil
	if callerPaged {
		page := req.Page
		if page < 1 {
			page = 1
		}
		if req.Cursor != "" {
			n, err := strconv.Atoi(req.Cursor)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("invalid gitea work-item cursor %q", req.Cursor)
			}
			page = n
		}
		values.Set("page", strconv.Itoa(page))
		values.Set("limit", strconv.Itoa(pageSize))
		endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues")
		if err != nil {
			return nil, err
		}
		endpoint, err = addQuery(endpoint, values)
		if err != nil {
			return nil, err
		}
		issues, total, err := p.listIssuesPage(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		if req.PageInfo != nil {
			req.PageInfo.CandidateCount = len(issues)
			req.PageInfo.HasNext = page*pageSize < total
			req.PageInfo.NextCursor = ""
			if req.PageInfo.HasNext {
				req.PageInfo.NextCursor = strconv.Itoa(page + 1)
			}
		}
		return giteaIssuesToWorkItems(issues, req)
	}

	// Ordinary call: page through until Limit is satisfied. OldestFirst sorts
	// the accumulated window ascending so a Limit truncation drops the newest.
	var raw []giteaIssue
	values.Set("limit", strconv.Itoa(pageSize))
	for page := 1; ; page++ {
		values.Set("page", strconv.Itoa(page))
		endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues")
		if err != nil {
			return nil, err
		}
		endpoint, err = addQuery(endpoint, values)
		if err != nil {
			return nil, err
		}
		issues, total, err := p.listIssuesPage(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		raw = append(raw, issues...)
		if len(issues) < pageSize || (total > 0 && len(raw) >= total) {
			break
		}
		// Bound the sweep: once we have enough candidates for Limit (predicates
		// aside), keep one extra page of headroom then stop.
		if req.Limit > 0 && len(raw) >= req.Limit+pageSize {
			break
		}
	}
	if req.OldestFirst {
		sort.SliceStable(raw, func(i, j int) bool {
			return giteaCreatedBefore(raw[i], raw[j])
		})
	}
	return giteaIssuesToWorkItems(raw, req)
}

func giteaCreatedBefore(a, b giteaIssue) bool {
	if a.CreatedAt == nil || b.CreatedAt == nil {
		return a.Number < b.Number
	}
	if a.CreatedAt.Equal(*b.CreatedAt) {
		return a.Number < b.Number
	}
	return a.CreatedAt.Before(*b.CreatedAt)
}

// listIssuesPage fetches one issues page and the x-total-count header.
func (p *GiteaProvider) listIssuesPage(ctx context.Context, endpoint string) ([]giteaIssue, int, error) {
	resp, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	body, _, err := readPage(resp, http.MethodGet, endpoint)
	if err != nil {
		return nil, 0, err
	}
	var issues []giteaIssue
	if err := json.Unmarshal(body, &issues); err != nil {
		return nil, 0, fmt.Errorf("decode issues page: %w", err)
	}
	total := 0
	if raw := strings.TrimSpace(resp.Header.Get("x-total-count")); raw != "" {
		if n, convErr := strconv.Atoi(raw); convErr == nil {
			total = n
		}
	}
	return issues, total, nil
}

func giteaIssuesToWorkItems(issues []giteaIssue, req ListWorkItemsRequest) ([]WorkItem, error) {
	items := make([]WorkItem, 0, len(issues))
	for _, issue := range issues {
		if issue.PullRequest != nil {
			continue
		}
		item := mapGiteaIssue(issue)
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
		if !matched {
			continue
		}
		items = append(items, item)
		if req.Limit > 0 && len(items) >= req.Limit {
			break
		}
	}
	return items, nil
}

// GetWorkItem reads a Gitea issue as a unified work item.
func (p *GiteaProvider) GetWorkItem(ctx context.Context, repo RepositoryRef, id string) (WorkItem, error) {
	if err := p.ready(); err != nil {
		return WorkItem{}, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return WorkItem{}, err
	}
	if id == "" {
		return WorkItem{}, errIssueIDRequired
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id)
	if err != nil {
		return WorkItem{}, err
	}
	var issue giteaIssue
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &issue); err != nil {
		return WorkItem{}, err
	}
	return mapGiteaIssue(issue), nil
}

// FindWorkItemsByMarker scans the authoritative issue listing for an exact
// single-line body marker.
func (p *GiteaProvider) FindWorkItemsByMarker(ctx context.Context, repo RepositoryRef, marker string) ([]WorkItem, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if strings.TrimSpace(marker) == "" || strings.ContainsAny(marker, "\r\n") {
		return nil, fmt.Errorf("single-line work item marker is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"state": []string{"all"}, "type": []string{"issues"}})
	if err != nil {
		return nil, err
	}
	var matches []WorkItem
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var issues []giteaIssue
		if err := json.Unmarshal(page, &issues); err != nil {
			return fmt.Errorf("decode issues page: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequest == nil && containsExactLine(issue.Body, marker) {
				matches = append(matches, mapGiteaIssue(issue))
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return matches, nil
}

// ListComments returns the comments on a Gitea issue, oldest first.
func (p *GiteaProvider) ListComments(ctx context.Context, repo RepositoryRef, id string) ([]Comment, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errIssueIDRequired
	}
	raw, err := p.allIssueComments(ctx, repo, id)
	if err != nil {
		return nil, err
	}
	comments := make([]Comment, 0, len(raw))
	for _, c := range raw {
		comments = append(comments, mapGiteaComment(c))
	}
	return comments, nil
}

func (p *GiteaProvider) allIssueComments(ctx context.Context, repo RepositoryRef, id string) ([]giteaComment, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return nil, err
	}
	var all []giteaComment
	err = p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageItems []giteaComment
		if err := json.Unmarshal(page, &pageItems); err != nil {
			return fmt.Errorf("decode comments page: %w", err)
		}
		all = append(all, pageItems...)
		return nil
	})
	return all, err
}

// AuthenticatedLogin returns the Gitea login the provider's credential
// represents.
func (p *GiteaProvider) AuthenticatedLogin(ctx context.Context) (string, error) {
	if err := p.ready(); err != nil {
		return "", err
	}
	endpoint, err := joinURL(p.BaseURL, "user")
	if err != nil {
		return "", err
	}
	var user githubUser
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &user); err != nil {
		return "", err
	}
	login := strings.TrimSpace(user.Login)
	if login == "" {
		return "", fmt.Errorf("authenticated gitea user has no login")
	}
	return login, nil
}

// UpdateComment edits an existing issue/PR comment's body in place — the
// sticky-comment pattern (#716) a caller uses so a repeated event (e.g.
// pr-remediation's per-cycle checkpoint/escalation state) updates the SAME
// comment instead of growing a new one every run. Gitea scopes comment IDs
// repo-wide, not per-issue, so the edit endpoint takes no issue number.
func (p *GiteaProvider) UpdateComment(ctx context.Context, repo RepositoryRef, commentID, body string) error {
	if err := p.ready(); err != nil {
		return err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return err
	}
	if commentID == "" {
		return fmt.Errorf("comment id is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", "comments", commentID)
	if err != nil {
		return err
	}
	return p.do(ctx, http.MethodPatch, endpoint, map[string]string{"body": body}, nil)
}

// DeleteComment removes an issue/PR comment. A missing comment is already in
// the desired state, so retries treat 404 as success.
func (p *GiteaProvider) DeleteComment(ctx context.Context, repo RepositoryRef, commentID string) error {
	if err := p.ready(); err != nil {
		return err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return err
	}
	if commentID == "" {
		return fmt.Errorf("comment id is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", "comments", commentID)
	if err != nil {
		return err
	}
	return p.doStatus(ctx, http.MethodDelete, endpoint, nil, nil, []int{http.StatusNotFound})
}

// CreateWorkItemComment appends one issue comment and returns its identity.
func (p *GiteaProvider) CreateWorkItemComment(ctx context.Context, repo RepositoryRef, id, body string) (Comment, error) {
	if err := p.ready(); err != nil {
		return Comment{}, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return Comment{}, err
	}
	if id == "" {
		return Comment{}, errIssueIDRequired
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return Comment{}, err
	}
	var comment giteaComment
	if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{"body": body}, &comment); err != nil {
		return Comment{}, err
	}
	return mapGiteaComment(comment), nil
}

// CreateWorkItem creates a Gitea issue. Gitea takes label IDs, not names, so
// requested labels are resolved (or created) via giteaLabelIDs. When RunID is
// set the body carries a run-id footer and a recent-window scan makes creation
// idempotent (Gitea's q= searches titles only, so the match is a client-side
// body-footer scan).
func (p *GiteaProvider) CreateWorkItem(ctx context.Context, req CreateWorkItemRequest) (WorkItem, error) {
	if err := p.ready(); err != nil {
		return WorkItem{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	itemBody := withRunIDFooter(req.Body, req.RunID)
	if req.RunID != "" {
		if existing, found, err := p.findRunItem(ctx, req.Repository, req.RunID); err != nil {
			return WorkItem{}, err
		} else if found {
			return existing, nil
		}
	}
	labelNames := replaceStatusLabel(req.Labels, req.Status)
	labelIDs, err := p.giteaLabelIDs(ctx, req.Repository, labelNames)
	if err != nil {
		return WorkItem{}, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues")
	if err != nil {
		return WorkItem{}, err
	}
	body := map[string]interface{}{
		"title": req.Title,
		"body":  itemBody,
	}
	if len(labelIDs) > 0 {
		body["labels"] = labelIDs
	}
	if req.Assignee != "" {
		body["assignees"] = []string{req.Assignee}
	}
	var issue giteaIssue
	if err := p.do(ctx, http.MethodPost, endpoint, body, &issue); err != nil {
		return WorkItem{}, err
	}
	item := mapGiteaIssue(issue)
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
		Ref:       issueRef(req.Repository, strconv.Itoa(issue.Number)),
		URL:       item.URL,
		Operation: "create",
		RunID:     req.RunID,
		Fields: map[string]FieldDigest{
			"title": {After: digestString(req.Title)},
			"body":  {After: digestString(itemBody)},
		},
	})
	return item, nil
}

// findRunItem scans a recent window of issues for one whose body carries the
// run-id footer, used by CreateWorkItem for idempotency (#140).
func (p *GiteaProvider) findRunItem(ctx context.Context, repo RepositoryRef, runID string) (WorkItem, bool, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues")
	if err != nil {
		return WorkItem{}, false, err
	}
	endpoint, err = addQuery(endpoint, url.Values{
		"type":  []string{"issues"},
		"state": []string{"all"},
		"limit": []string{"50"},
	})
	if err != nil {
		return WorkItem{}, false, err
	}
	issues, _, err := p.listIssuesPage(ctx, endpoint)
	if err != nil {
		return WorkItem{}, false, err
	}
	footer := runFooter(runID)
	for _, issue := range issues {
		if issue.PullRequest == nil && strings.Contains(issue.Body, footer) {
			return mapGiteaIssue(issue), true, nil
		}
	}
	return WorkItem{}, false, nil
}

// UpdateWorkItem applies title/body edits, assignee changes, label add/remove,
// milestone assignment, open/close, and an optional comment to a Gitea issue.
// Only fields the caller set are touched.
func (p *GiteaProvider) UpdateWorkItem(ctx context.Context, req UpdateWorkItemRequest) (WorkItem, error) {
	if err := p.ready(); err != nil {
		return WorkItem{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	if req.ID == "" {
		return WorkItem{}, errIssueIDRequired
	}
	if req.Milestone != nil && *req.Milestone <= 0 {
		return WorkItem{}, fmt.Errorf("milestone number must be positive")
	}
	before, err := p.GetWorkItem(ctx, req.Repository, req.ID)
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
		patch["milestone"] = *req.Milestone
		fields["milestone"] = FieldDigest{Before: digestString(milestoneBefore), After: digestString(strconv.Itoa(*req.Milestone))}
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
		endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.ID)
		if err != nil {
			return WorkItem{}, err
		}
		if err := p.do(ctx, http.MethodPatch, endpoint, patch, nil); err != nil {
			return WorkItem{}, err
		}
	}

	if req.Comment != "" {
		if err := p.postComment(ctx, req.Repository, req.ID, req.Comment); err != nil {
			return WorkItem{}, err
		}
		fields["comment"] = FieldDigest{After: digestString(req.Comment)}
	}

	if labelsChanged(req) {
		if err := p.applyLabelChanges(ctx, req.Repository, req.ID, req.AddLabels, req.RemoveLabels); err != nil {
			return WorkItem{}, err
		}
		after := applyLabelSet(before.Labels, req.AddLabels, req.RemoveLabels)
		fields["labels"] = FieldDigest{Before: digestLabels(before.Labels), After: digestLabels(after)}
	}

	final, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	if len(fields) > 0 {
		p.recordExternalRef(ctx, ExternalRef{
			Provider:  ProviderGitea,
			Ref:       issueRef(req.Repository, req.ID),
			URL:       final.URL,
			Operation: updateOperation(req),
			Fields:    fields,
		})
	}
	return final, nil
}

// UpdateWorkItemStatus mirrors Goobers processing status to Gitea labels,
// swapping only the status label (add new, remove stale) rather than clobbering
// the whole label set.
func (p *GiteaProvider) UpdateWorkItemStatus(ctx context.Context, req UpdateWorkItemStatusRequest) (WorkItem, error) {
	if err := p.ready(); err != nil {
		return WorkItem{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	current, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	newLabel := statusLabel(req.Status)
	var remove []string
	for _, l := range current.Labels {
		if strings.HasPrefix(l, statusLabelPrefix) && l != newLabel {
			remove = append(remove, l)
		}
	}
	if err := p.applyLabelChanges(ctx, req.Repository, req.ID, []string{newLabel}, remove); err != nil {
		return WorkItem{}, err
	}
	if req.Status == WorkItemStatusDone {
		endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.ID)
		if err != nil {
			return WorkItem{}, err
		}
		if err := p.do(ctx, http.MethodPatch, endpoint, map[string]interface{}{"state": "closed"}, nil); err != nil {
			return WorkItem{}, err
		}
	}
	if req.Comment != "" {
		if err := p.postComment(ctx, req.Repository, req.ID, req.Comment); err != nil {
			return WorkItem{}, err
		}
	}
	item, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
		Ref:       issueRef(req.Repository, req.ID),
		URL:       item.URL,
		Operation: "status",
		Fields: map[string]FieldDigest{
			"status": {Before: digestString(string(statusFromLabels(current.Labels, current.State))), After: digestString(string(req.Status))},
		},
	})
	return item, nil
}

// ClaimWorkItem writes a best-effort claiming marker (a label plus a run-id
// breadcrumb comment) so concurrent runs never double-process an item. The
// winner is the run whose breadcrumb has the smallest server-assigned comment
// id in the current claim epoch. The comment-breadcrumb race protocol is
// carried over from the GitHub provider verbatim.
func (p *GiteaProvider) ClaimWorkItem(ctx context.Context, req ClaimWorkItemRequest) (ClaimResult, error) {
	if err := p.ready(); err != nil {
		return ClaimResult{}, err
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return ClaimResult{}, err
	}
	if req.ID == "" {
		return ClaimResult{}, errIssueIDRequired
	}
	if req.RunID == "" {
		return ClaimResult{}, fmt.Errorf("run id is required to claim an item")
	}
	label := req.ClaimLabel
	if label == "" {
		label = LabelClaimed
	}

	if winner, ok, err := p.claimWinner(ctx, req.Repository, req.ID); err != nil {
		return ClaimResult{}, err
	} else if ok {
		return p.finishClaim(ctx, req.Repository, req.ID, req.RunID, winner)
	}

	if err := p.postComment(ctx, req.Repository, req.ID, claimBreadcrumb(req.RunID)); err != nil {
		return ClaimResult{}, err
	}
	winner, ok, err := p.claimWinner(ctx, req.Repository, req.ID)
	if err != nil {
		return ClaimResult{}, err
	}
	if !ok {
		winner = req.RunID
	}
	if winner == req.RunID {
		if err := p.applyLabelChanges(ctx, req.Repository, req.ID, []string{label}, nil); err != nil {
			return ClaimResult{}, err
		}
	}
	return p.finishClaim(ctx, req.Repository, req.ID, req.RunID, winner)
}

// ReleaseWorkItemClaim ends the current provider claim epoch and removes its
// label mirror.
func (p *GiteaProvider) ReleaseWorkItemClaim(ctx context.Context, req ClaimWorkItemRequest) (WorkItem, error) {
	if err := p.ready(); err != nil {
		return WorkItem{}, err
	}
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

	winner, claimed, err := p.claimWinner(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	if claimed && winner != req.RunID && !req.LedgerAuthorized {
		return WorkItem{}, fmt.Errorf("provider claim is held by run %q", winner)
	}
	before, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	releasedRunID := req.RunID
	if claimed {
		releasedRunID = winner
		if err := p.postComment(ctx, req.Repository, req.ID, claimReleaseBreadcrumb(winner)); err != nil {
			return WorkItem{}, err
		}
	}
	if before.HasLabel(label) {
		if err := p.applyLabelChanges(ctx, req.Repository, req.ID, nil, []string{label}); err != nil {
			return WorkItem{}, err
		}
	}
	final, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
		Ref:       issueRef(req.Repository, req.ID),
		URL:       final.URL,
		Operation: "claim-release",
		RunID:     req.RunID,
		Fields: map[string]FieldDigest{
			"claim":  {Before: digestString("run=" + releasedRunID), After: digestString("released")},
			"labels": {Before: digestLabels(before.Labels), After: digestLabels(final.Labels)},
		},
	})
	return final, nil
}

// finishClaim loads the final item, records the claim mutation, and reports
// whether runID is the recognized winner.
func (p *GiteaProvider) finishClaim(ctx context.Context, repo RepositoryRef, id, runID, winner string) (ClaimResult, error) {
	item, err := p.GetWorkItem(ctx, repo, id)
	if err != nil {
		return ClaimResult{}, err
	}
	claimed := winner == runID
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitea,
		Ref:       issueRef(repo, id),
		URL:       item.URL,
		Operation: "claim",
		RunID:     runID,
		Fields: map[string]FieldDigest{
			"claim": {After: digestString("run=" + winner)},
		},
	})
	return ClaimResult{Claimed: claimed, ClaimedBy: winner, Item: item}, nil
}

// claimWinner reads trusted issue comments and returns the run id of the
// recognized claimer in the current epoch. Only the authenticated provider
// identity can change epoch state.
func (p *GiteaProvider) claimWinner(ctx context.Context, repo RepositoryRef, id string) (string, bool, error) {
	markerAuthor, err := p.AuthenticatedLogin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("resolve claim marker author: %w", err)
	}
	raw, err := p.allIssueComments(ctx, repo, id)
	if err != nil {
		return "", false, err
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].ID < raw[j].ID })
	winner := ""
	for _, c := range raw {
		if !strings.EqualFold(c.User.Login, markerAuthor) {
			continue
		}
		if releasedBy := claimReleaseRunID(c.Body); releasedBy != "" {
			if winner == releasedBy {
				winner = ""
			}
			continue
		}
		if winner == "" {
			winner = claimRunID(c.Body)
		}
	}
	if winner == "" {
		return "", false, nil
	}
	return winner, true, nil
}

// HasOpenWorkItemBlocker reports whether a Gitea issue has a native dependency
// that is still open. Gitea has native issue dependencies.
func (p *GiteaProvider) HasOpenWorkItemBlocker(ctx context.Context, repo RepositoryRef, id string) (bool, error) {
	if err := p.ready(); err != nil {
		return false, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return false, err
	}
	if id == "" {
		return false, errIssueIDRequired
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "dependencies")
	if err != nil {
		return false, err
	}
	open := false
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var deps []giteaIssue
		if err := json.Unmarshal(page, &deps); err != nil {
			return fmt.Errorf("decode dependencies page: %w", err)
		}
		for _, dep := range deps {
			if dep.PullRequest == nil && strings.EqualFold(dep.State, "open") {
				open = true
				return errStopPaging
			}
		}
		return nil
	}); err != nil {
		return false, err
	}
	return open, nil
}

// ListWorkItemLabelTransitionsForItem returns one issue's label add/remove
// history from Gitea's timeline (>=1.14), filtering type=="label" events. A
// label timeline entry's Content is "1" for an addition and empty for a removal.
func (p *GiteaProvider) ListWorkItemLabelTransitionsForItem(ctx context.Context, repo RepositoryRef, id, label string) ([]WorkItemLabelTransition, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errIssueIDRequired
	}
	if label == "" {
		return nil, fmt.Errorf("label is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "timeline")
	if err != nil {
		return nil, err
	}
	var transitions []WorkItemLabelTransition
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var events []giteaTimelineComment
		if err := json.Unmarshal(page, &events); err != nil {
			return fmt.Errorf("decode timeline page: %w", err)
		}
		for _, event := range events {
			if event.Type != "label" || event.Label == nil || event.Label.Name != label {
				continue
			}
			transitions = append(transitions, WorkItemLabelTransition{
				EventID:    event.ID,
				ItemID:     id,
				Label:      label,
				Added:      strings.TrimSpace(event.Content) == "1",
				OccurredAt: event.CreatedAt,
			})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(transitions, func(i, j int) bool {
		if transitions[i].OccurredAt.Equal(transitions[j].OccurredAt) {
			return transitions[i].EventID < transitions[j].EventID
		}
		return transitions[i].OccurredAt.Before(transitions[j].OccurredAt)
	})
	return transitions, nil
}

// EnsureWorkItemLabels creates missing Gitea issue labels without modifying
// existing labels.
func (p *GiteaProvider) EnsureWorkItemLabels(ctx context.Context, repo RepositoryRef, labels []WorkItemLabel) (EnsureWorkItemLabelsResult, error) {
	if err := p.ready(); err != nil {
		return EnsureWorkItemLabelsResult{}, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return EnsureWorkItemLabelsResult{}, err
	}
	existing, err := p.listRepoLabels(ctx, repo)
	if err != nil {
		return EnsureWorkItemLabelsResult{}, err
	}
	have := make(map[string]bool, len(existing))
	for _, l := range existing {
		have[strings.ToLower(l.Name)] = true
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "labels")
	if err != nil {
		return EnsureWorkItemLabelsResult{}, err
	}
	result := EnsureWorkItemLabelsResult{Created: []string{}, Skipped: []string{}}
	for _, label := range labels {
		label.Name = strings.TrimSpace(label.Name)
		label.Color = strings.TrimPrefix(strings.TrimSpace(label.Color), "#")
		if label.Name == "" || label.Color == "" {
			return EnsureWorkItemLabelsResult{}, fmt.Errorf("label name and color are required")
		}
		key := strings.ToLower(label.Name)
		if have[key] {
			result.Skipped = append(result.Skipped, label.Name)
			continue
		}
		var created giteaLabel
		if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{
			"name":        label.Name,
			"color":       label.Color,
			"description": label.Description,
		}, &created); err != nil {
			return EnsureWorkItemLabelsResult{}, fmt.Errorf("create label %q: %w", label.Name, err)
		}
		have[key] = true
		result.Created = append(result.Created, label.Name)
	}
	return result, nil
}

// Subscribe emits Gitea backlog item availability events by polling
// ListWorkItems. TriggerWebhook is unsupported.
func (p *GiteaProvider) Subscribe(ctx context.Context, sub TriggerSubscription) (<-chan WorkItemEvent, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	if sub.Kind != TriggerPolling {
		return nil, fmt.Errorf("gitea provider does not support webhook triggers")
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
			items, err := p.ListWorkItems(ctx, ListWorkItemsRequest{Repository: sub.Repository, State: "open", Limit: 100})
			if err == nil {
				for _, item := range items {
					if !shouldEmitWorkItem(seen, item) {
						continue
					}
					select {
					case <-ctx.Done():
						return
					case events <- WorkItemEvent{Provider: ProviderGitea, Kind: TriggerPolling, Item: item, Action: "available"}:
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

// --- label ID resolution (Gitea labels are ID-based, not name-based) ---

// giteaLabelIDs resolves label names to Gitea label IDs, creating any that do
// not yet exist. A create that loses a concurrent race (409) re-fetches and
// resolves the now-existing label.
func (p *GiteaProvider) giteaLabelIDs(ctx context.Context, repo RepositoryRef, names []string) ([]int64, error) {
	names = uniqueStrings(names)
	if len(names) == 0 {
		return nil, nil
	}
	existing, err := p.listRepoLabels(ctx, repo)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]int64, len(existing))
	for _, l := range existing {
		byName[strings.ToLower(l.Name)] = l.ID
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "labels")
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(names))
	for _, name := range names {
		key := strings.ToLower(name)
		if id, ok := byName[key]; ok {
			ids = append(ids, id)
			continue
		}
		var created giteaLabel
		if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{
			"name":  name,
			"color": "#ededed",
		}, &created); err != nil {
			// Lost a create race: re-fetch and resolve.
			refreshed, refErr := p.listRepoLabels(ctx, repo)
			if refErr != nil {
				return nil, fmt.Errorf("resolve label %q after create failure: %w", name, err)
			}
			found := false
			for _, l := range refreshed {
				if strings.EqualFold(l.Name, name) {
					byName[key] = l.ID
					ids = append(ids, l.ID)
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("create label %q: %w", name, err)
			}
			continue
		}
		byName[key] = created.ID
		ids = append(ids, created.ID)
	}
	return ids, nil
}

func (p *GiteaProvider) listRepoLabels(ctx context.Context, repo RepositoryRef) ([]giteaLabel, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "labels")
	if err != nil {
		return nil, err
	}
	var all []giteaLabel
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageLabels []giteaLabel
		if err := json.Unmarshal(page, &pageLabels); err != nil {
			return fmt.Errorf("decode labels page: %w", err)
		}
		all = append(all, pageLabels...)
		return nil
	}); err != nil {
		return nil, err
	}
	return all, nil
}

// applyLabelChanges adds and removes labels by resolving names to Gitea label
// IDs. Add posts the ID set; each removal is a DELETE of one label id,
// tolerating a 404 when the label is not present.
func (p *GiteaProvider) applyLabelChanges(ctx context.Context, repo RepositoryRef, id string, add, remove []string) error {
	if add = uniqueStrings(add); len(add) > 0 {
		addIDs, err := p.giteaLabelIDs(ctx, repo, add)
		if err != nil {
			return err
		}
		endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "labels")
		if err != nil {
			return err
		}
		if err := p.do(ctx, http.MethodPost, endpoint, map[string][]int64{"labels": addIDs}, nil); err != nil {
			return err
		}
	}
	remove = uniqueStrings(remove)
	if len(remove) == 0 {
		return nil
	}
	removeIDs, err := p.resolveExistingLabelIDs(ctx, repo, remove)
	if err != nil {
		return err
	}
	for _, labelID := range removeIDs {
		endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "labels", strconv.FormatInt(labelID, 10))
		if err != nil {
			return err
		}
		if err := p.doStatus(ctx, http.MethodDelete, endpoint, nil, nil, []int{http.StatusNotFound}); err != nil {
			return err
		}
	}
	return nil
}

// resolveExistingLabelIDs maps names to IDs for labels that already exist,
// silently skipping unknown names (removing a label that was never created is a
// no-op, matching the GitHub provider's 404-tolerant removal).
func (p *GiteaProvider) resolveExistingLabelIDs(ctx context.Context, repo RepositoryRef, names []string) ([]int64, error) {
	existing, err := p.listRepoLabels(ctx, repo)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]int64, len(existing))
	for _, l := range existing {
		byName[strings.ToLower(l.Name)] = l.ID
	}
	ids := make([]int64, 0, len(names))
	for _, name := range names {
		if id, ok := byName[strings.ToLower(name)]; ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (p *GiteaProvider) postComment(ctx context.Context, repo RepositoryRef, id, body string) error {
	_, err := p.CreateWorkItemComment(ctx, repo, id, body)
	return err
}

// --- Gitea issue/comment/timeline decode structs and mappers ---

type giteaIssue struct {
	ID        int64           `json:"id"`
	Number    int             `json:"number"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	State     string          `json:"state"`
	Comments  int             `json:"comments"`
	HTMLURL   string          `json:"html_url"`
	User      githubUser      `json:"user"`
	Labels    []giteaLabel    `json:"labels"`
	Assignees []githubUser    `json:"assignees"`
	Milestone *giteaMilestone `json:"milestone"`
	CreatedAt *time.Time      `json:"created_at"`
	UpdatedAt *time.Time      `json:"updated_at"`
	// PullRequest is non-nil when this "issue" is actually a pull request.
	PullRequest *githubPullRequestLink `json:"pull_request"`
}

type giteaMilestone struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

type giteaComment struct {
	ID        int64      `json:"id"`
	Body      string     `json:"body"`
	User      githubUser `json:"user"`
	HTMLURL   string     `json:"html_url"`
	CreatedAt *time.Time `json:"created_at"`
}

type giteaTimelineComment struct {
	ID        int64       `json:"id"`
	Type      string      `json:"type"`
	Content   string      `json:"body"`
	Label     *giteaLabel `json:"label"`
	CreatedAt time.Time   `json:"created_at"`
}

func mapGiteaComment(c giteaComment) Comment {
	return Comment{
		ID:        strconv.FormatInt(c.ID, 10),
		Author:    c.User.Login,
		Body:      c.Body,
		CreatedAt: c.CreatedAt,
		URL:       c.HTMLURL,
		Integrity: apiintegrity.Unapproved,
	}
}

func mapGiteaIssue(issue giteaIssue) WorkItem {
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.Name)
	}
	links := []Link{{Rel: "self", URL: issue.HTMLURL}}
	var parent *WorkItemRef
	hierarchy := map[string]interface{}{}
	if issue.Milestone != nil {
		parent = &WorkItemRef{Provider: ProviderGitea, ID: strconv.FormatInt(issue.Milestone.ID, 10), Type: "milestone"}
		hierarchy["milestone"] = issue.Milestone
	}
	assignee := ""
	if len(issue.Assignees) > 0 {
		assignee = issue.Assignees[0].Login
	}
	return WorkItem{
		Provider:   ProviderGitea,
		ID:         strconv.Itoa(issue.Number),
		ExternalID: strconv.FormatInt(issue.ID, 10),
		Revision:   timeRevision(issue.UpdatedAt),
		Type:       "issue",
		Title:      issue.Title,
		Body:       issue.Body,
		Labels:     labels,
		State:      issue.State,
		Status:     statusFromLabels(labels, issue.State),
		Assignee:   assignee,
		Links:      links,
		Parent:     parent,
		Hierarchy:  hierarchy,
		URL:        issue.HTMLURL,
		CreatedAt:  issue.CreatedAt,
		UpdatedAt:  issue.UpdatedAt,
		Fields:     giteaIssueFields(issue),
		Raw:        issue,
		Integrity:  apiintegrity.Unapproved,
	}
}

func giteaIssueFields(issue giteaIssue) fieldpredicate.Fields {
	fields := fieldpredicate.Fields{
		"id":       issue.ID,
		"number":   int64(issue.Number),
		"state":    issue.State,
		"comments": int64(issue.Comments),
	}
	if issue.User.Login != "" {
		fields["user.login"] = issue.User.Login
	}
	if issue.CreatedAt != nil {
		fields["created_at"] = issue.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if issue.UpdatedAt != nil {
		fields["updated_at"] = issue.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	if len(issue.Assignees) > 0 {
		fields["assignee.login"] = issue.Assignees[0].Login
	}
	if issue.Milestone != nil {
		fields["milestone.title"] = issue.Milestone.Title
	}
	return fields
}
