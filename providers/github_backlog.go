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
)

// ListWorkItems lists GitHub issues as unified work items.
func (p *GitHubProvider) ListWorkItems(ctx context.Context, req ListWorkItemsRequest) ([]WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues")
	if err != nil {
		return nil, err
	}
	values := url.Values{"state": []string{"all"}}
	if req.State != "" {
		values.Set("state", req.State)
	}
	if len(req.Labels) > 0 {
		values.Set("labels", strings.Join(req.Labels, ","))
	}
	if req.Assignee != "" {
		values.Set("assignee", req.Assignee)
	}
	if req.UpdatedSince != nil {
		values.Set("since", req.UpdatedSince.UTC().Format(time.RFC3339))
	}
	if req.OldestFirst {
		// GitHub's issues list defaults to newest-first (sort=created,
		// direction=desc — undocumented but confirmed live, #532). An explicit
		// ascending sort makes the fetch itself FIFO, so a Limit-truncated read
		// drops the newest items (still reachable after older ones drain)
		// instead of permanently starving the oldest.
		values.Set("sort", "created")
		values.Set("direction", "asc")
	}
	pageSize := 30
	if req.Limit > 0 {
		pageSize = min(req.Limit, 100)
		if req.NeedsOversizedCandidateScan() {
			// #2067: a page capped at exactly Limit raw candidates can
			// under-match when a post-fetch filter (FieldPredicate, label
			// predicate) rejects the candidate at the truncation boundary
			// — give the fetch GitHub's own per-page ceiling (100; the
			// full candidateScanCeiling isn't reachable in one page here)
			// instead so "Limit means matches" holds for realistic
			// candidate distributions.
			pageSize = 100
		}
		values.Set("per_page", strconv.Itoa(pageSize))
	}
	callerPaged := req.Page > 0 || req.Cursor != "" || req.PageInfo != nil
	if callerPaged {
		// Page/Cursor means the caller drives pagination itself: honor it as a
		// single-page read (its own per_page, no Link following).
		page := req.Page
		offset := 0
		if page < 1 {
			page = 1
		} else {
			offset = (page - 1) * pageSize
		}
		skipped := 0
		if req.Cursor != "" {
			offset, err = strconv.Atoi(req.Cursor)
			if err != nil || offset < 0 {
				return nil, fmt.Errorf("invalid GitHub work-item cursor %q", req.Cursor)
			}
			// Resume at an arbitrary offset by reading the FULL-WIDTH page
			// that contains it and dropping the records before it, rather
			// than shrinking per_page until it divides the offset evenly
			// (#4036). That shrink made the resumed window's width a
			// property of the offset's factorization: offset 158 read a
			// 79-record page, and a prime offset collapsed per_page to 1 —
			// so a scan budgeted for backlogScanCeiling candidates examined
			// a small fraction of them and then, because the short page was
			// not itself capped, reported the result set exhausted. Full
			// pages keep "one page = up to per_page candidates" true for
			// every offset, which is what the caller's page budget assumes.
			page = offset/pageSize + 1
			skipped = offset % pageSize
		}
		values.Set("per_page", strconv.Itoa(pageSize))
		values.Set("page", strconv.Itoa(page))
		endpoint, err = addQuery(endpoint, values)
		if err != nil {
			return nil, err
		}
		var issues []githubIssue
		if err := p.do(ctx, http.MethodGet, endpoint, nil, &issues); err != nil {
			return nil, err
		}
		// fetched is the raw page width, which is what says whether GitHub
		// itself capped this read; issues is narrowed to the candidates at or
		// after the resume offset, which is what this call actually inspects.
		fetched := len(issues)
		if skipped > 0 {
			issues = issues[min(skipped, fetched):]
		}
		items, scanned, err := issuesToWorkItems(issues, req)
		if err != nil {
			return nil, err
		}
		if req.PageInfo != nil {
			// HasNext/NextCursor track the MATCH stream, not the candidate
			// stream (#2067's third acceptance criterion): issuesToWorkItems
			// can stop scanning before scanned reaches len(issues) once
			// Limit real matches are in hand, leaving fetched-but-unscanned
			// issues behind — and even scanning every fetched issue without
			// reaching Limit matches still leaves more to look at when the
			// fetch itself hit pageSize (GitHub may hold further candidates
			// beyond what this round asked for) — measured on the raw page
			// width, before the resume offset's leading records were
			// dropped, since those were still fetched.
			scannedEverything := scanned == len(issues)
			fetchWasCapped := fetched == pageSize
			req.PageInfo.CandidateCount = len(issues)
			req.PageInfo.HasNext = !scannedEverything || fetchWasCapped
			req.PageInfo.NextCursor = ""
			if req.PageInfo.HasNext {
				req.PageInfo.NextCursor = strconv.Itoa(offset + scanned)
			}
		}
		return items, nil
	}

	endpoint, err = addQuery(endpoint, values)
	if err != nil {
		return nil, err
	}
	// Preserve the legacy contract for ordinary calls: Limit counts returned
	// issues, not raw records from GitHub's mixed issues-and-pull-requests API.
	// Callers that need a bounded raw scan opt into the PageInfo/Cursor path
	// above.
	var items []WorkItem
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var issues []githubIssue
		if err := json.Unmarshal(page, &issues); err != nil {
			return fmt.Errorf("decode issues page: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequest != nil {
				continue
			}
			item := mapGitHubIssue(issue)
			matched, err := req.MatchesLabelPredicate(item.Labels)
			if err != nil {
				return err
			}
			if !matched {
				continue
			}
			matched, err = req.MatchesFieldPredicate(item.Fields)
			if err != nil {
				return err
			}
			if !matched {
				continue
			}
			items = append(items, item)
			if req.Limit > 0 && len(items) >= req.Limit {
				return errStopPaging
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return items, nil
}

// issuesToWorkItems maps a page of GitHub issues to WorkItems, skipping pull
// requests, and truncates to limit (0 = no cap). scanned reports how many
// raw issues were actually inspected before stopping — equal to len(issues)
// unless Limit real matches were reached first (#2067), so a caller-paged
// resume cursor can advance exactly past what was scanned rather than past
// every raw candidate fetched.
func issuesToWorkItems(issues []githubIssue, req ListWorkItemsRequest) ([]WorkItem, int, error) {
	items := make([]WorkItem, 0, len(issues))
	scanned := 0
	for _, issue := range issues {
		scanned++
		if issue.PullRequest != nil {
			continue
		}
		item := mapGitHubIssue(issue)
		matched, err := req.MatchesLabelPredicate(item.Labels)
		if err != nil {
			return nil, scanned, err
		}
		if !matched {
			continue
		}
		matched, err = req.MatchesFieldPredicate(item.Fields)
		if err != nil {
			return nil, scanned, err
		}
		if !matched {
			continue
		}
		items = append(items, item)
		if req.Limit > 0 && len(items) >= req.Limit {
			break
		}
	}
	return items, scanned, nil
}

// GetWorkItem reads a GitHub issue as a unified work item.
func (p *GitHubProvider) GetWorkItem(ctx context.Context, repo RepositoryRef, id string) (WorkItem, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return WorkItem{}, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id)
	if err != nil {
		return WorkItem{}, err
	}
	var issue githubIssue
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &issue); err != nil {
		return WorkItem{}, err
	}
	return mapGitHubIssue(issue), nil
}

