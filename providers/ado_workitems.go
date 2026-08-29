package providers

import (
	"context"
	"encoding/base64"
	"errors"
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

const (
	adoCommentPageSize = 200
	adoWIQLPageSize    = 20000
	adoClaimRetries    = 4
	adoMaxTagLength    = 400
	adoClaimTagPrefix  = "goobers:claim-run:"
)

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
	// candidateLimit is what WIQL's $top actually requests — req.Limit,
	// unless a post-WIQL filter could reject a candidate (#2067), in which
	// case the raw fetch is given an oversized ceiling so "Limit = up to N
	// matches" holds regardless of where in the true result set the
	// matches land, not just "N raw records happened to be returned". Two
	// ADO-specific conditions are added on top of the shared
	// NeedsOversizedCandidateScan (LabelPredicate/FieldPredicate, safe for
	// both providers): requestedState "open"/"closed" needs each
	// candidate's process-specific state category read before it can be
	// compared (not a raw WIQL-comparable value), and req.Labels'
	// server-side WIQL CONTAINS is a substring match — hasAllLabels' exact
	// client-side recheck can reject a CONTAINS false-positive. Neither
	// condition is added to the shared check: GitHub's own `state`/`labels`
	// query params filter both reliably server-side, so applying ADO's
	// reasoning there would make cmd/goobers/backlogquery.go's strict
	// per-page candidate-count invariant fail for a caller that never hits
	// ADO at all.
	candidateLimit := req.Limit
	adoNeedsOversizedScan := req.NeedsOversizedCandidateScan() ||
		requestedState == "open" || requestedState == "closed" ||
		len(req.Labels) > 0
	if boundedScan && req.Limit > 0 && adoNeedsOversizedScan {
		candidateLimit = max(req.Limit, candidateScanCeiling)
	}
	if boundedScan && candidateLimit > 0 {
		endpoint, err = addQuery(endpoint, url.Values{"$top": []string{strconv.Itoa(candidateLimit)}})
		if err != nil {
			return nil, err
		}
	}
	var wiql adoWIQLResponse
	if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{"query": query}, &wiql); err != nil {
		return nil, err
	}
	refs := wiql.WorkItems
	if boundedScan && candidateLimit > 0 {
		refs = refs[:min(candidateLimit, len(refs))]
	}
	items := make([]WorkItem, 0, len(refs))
	lastScanned := -1
	for i, ref := range refs {
		lastScanned = i
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
			// Stop once Limit real matches are in hand, whether bounded or
			// not (#2067): with an oversized candidate fetch, scanning the
			// remaining candidates after the caller's Limit is already
			// satisfied would only waste GetWorkItem round trips.
			if req.Limit > 0 && len(items) >= req.Limit {
				break
			}
		}
	}
	if req.PageInfo != nil {
		// CandidateCount is how many candidates were actually INSPECTED
		// this round (lastScanned+1), not how many were fetched (len(refs))
		// — an early break once Limit real matches are in hand can leave
		// fetched-but-never-looked-at candidates behind, and counting those
		// as "consumed" overstates the caller's own scan-budget spend
		// (cmd/goobers/backlogquery.go's listBacklogScanWindow decrements
		// its outer limit by CandidateCount), starving later pages of
		// budget for work this round never actually did.
		//
		// HasNext/NextCursor track the MATCH stream, not the candidate
		// stream (#2067's third acceptance criterion): stopping early
		// because Limit matches are already in hand leaves fetched-but-
		// unscanned candidates behind (scannedEverything=false), and even
		// having scanned every fetched candidate without reaching Limit
		// matches still leaves more to look at when the fetch itself hit
		// candidateLimit (fetchWasCapped) — ADO may hold further
		// candidates beyond what this round asked for.
		req.PageInfo.CandidateCount = lastScanned + 1
		scannedEverything := lastScanned == len(refs)-1
		fetchWasCapped := candidateLimit > 0 && len(refs) == candidateLimit
		req.PageInfo.HasNext = boundedScan && req.Limit > 0 && (!scannedEverything || fetchWasCapped)
		req.PageInfo.NextCursor = ""
		if req.PageInfo.HasNext && lastScanned >= 0 {
			req.PageInfo.NextCursor = strconv.Itoa(refs[lastScanned].ID)
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

// FindWorkItemsByMarker reads the project's authoritative work-item IDs and
// checks each live description for an exact single-line marker.
func (p *ADOProvider) FindWorkItemsByMarker(ctx context.Context, repo RepositoryRef, marker string) ([]WorkItem, error) {
	project := p.project(repo)
	if err := p.requireWorkItemScope(project); err != nil {
		return nil, err
	}
	if strings.TrimSpace(marker) == "" || strings.ContainsAny(marker, "\r\n") {
		return nil, fmt.Errorf("single-line work item marker is required")
	}
	return p.findWorkItemsByMarker(ctx, repo, marker, adoWIQLPageSize)
}

func (p *ADOProvider) findWorkItemsByMarker(ctx context.Context, repo RepositoryRef, marker string, pageSize int) ([]WorkItem, error) {
	if pageSize <= 0 {
		return nil, fmt.Errorf("ADO WIQL page size must be positive")
	}
	project := p.project(repo)
	endpoint, err := p.workURL(project, "wiql")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"$top": []string{strconv.Itoa(pageSize)}})
	if err != nil {
		return nil, err
	}
	var matches []WorkItem
	afterID := 0
	for {
		query := "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project"
		if afterID > 0 {
			query += fmt.Sprintf(" AND [System.Id] > %d", afterID)
		}
		query += " ORDER BY [System.Id] ASC"

		var result adoWIQLResponse
		if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{"query": query}, &result); err != nil {
			return nil, err
		}
		for _, ref := range result.WorkItems {
			item, err := p.GetWorkItem(ctx, repo, strconv.Itoa(ref.ID))
			if err != nil {
				return nil, err
			}
			if containsExactLine(item.Body, marker) {
				matches = append(matches, item)
			}
		}
		if len(result.WorkItems) < pageSize {
			return matches, nil
		}
		nextID := result.WorkItems[len(result.WorkItems)-1].ID
		if nextID <= afterID {
			return nil, fmt.Errorf("ADO WIQL marker scan did not advance beyond work item %d", afterID)
		}
		afterID = nextID
	}
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