// ListWorkItemChildren returns the provider-native sub-issues of a GitHub issue.
func (p *GitHubProvider) ListWorkItemChildren(ctx context.Context, repo RepositoryRef, id string) ([]WorkItem, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errIssueIDRequired
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "sub_issues")
	if err != nil {
		return nil, err
	}
	var items []WorkItem
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var issues []githubIssue
		if err := json.Unmarshal(page, &issues); err != nil {
			return fmt.Errorf("decode sub-issues page: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequest == nil {
				items = append(items, mapGitHubIssue(issue))
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return items, nil
}

// FindWorkItemsByMarker returns every issue whose body contains marker as an
// exact line. It scans the authoritative issues listing rather than GitHub's
// eventually-consistent search index.
func (p *GitHubProvider) FindWorkItemsByMarker(ctx context.Context, repo RepositoryRef, marker string) ([]WorkItem, error) {
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
	endpoint, err = addQuery(endpoint, url.Values{"state": []string{"all"}})
	if err != nil {
		return nil, err
	}
	var matches []WorkItem
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var issues []githubIssue
		if err := json.Unmarshal(page, &issues); err != nil {
			return fmt.Errorf("decode issues page: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequest == nil && containsExactLine(issue.Body, marker) {
				matches = append(matches, mapGitHubIssue(issue))
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return matches, nil
}

// AttachWorkItemChild attaches child to parent through GitHub's native
// sub-issues API after confirming both revisions still match the caller's
// immediately preceding reads.
func (p *GitHubProvider) AttachWorkItemChild(ctx context.Context, req AttachWorkItemChildRequest) error {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return err
	}
	if req.ParentID == "" || req.ChildID == "" {
		return fmt.Errorf("parent and child issue ids are required")
	}
	parent, err := p.GetWorkItem(ctx, req.Repository, req.ParentID)
	if err != nil {
		return err
	}
	child, err := p.GetWorkItem(ctx, req.Repository, req.ChildID)
	if err != nil {
		return err
	}
	if err := checkWorkItemRevision(parent, req.ExpectedParentRevision); err != nil {
		return err
	}
	if err := checkWorkItemRevision(child, req.ExpectedChildRevision); err != nil {
		return err
	}
	childDatabaseID, err := strconv.ParseInt(child.ExternalID, 10, 64)
	if err != nil || childDatabaseID <= 0 {
		return fmt.Errorf("child issue %q has invalid provider id %q", req.ChildID, child.ExternalID)
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.ParentID, "sub_issues")
	if err != nil {
		return err
	}
	return p.do(ctx, http.MethodPost, endpoint, map[string]int64{"sub_issue_id": childDatabaseID}, nil)
}

// ListWorkItemBlockers returns the GitHub issues and pull requests registered as
// native blockers for an issue.
func (p *GitHubProvider) ListWorkItemBlockers(ctx context.Context, repo RepositoryRef, id string) ([]WorkItem, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errIssueIDRequired
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "dependencies", "blocked_by")
	if err != nil {
		return nil, err
	}
	var blockers []WorkItem
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var issues []githubIssue
		if err := json.Unmarshal(page, &issues); err != nil {
			return fmt.Errorf("decode blocked-by dependencies page: %w", err)
		}
		for _, issue := range issues {
			blockers = append(blockers, mapGitHubIssue(issue))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return blockers, nil
}

// HasOpenWorkItemBlocker reports whether a GitHub issue has a native issue
// blocker that is still open.
func (p *GitHubProvider) HasOpenWorkItemBlocker(ctx context.Context, repo RepositoryRef, id string) (bool, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return false, err
	}
	if id == "" {
		return false, errIssueIDRequired
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "dependencies", "blocked_by")
	if err != nil {
		return false, err
	}
	open := false
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var issues []githubIssue
		if err := json.Unmarshal(page, &issues); err != nil {
			return fmt.Errorf("decode blocked-by dependencies page: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequest == nil && strings.EqualFold(issue.State, "open") {
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

// AttachWorkItemBlocker adds one native blocked-by dependency after checking
// both immediately observed revisions.
func (p *GitHubProvider) AttachWorkItemBlocker(ctx context.Context, req AttachWorkItemBlockerRequest) error {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return err
	}
	if req.ItemID == "" || req.BlockerID == "" {
		return fmt.Errorf("item and blocker issue ids are required")
	}
	item, err := p.GetWorkItem(ctx, req.Repository, req.ItemID)
	if err != nil {
		return err
	}
	blocker, err := p.GetWorkItem(ctx, req.Repository, req.BlockerID)
	if err != nil {
		return err
	}
	if err := checkWorkItemRevision(item, req.ExpectedItemRevision); err != nil {
		return err
	}
	if err := checkWorkItemRevision(blocker, req.ExpectedBlockerRevision); err != nil {
		return err
	}
	blockerDatabaseID, err := strconv.ParseInt(blocker.ExternalID, 10, 64)
	if err != nil || blockerDatabaseID <= 0 {
		return fmt.Errorf("blocker issue %q has invalid provider id %q", req.BlockerID, blocker.ExternalID)
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.ItemID, "dependencies", "blocked_by")
	if err != nil {
		return err
	}
	return p.do(ctx, http.MethodPost, endpoint, map[string]int64{"issue_id": blockerDatabaseID}, nil)
}

// CreateWorkItem creates a GitHub issue from a unified work item request.
func (p *GitHubProvider) CreateWorkItem(ctx context.Context, req CreateWorkItemRequest) (WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues")
	if err != nil {
		return WorkItem{}, err
	}
	// Idempotency (#140): a prior attempt's POST may have committed on the
	// server before its response reached us (a timeout), so a policy retry must
	// not file a duplicate. When the caller supplies a RunID we stamp a
	// run-id footer into the body and, before creating, search for an existing
	// item carrying it — returning that instead. Best-effort: GitHub's search
	// index is eventually consistent, so a retry within a second or two of the
	// original may still miss it; the footer at least makes any duplicate
	// traceable and recordExternalRef journals every create.
	itemBody := withRunIDFooter(req.Body, req.RunID)
	itemBody, err = withAttribution(itemBody, p.attribution, "issue-create")
	if err != nil {
		return WorkItem{}, err
	}
	if req.RunID != "" {
		if existing, found, err := p.findRunItem(ctx, req.Repository, req.RunID); err != nil {
			return WorkItem{}, err
		} else if found {
			return existing, nil
		}
	}
	labels := replaceStatusLabel(req.Labels, req.Status)
	body := map[string]interface{}{
		"title":  req.Title,
		"body":   itemBody,
		"labels": labels,
	}
	if req.Assignee != "" {
		body["assignees"] = []string{req.Assignee}
	}
	var issue githubIssue
	if err := p.do(ctx, http.MethodPost, endpoint, body, &issue); err != nil {
		return WorkItem{}, err
	}
	item := mapGitHubIssue(issue)
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
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

// RepositoryLabelNames lists the repository's issue-label names, read-only —
// the validator's selector-reality pass compares config selectors against
// them and must never create anything (creation is EnsureWorkItemLabels'
// job, invoked only by connect --seed).
func (p *GitHubProvider) RepositoryLabelNames(ctx context.Context, repo RepositoryRef) ([]string, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "labels")
	if err != nil {
		return nil, err
	}
	var names []string
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageLabels []githubLabel
		if err := json.Unmarshal(page, &pageLabels); err != nil {
			return fmt.Errorf("decode labels page: %w", err)
		}
		for _, label := range pageLabels {
			names = append(names, label.Name)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return names, nil
}

// ActionsWorkflowCount reports how many GitHub Actions workflows the
// repository defines — the cheap "does anything here produce check runs"
// signal the validator's ci-poll reality warning keys on. External check
// apps are invisible to this probe by design; callers must hedge.
func (p *GitHubProvider) ActionsWorkflowCount(ctx context.Context, repo RepositoryRef) (int, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return 0, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "actions", "workflows")
	if err != nil {
		return 0, err
	}
	var out struct {
		TotalCount int `json:"total_count"`
	}
	if err := p.doStatus(ctx, http.MethodGet, endpoint, nil, &out, nil); err != nil {
		return 0, err
	}
	return out.TotalCount, nil
}

// EnsureWorkItemLabels creates missing GitHub issue labels without modifying existing labels.
func (p *GitHubProvider) EnsureWorkItemLabels(
	ctx context.Context,
	repo RepositoryRef,
	labels []WorkItemLabel,
) (EnsureWorkItemLabelsResult, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return EnsureWorkItemLabelsResult{}, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "labels")
	if err != nil {
		return EnsureWorkItemLabelsResult{}, err
	}

	existing := make(map[string]bool)
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageLabels []githubLabel
		if err := json.Unmarshal(page, &pageLabels); err != nil {
			return fmt.Errorf("decode labels page: %w", err)
		}
		for _, label := range pageLabels {
			existing[strings.ToLower(label.Name)] = true
		}
		return nil
	}); err != nil {
		return EnsureWorkItemLabelsResult{}, err
	}

	result := EnsureWorkItemLabelsResult{
		Created: []string{},
		Skipped: []string{},
	}
	for _, label := range labels {
		label.Name = strings.TrimSpace(label.Name)
		label.Color = strings.TrimPrefix(strings.TrimSpace(label.Color), "#")
		if label.Name == "" || label.Color == "" {
			return EnsureWorkItemLabelsResult{}, fmt.Errorf("label name and color are required")
		}
		key := strings.ToLower(label.Name)
		if existing[key] {
			result.Skipped = append(result.Skipped, label.Name)
			continue
		}
		var created githubLabel
		if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{
			"name":        label.Name,
			"color":       label.Color,
			"description": label.Description,
		}, &created); err != nil {
			return EnsureWorkItemLabelsResult{}, fmt.Errorf("create label %q: %w", label.Name, err)
		}
		existing[key] = true
		result.Created = append(result.Created, label.Name)
	}
	return result, nil
}

// findRunItem searches the repo for an issue whose body carries the run-id
// footer for runID, used by CreateWorkItem for idempotency (#140). The search
// term is fuzzy, so the match is confirmed against the exact footer before
// returning.
func (p *GitHubProvider) findRunItem(ctx context.Context, repo RepositoryRef, runID string) (WorkItem, bool, error) {
	endpoint, err := joinURL(p.BaseURL, "search", "issues")
	if err != nil {
		return WorkItem{}, false, err
	}
	query := fmt.Sprintf(`repo:%s/%s in:body type:issue "%s"`, repo.Owner, repo.Name, runFooter(runID))
	endpoint, err = addQuery(endpoint, url.Values{"q": {query}, "per_page": {"20"}})
	if err != nil {
		return WorkItem{}, false, err
	}
	var out struct {
		Items []githubIssue `json:"items"`
	}
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return WorkItem{}, false, err
	}
	for _, issue := range out.Items {
		if strings.Contains(issue.Body, runFooter(runID)) {
			return mapGitHubIssue(issue), true, nil
		}
	}
	return WorkItem{}, false, nil
}

// UpdateWorkItemStatus mirrors Goobers processing status to GitHub labels.
func (p *GitHubProvider) UpdateWorkItemStatus(ctx context.Context, req UpdateWorkItemStatusRequest) (WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	current, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	// Swap only the status label via the label sub-API (add new, remove any
	// stale status labels) rather than PATCHing the whole label set. A
	// read-modify-write of all labels would silently clobber a label a human or
	// the curator added between our GET above and the write — the status mirror
	// has no business overwriting unrelated labels (#140).
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
		// state-only PATCH — labels are handled above, so closing never
		// round-trips (and races) the label set.
		if err := p.do(ctx, http.MethodPatch, endpoint, map[string]interface{}{"state": "closed"}, nil); err != nil {
			return WorkItem{}, err
		}
	}
	if req.Comment != "" {
		if err := p.postAttributedComment(ctx, req.Repository, req.ID, req.Comment, "state-change"); err != nil {
			return WorkItem{}, err
		}
	}
	item, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(req.Repository, req.ID),
		URL:       item.URL,
		Operation: "status",
		Fields: map[string]FieldDigest{
			"status": {Before: digestString(string(statusFromLabels(current.Labels, current.State))), After: digestString(string(req.Status))},
		},
	})
	return item, nil
}

// Subscribe emits GitHub backlog item availability events.
//
// NOT WIRED YET — banner per #140 item 5. Two issues to resolve before anyone
// depends on this: (1) the poll loop silently swallows ListWorkItems errors
// (a persistent failure looks like an empty backlog forever, not an error);
// (2) the `seen` map grows unbounded for the process lifetime. At V0 the
// scheduler triggers via cron backlog-query stages, not this in-process
// subscription, so it has no live caller; fix both before it gets one.