// CreateWorkItemComment appends one work-item comment and returns its identity.
func (p *ADOProvider) CreateWorkItemComment(ctx context.Context, repo RepositoryRef, id, body string) (Comment, error) {
	project := p.project(repo)
	if err := p.requireWorkItemScope(project); err != nil {
		return Comment{}, err
	}
	if err := validateADOWorkItemID(id); err != nil {
		return Comment{}, err
	}
	endpoint, err := p.workURLVersion(project, "7.1-preview.4", "workItems", id, "comments")
	if err != nil {
		return Comment{}, err
	}
	var comment adoComment
	if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{"text": body}, &comment); err != nil {
		return Comment{}, err
	}
	return mapADOComment(comment), nil
}

// UpdateWorkItem edits Azure Boards fields, assignee, tags, state, and comments.
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
	if req.ExpectedRevision != "" {
		if err := checkWorkItemRevision(current, req.ExpectedRevision); err != nil {
			return WorkItem{}, err
		}
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
	if req.Assignee != nil {
		patch = append(patch, adoPatchOperation{Op: "add", Path: "/fields/System.AssignedTo", Value: *req.Assignee})
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

	// Fast path: an existing claim (breadcrumb, or a legacy owner tag) settles
	// this without writing anything. The winner may be us on a re-claim.
	winner, claimed, err := p.adoClaimWinner(ctx, req.Repository, req.ID)
	if err != nil {
		return ClaimResult{}, err
	}
	if claimed {
		item, getErr := p.GetWorkItem(ctx, req.Repository, req.ID)
		if getErr != nil {
			return ClaimResult{}, getErr
		}
		return ClaimResult{Claimed: winner == req.RunID, ClaimedBy: winner, Item: item}, nil
	}

	// Stake ours, then re-read to settle a race deterministically by comment
	// order — the same protocol the GitHub provider uses.
	if err := p.postWorkItemComment(ctx, req.Repository, req.ID, claimBreadcrumb(req.RunID)); err != nil {
		return ClaimResult{}, err
	}
	winner, claimed, err = p.adoClaimWinner(ctx, req.Repository, req.ID)
	if err != nil {
		return ClaimResult{}, err
	}
	if !claimed {
		return ClaimResult{}, fmt.Errorf("claim breadcrumb for run %q is not visible after write", req.RunID)
	}
	if winner != req.RunID {
		item, getErr := p.GetWorkItem(ctx, req.Repository, req.ID)
		if getErr != nil {
			return ClaimResult{}, getErr
		}
		return ClaimResult{Claimed: false, ClaimedBy: winner, Item: item}, nil
	}

	// Mirror the win as the fixed visible label. The rev test keeps the tag
	// write safe against a concurrent edit; it is not what decides the claim.
	item, err := p.setADOClaimLabel(ctx, req.Repository, req.ID, []string{label}, nil)
	if err != nil {
		return ClaimResult{}, err
	}
	if !item.HasLabel(label) {
		return ClaimResult{}, fmt.Errorf("claim label %q is not visible after write", label)
	}
	return ClaimResult{Claimed: true, ClaimedBy: req.RunID, Item: item}, nil
}

// setADOClaimLabel adds and removes work-item tags under the optimistic
// revision test, retrying a revision conflict the same way the claim path
// always has.
func (p *ADOProvider) setADOClaimLabel(ctx context.Context, repo RepositoryRef, id string, add, remove []string) (WorkItem, error) {
	var conflict error
	for range adoClaimRetries {
		current, getErr := p.GetWorkItem(ctx, repo, id)
		if getErr != nil {
			return WorkItem{}, getErr
		}
		raw, rawErr := rawADOWorkItem(current)
		if rawErr != nil {
			return WorkItem{}, rawErr
		}
		labels := applyLabelSet(adoRawTags(raw), add, remove)
		patch := []adoPatchOperation{
			{Op: "test", Path: "/rev", Value: raw.Rev},
			adoTagPatch(labels),
		}
		endpoint, endpointErr := p.workURL(p.project(repo), "workitems", id)
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
		return p.mapADOWorkItem(ctx, repo, out)
	}
	return WorkItem{}, fmt.Errorf("update claim label on work item %s after revision conflicts: %w", id, conflict)
}

// adoClaimWinner resolves the current claim owner from the work item's comment
// thread, falling back to a legacy owner tag.
//
// Ownership used to live in a tag encoding the run id, which minted a unique,
// never-reused entry in the project-global tag namespace on every claim — one
// per run, forever, with a 100% garbage rate (#1979). Comments carry the same
// information without a shared namespace, and match the GitHub provider's
// protocol exactly so the two backends do not drift.
//
// The legacy tag is still READ so items claimed before this change are not
// orphaned; it is never written again, and release clears it. That fallback is
// TEMPORARY — #1990 removes it (target 2026-08-14). A pre-1.0 product should
// not carry a permanent compat path for a format only Goobers ever wrote.
//
// KNOWN GAP vs the GitHub provider: GitHub filters breadcrumbs to the
// authenticated login, so a project member cannot spoof a claim by posting the
// marker themselves. The ADO provider has no authenticated-identity lookup
// wired, so it cannot apply the same filter and a member with comment access
// could forge one. Tracked separately rather than silently accepted.
func (p *ADOProvider) adoClaimWinner(ctx context.Context, repo RepositoryRef, id string) (string, bool, error) {
	comments, err := p.ListComments(ctx, repo, id)
	if err != nil {
		return "", false, err
	}
	sort.SliceStable(comments, func(i, j int) bool {
		left, leftErr := strconv.Atoi(comments[i].ID)
		right, rightErr := strconv.Atoi(comments[j].ID)
		if leftErr != nil || rightErr != nil {
			return comments[i].ID < comments[j].ID
		}
		return left < right
	})
	winner := ""
	for _, comment := range comments {
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
	if winner != "" {
		return winner, true, nil
	}

	current, err := p.GetWorkItem(ctx, repo, id)
	if err != nil {
		return "", false, err
	}
	raw, err := rawADOWorkItem(current)
	if err != nil {
		return "", false, err
	}
	return adoClaimOwner(adoRawTags(raw))
}

// ReleaseWorkItemClaim ends the current ADO claim epoch: it posts a release
// breadcrumb, drops the visible claim label, and clears any legacy owner tag
// left by a claim taken before ownership moved into the comment thread (#1979).
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

	winner, claimed, err := p.adoClaimWinner(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	if claimed && winner != req.RunID && !req.LedgerAuthorized {
		return WorkItem{}, fmt.Errorf("provider claim is held by run %q", winner)
	}

	if claimed {
		// The breadcrumb lands first so a successful release never leaves a later
		// claimer stuck behind the previous owner's durable marker.
		if err := p.postWorkItemComment(ctx, req.Repository, req.ID, claimReleaseBreadcrumb(winner)); err != nil {
			return WorkItem{}, err
		}
	}
	current, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	if !current.HasLabel(label) {
		return current, nil
	}
	remove := []string{label}
	// Legacy owner-tag cleanup; removed with the rest of the fallback in #1990.
	if legacy, tagErr := adoClaimTag(winner); tagErr == nil {
		remove = append(remove, legacy)
	}
	return p.setADOClaimLabel(ctx, req.Repository, req.ID, nil, remove)
}

// ListWorkItemLabelTransitionsForItem reaches parity in V1: ADO's work-item
// update history maps to label-transition events differently than GitHub's
// timeline. Only reached when a workflow gates claiming on a ready label's age
// (requireLabels contains a ready label), which the ADO backlog workload does
// not use.
func (p *ADOProvider) ListWorkItemLabelTransitionsForItem(context.Context, RepositoryRef, string, string) ([]WorkItemLabelTransition, error) {
	return nil, fmt.Errorf("ADO work-item label transitions reach parity in V1")
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
		Provider:       ProviderADO,
		ID:             strconv.Itoa(item.ID),
		ExternalID:     strconv.Itoa(item.Rev),
		Revision:       strconv.Itoa(item.Rev),
		Type:           stringField(item.Fields, "System.WorkItemType"),
		Title:          stringField(item.Fields, "System.Title"),
		Body:           stringField(item.Fields, "System.Description"),
		Labels:         labels,
		State:          state,
		Status:         statusFromLabels(labels, string(status)),
		Assignee:       stringField(item.Fields, "System.AssignedTo"),
		Links:          links,
		Parent:         parent,
		Hierarchy:      hierarchy,
		URL:            item.URL,
		CreatedAt:      timeField(item.Fields, "System.CreatedDate"),
		UpdatedAt:      updated,
		Fields:         adoWorkItemFields(item),
		BlockedByCount: adoBlockedByCount(item.Relations),
		Raw:            item,
		Integrity:      apiintegrity.Unapproved,
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
		Integrity:  apiintegrity.Unapproved,
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

func adoBlockedByCount(relations []adoRelation) int {
	count := 0
	for _, relation := range relations {
		if relation.Rel == "System.LinkTypes.Dependency-Reverse" {
			count++
		}
	}
	return count
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
	_, err := p.CreateWorkItemComment(ctx, repo, id, text)
	return err
}

// adoClaimTag renders the LEGACY owner tag. Retained only to recognize and
// clear claims taken before ownership moved into the comment thread (#1979) —
// never written by a new claim. Deleted by #1990 (target 2026-08-14).
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
