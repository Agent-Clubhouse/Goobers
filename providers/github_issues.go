package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// GitHub directs clients to wait at least one minute when a secondary-limit
// response carries neither Retry-After nor an exhausted primary quota.
const githubSecondaryFallbackMin = time.Minute

// ErrorCodeRateLimited is the stable error code a GitHub rate-limited failure
// surfaces under (#614) — carried by RateLimitError and by the stage
// result-file error convention (internal/executor's OutputErrorCode), so a
// quota-exhausted tick journals as itself rather than the generic
// missing_result_file it used to hide behind.
const ErrorCodeRateLimited = "github_rate_limited"

// ErrorCodeAuthFailed is the stable code for a credential that GitHub rejects
// with 401 or a permission-denied 403.
const ErrorCodeAuthFailed = "github_auth_failed"

// RateLimitError is the typed error send() returns when a rate-limited
// request cannot be absorbed by in-request backoff — the reset is further out
// than the wait budget, or the retry budget is exhausted (#614). Callers can
// errors.As it to learn when the quota recovers, instead of parsing the
// generic "status 403" string the non-2xx path used to fold this into.
type RateLimitError struct {
	// Provider names the forge that rate limited the request, so the
	// message reads as itself for every provider rather than always
	// claiming GitHub (#3647). Empty when the limit was recovered from an
	// untyped non-2xx response whose forge is not identifiable at that
	// boundary; Error() then says "provider" instead of naming one.
	Provider  ProviderKind
	Endpoint  string
	Status    int
	Remaining int
	// Reset is when GitHub says the quota window rolls over — zero when the
	// response carried no X-RateLimit-Reset header.
	Reset time.Time
	// Secondary marks a Retry-After-driven (abuse/secondary) limit rather
	// than an exhausted primary quota.
	Secondary bool
	// RetryAfterRaw/RemainingRaw/ResetRaw are the unparsed header string
	// values, carried through unchanged from RateLimitEvent — see its own
	// doc comment for why Error() needs these alongside the parsed fields
	// above.
	RetryAfterRaw string
	RemainingRaw  string
	ResetRaw      string
}

func (e *RateLimitError) Error() string {
	provider := string(e.Provider)
	if provider == "" {
		provider = "provider"
	}
	msg := fmt.Sprintf("%s rate limited (%s): %s: status %d, remaining %d", provider, ErrorCodeRateLimited, e.Endpoint, e.Status, e.Remaining)
	if !e.Reset.IsZero() {
		msg += ", resets at " + e.Reset.UTC().Format(time.RFC3339)
	}
	return msg + retryGuidanceSuffix(e.RetryAfterRaw, e.RemainingRaw, e.ResetRaw)
}

// rateLimitErrorFrom builds the typed give-up error from the same decision
// record rateLimitPlan produced for telemetry.
func rateLimitErrorFrom(ev RateLimitEvent) *RateLimitError {
	return &RateLimitError{
		Provider:      ev.Provider,
		Endpoint:      ev.Endpoint,
		Status:        ev.Status,
		Remaining:     ev.Remaining,
		Reset:         ev.Reset,
		Secondary:     ev.Secondary,
		RetryAfterRaw: ev.RetryAfterRaw,
		RemainingRaw:  ev.RemainingRaw,
		ResetRaw:      ev.ResetRaw,
	}
}

// isRateLimited reports whether resp is a GitHub rate-limit response we should
// back off and retry. Secondary-limit responses do not always include guidance
// headers, so 403 bodies are inspected and restored for the non-retry path.
func isRateLimited(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		if resp.Header.Get("Retry-After") != "" {
			return true
		}
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return true
		}
		if resp.Body == nil {
			return false
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		if err != nil {
			return false
		}
		message := strings.ToLower(string(body))
		return strings.Contains(message, "secondary rate limit") ||
			strings.Contains(message, "abuse detection") ||
			strings.Contains(message, "abuse rate limit")
	}
	return false
}

// rateLimitPlan computes how long to wait before retrying a rate-limited response
// and the event describing the decision.
func (p *GitHubProvider) rateLimitPlan(resp *http.Response, endpoint string, attempt int) (time.Duration, RateLimitEvent) {
	ev := RateLimitEvent{
		Provider: ProviderGitHub,
		Scope:    rateLimitScope(endpoint),
		Endpoint: endpoint,
		Status:   resp.StatusCode,
		Attempt:  attempt,
	}
	var wait time.Duration
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		ev.RetryAfterRaw = ra
		ev.Secondary = true
		if delay, ok := retryAfterDelay(ra, p.now()); ok {
			wait = delay
			ev.RetryAfter = wait
		}
	}
	if rem := strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")); rem != "" {
		ev.RemainingRaw = rem
		if n, err := strconv.Atoi(rem); err == nil {
			ev.Remaining = n
		}
	}
	if reset := strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")); reset != "" {
		ev.ResetRaw = reset
		if secs, err := strconv.ParseInt(reset, 10, 64); err == nil {
			ev.Reset = time.Unix(secs, 0)
			if wait == 0 {
				if d := ev.Reset.Sub(p.now()); d > 0 {
					wait = d
				}
			}
		}
	}
	if ev.RemainingRaw != "0" {
		ev.Secondary = true
	}
	if wait <= 0 {
		wait = fallbackBackoff(attempt, p.jitter)
		if ev.Secondary {
			wait += githubSecondaryFallbackMin
		}
	} else {
		// A server-directed wait (Retry-After or the reset clock) is honored
		// as-is plus slack — the old blanket rateLimitBackoffMax cap turned a
		// 21-minute reset into futile 60s sleeps that could never straddle
		// the window (#614). send() bounds the total via its wait budget
		// instead of capping each individual wait here.
		wait += rateLimitResetSlack
	}
	ev.Delay = wait
	return wait, ev
}

// ListComments returns the comments on a GitHub issue, oldest first.
func (p *GitHubProvider) ListComments(ctx context.Context, repo RepositoryRef, id string) ([]Comment, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errIssueIDRequired
	}
	raw, err := allIssueComments(ctx, p, p.BaseURL, repo, id)
	if err != nil {
		return nil, err
	}
	comments := make([]Comment, 0, len(raw))
	for _, c := range raw {
		comments = append(comments, mapGitHubComment(c))
	}
	return comments, nil
}

// AuthenticatedLogin returns the GitHub login represented by the provider's
// credential.
//
// Three arms, in this order: the login declared by configuration (#3343, the
// only arm an App installation token can satisfy), a REFUSAL when the caller
// declared identity resolution unavailable and the fallback unsafe (#3914),
// and GET /user — which is PAT-only and remains the correct answer for one.
func (p *GitHubProvider) AuthenticatedLogin(ctx context.Context) (string, error) {
	if p.configuredLogin != "" {
		return p.configuredLogin, nil
	}
	if p.loginSelfReportRefusal != "" {
		return "", &LoginSelfReportRefusedError{Reason: p.loginSelfReportRefusal}
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
		return "", fmt.Errorf("authenticated GitHub user has no login")
	}
	return login, nil
}

// LoginSelfReportRefusedError is returned by AuthenticatedLogin when the
// provider was constructed with WithLoginSelfReportRefused: the caller could
// not resolve the declared identity and refused the GET /user fallback rather
// than issue a request an App installation token cannot make (Goobers#3914).
//
// A distinct type so a caller can tell "this platform could not tell me who I
// am" from a forge-side permission failure, and so a test can prove the
// refusal happened WITHOUT a request rather than inferring it from a message.
type LoginSelfReportRefusedError struct {
	// Reason states what was missing and where, for the operator who has to
	// fix it.
	Reason string
}

func (e *LoginSelfReportRefusedError) Error() string {
	return "github: the authenticated login is unresolved and self-report (GET /user) is refused: " + e.Reason
}

// UpdateComment edits an existing issue/PR comment's body in place — the
// sticky-comment pattern (#716) a caller uses so a repeated event (e.g.
// pr-remediation's per-cycle checkpoint/escalation state) updates the SAME
// comment instead of growing a new one every run. GitHub scopes comment IDs
// repo-wide, not per-issue, so the edit endpoint takes no issue number.
func (p *GitHubProvider) UpdateComment(ctx context.Context, repo RepositoryRef, commentID, body string) error {
	if err := requireOwnerRepo(repo); err != nil {
		return err
	}
	if commentID == "" {
		return fmt.Errorf("comment id is required")
	}
	body, err := withAttribution(body, p.attribution, "comment-update")
	if err != nil {
		return err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", "comments", commentID)
	if err != nil {
		return err
	}
	return p.do(ctx, http.MethodPatch, endpoint, map[string]string{"body": body}, nil)
}

// DeleteComment removes an issue/PR comment. A missing comment is already in
// the desired state, so deletion is idempotent for concurrent reconcilers.
func (p *GitHubProvider) DeleteComment(ctx context.Context, repo RepositoryRef, commentID string) error {
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
	return doStatus(ctx, p, http.MethodDelete, endpoint, nil, nil, []int{http.StatusNotFound})
}

type githubIssueEvent struct {
	ID        int64        `json:"id"`
	Event     string       `json:"event"`
	CreatedAt time.Time    `json:"created_at"`
	Label     *githubLabel `json:"label"`
	Issue     githubIssue  `json:"issue"`
}

// LabelTransitionScanStop* name why a bounded label-transition walk stopped
// before it reached the end of the history (or its resume cursor).
const (
	LabelTransitionScanStopPageBudget = "page-budget"
	LabelTransitionScanStopQuotaFloor = "quota-floor"
)

// LabelTransitionScan bounds one repository label-transition walk (#3392).
// Before it, a periodic caller re-read the repo's ENTIRE issue-event history
// on every cycle — 200+ pages on an aged repo, growing monotonically with the
// repo's age, spent out of the same installation credential that claims, label
// writes, and PR creation share.
type LabelTransitionScan struct {
	// AfterEventID resumes from a persisted high-water mark: only events with a
	// strictly greater id are collected, and the walk stops as soon as it
	// reaches the cursor. Zero requests a full scan.
	AfterEventID int64
	// MaxPages bounds a full scan. Zero means unbounded.
	MaxPages int
	// MinQuotaFraction stops the walk once the response rate-limit window
	// reports less than this fraction of the window's limit remaining (0.1 =
	// 10%). Zero disables the floor. Deferring a periodic, self-healing check
	// to its next cycle is free; exhausting the shared credential is not.
	MinQuotaFraction float64
}

// LabelTransitionScanResult reports a bounded walk's transitions and how far it
// actually got. A Truncated result has a gap below its oldest transition, so a
// caller MUST NOT advance a persisted high-water mark from it.
type LabelTransitionScanResult struct {
	Transitions []WorkItemLabelTransition
	// Pages is how many event pages the walk actually read.
	Pages int
	// HighEventID is the greatest event id examined (matching the label or
	// not) — the cursor a caller persists to resume from next cycle. Zero when
	// the walk saw no events at all.
	HighEventID int64
	// ReachedCursor reports that the walk saw AfterEventID's generation, so
	// everything newer than the cursor is present.
	ReachedCursor bool
	// Truncated is set when MaxPages or MinQuotaFraction stopped the walk with
	// history still unread.
	Truncated bool
	// StopReason is one of the LabelTransitionScanStop* constants when
	// Truncated, else empty.
	StopReason string
	// QuotaLimit/QuotaRemaining/QuotaKnown carry the last observed rate-limit
	// window so a caller can log what it spent and why it stopped.
	QuotaLimit     int
	QuotaRemaining int
	QuotaKnown     bool
}

// ListWorkItemLabelTransitions returns the complete paginated repository event
// history for one issue label. Event IDs are stable provider cursors, so callers
// can persist and deduplicate overlapping snapshots without losing transitions.
func (p *GitHubProvider) ListWorkItemLabelTransitions(
	ctx context.Context,
	repo RepositoryRef,
	label string,
) ([]WorkItemLabelTransition, error) {
	result, err := p.ScanWorkItemLabelTransitions(ctx, repo, label, LabelTransitionScan{})
	if err != nil {
		return nil, err
	}
	return result.Transitions, nil
}

// ScanWorkItemLabelTransitions walks the repository event history for one issue
// label under the caller's bounds. With LabelTransitionScan.AfterEventID set,
// steady state costs the pages holding new events instead of the whole history.
func (p *GitHubProvider) ScanWorkItemLabelTransitions(
	ctx context.Context,
	repo RepositoryRef,
	label string,
	scan LabelTransitionScan,
) (LabelTransitionScanResult, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return LabelTransitionScanResult{}, err
	}
	if label == "" {
		return LabelTransitionScanResult{}, fmt.Errorf("label is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", "events")
	if err != nil {
		return LabelTransitionScanResult{}, err
	}
	return p.scanWorkItemLabelTransitions(ctx, endpoint, label, "", scan)
}

// ListWorkItemLabelTransitionsForItem returns one issue's complete paginated
// label history without scanning repository-wide events.
func (p *GitHubProvider) ListWorkItemLabelTransitionsForItem(
	ctx context.Context,
	repo RepositoryRef,
	id, label string,
) ([]WorkItemLabelTransition, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errIssueIDRequired
	}
	if label == "" {
		return nil, fmt.Errorf("label is required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "events")
	if err != nil {
		return nil, err
	}
	result, err := p.scanWorkItemLabelTransitions(ctx, endpoint, label, id, LabelTransitionScan{})
	if err != nil {
		return nil, err
	}
	return result.Transitions, nil
}

func (p *GitHubProvider) scanWorkItemLabelTransitions(
	ctx context.Context,
	endpoint, label, itemID string,
	scan LabelTransitionScan,
) (LabelTransitionScanResult, error) {
	endpoint, err := addQuery(endpoint, url.Values{"per_page": []string{"100"}})
	if err != nil {
		return LabelTransitionScanResult{}, err
	}
	var result LabelTransitionScanResult
	// GitHub serves repository issue events newest-first, so a cursor lets the
	// walk stop at the first already-seen event. That ordering is not a
	// documented contract, so it is *detected* per page rather than assumed:
	// against an oldest-first server the walk simply reads on to the end, which
	// is exactly today's cost, and never drops a transition.
	descending, orderKnown := false, false
	if err := p.getAllPagesWithContext(ctx, endpoint, func(page []byte, meta pageContext) error {
		result.Pages++
		if limit, remaining, ok := quotaFromHeaders(meta.Header); ok {
			result.QuotaLimit, result.QuotaRemaining, result.QuotaKnown = limit, remaining, true
		}
		var events []githubIssueEvent
		if err := json.Unmarshal(page, &events); err != nil {
			return fmt.Errorf("decode issue events page: %w", err)
		}
		if !orderKnown && len(events) > 1 {
			descending, orderKnown = events[0].ID > events[len(events)-1].ID, true
		}
		reachedCursor := false
		for _, event := range events {
			if event.ID > result.HighEventID {
				result.HighEventID = event.ID
			}
			if scan.AfterEventID > 0 && event.ID <= scan.AfterEventID {
				reachedCursor = true
				continue
			}
			if event.Label == nil || event.Label.Name != label ||
				(itemID == "" && event.Issue.PullRequest != nil) {
				continue
			}
			added := false
			switch event.Event {
			case "labeled":
				added = true
			case "unlabeled":
			default:
				continue
			}
			eventItemID := itemID
			if eventItemID == "" {
				eventItemID = strconv.Itoa(event.Issue.Number)
			}
			result.Transitions = append(result.Transitions, WorkItemLabelTransition{
				EventID:    event.ID,
				ItemID:     eventItemID,
				Label:      label,
				Added:      added,
				OccurredAt: event.CreatedAt,
			})
		}
		if reachedCursor {
			result.ReachedCursor = true
			if descending {
				return errStopPaging
			}
		}
		if !meta.HasNext {
			return nil
		}
		if scan.MaxPages > 0 && result.Pages >= scan.MaxPages {
			result.Truncated, result.StopReason = true, LabelTransitionScanStopPageBudget
			return errStopPaging
		}
		if scan.MinQuotaFraction > 0 && result.QuotaKnown &&
			float64(result.QuotaRemaining) < scan.MinQuotaFraction*float64(result.QuotaLimit) {
			result.Truncated, result.StopReason = true, LabelTransitionScanStopQuotaFloor
			return errStopPaging
		}
		return nil
	}); err != nil {
		return LabelTransitionScanResult{}, err
	}
	sort.Slice(result.Transitions, func(i, j int) bool {
		if result.Transitions[i].OccurredAt.Equal(result.Transitions[j].OccurredAt) {
			return result.Transitions[i].EventID < result.Transitions[j].EventID
		}
		return result.Transitions[i].OccurredAt.Before(result.Transitions[j].OccurredAt)
	})
	return result, nil
}

// UpdateWorkItem applies title/body edits, assignee changes, label add/remove,
// milestone assignment, open/close, and an optional comment to a GitHub issue.
// Only the fields the caller set are touched. Each applied change is recorded as
// an external-ref mutation with before/after field digests so the run journal can
// trace it.
func (p *GitHubProvider) UpdateWorkItem(ctx context.Context, req UpdateWorkItemRequest) (WorkItem, error) {
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
			Provider:  ProviderGitHub,
			Ref:       issueRef(req.Repository, req.ID),
			URL:       final.URL,
			Operation: updateOperation(req),
			Fields:    fields,
		})
	}
	return final, nil
}

// ClaimWorkItem writes a best-effort claiming marker (a label plus a run-id
// breadcrumb comment) so concurrent runs never double-process an item (WF-031). The
// winner is the run whose claim breadcrumb has the smallest server-assigned comment
// id in the current claim epoch; because those ids are monotonic and a racer only
// reads after its own comment persists, exactly one run is recognized as the winner.
// The runner's lease ledger remains the claim source of truth (BL-005); this marker
// only mirrors it.
func (p *GitHubProvider) ClaimWorkItem(ctx context.Context, req ClaimWorkItemRequest) (ClaimResult, error) {
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

	// Fast path: if a claim breadcrumb already exists, do not add another. Recognize
	// the existing winner (which may be us on an idempotent re-claim).
	if winner, ok, err := claimWinner(ctx, p, p.BaseURL, req.Repository, req.ID); err != nil {
		return ClaimResult{}, err
	} else if ok {
		return p.finishClaim(ctx, req.Repository, req.ID, req.RunID, winner, label)
	}

	// No existing claim: stake ours with a breadcrumb comment, then re-read to settle
	// the race deterministically by minimum comment id.
	if err := postAttributedComment(ctx, p, p.BaseURL, p.attribution, req.Repository, req.ID, claimBreadcrumb(req.RunID), "claim"); err != nil {
		return ClaimResult{}, err
	}
	winner, ok, err := claimWinner(ctx, p, p.BaseURL, req.Repository, req.ID)
	if err != nil {
		return ClaimResult{}, err
	}
	if !ok {
		return ClaimResult{}, fmt.Errorf("claim breadcrumb for run %q is not visible after write", req.RunID)
	}
	if winner == req.RunID {
		if err := p.applyLabelChanges(ctx, req.Repository, req.ID, []string{label}, nil); err != nil {
			return ClaimResult{}, err
		}
	}
	return p.finishClaim(ctx, req.Repository, req.ID, req.RunID, winner, label)
}

// ReleaseWorkItemClaim ends the current provider claim epoch and removes its label
// mirror. A ledger-authorized owner may also retire a historical provider winner.
// The release breadcrumb lands first so a successful release never leaves later
// claimers stuck behind the durable breadcrumb from the previous owner.
func (p *GitHubProvider) ReleaseWorkItemClaim(ctx context.Context, req ClaimWorkItemRequest) (WorkItem, error) {
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

	winner, claimed, err := claimWinner(ctx, p, p.BaseURL, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	if claimed && winner != req.RunID {
		if !req.LedgerAuthorized {
			return WorkItem{}, fmt.Errorf("provider claim is held by run %q", winner)
		}
	}
	before, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	releasedRunID := req.RunID
	if claimed {
		releasedRunID = winner
		if err := postAttributedComment(ctx, p, p.BaseURL, p.attribution, req.Repository, req.ID, claimReleaseBreadcrumb(winner), "claim-release"); err != nil {
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
		Provider:  ProviderGitHub,
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

// ReconcileOrphanedWorkItemClaim closes any historical provider claim epoch,
// removes the requested drifted labels, and records why the ledger-authoritative
// reconciliation changed the issue.
func (p *GitHubProvider) ReconcileOrphanedWorkItemClaim(
	ctx context.Context,
	repo RepositoryRef,
	id string,
	removeLabels []string,
	comment string,
) (WorkItem, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return WorkItem{}, err
	}
	if id == "" {
		return WorkItem{}, errIssueIDRequired
	}
	if strings.TrimSpace(comment) == "" {
		return WorkItem{}, fmt.Errorf("reconciliation comment is required")
	}
	removesClaimLabel := false
	for _, label := range removeLabels {
		if label == LabelClaimed {
			removesClaimLabel = true
			break
		}
	}
	if !removesClaimLabel {
		removeLabels = append(removeLabels, LabelClaimed)
	}

	winner, claimed, err := claimWinner(ctx, p, p.BaseURL, repo, id)
	if err != nil {
		return WorkItem{}, err
	}
	before, err := p.GetWorkItem(ctx, repo, id)
	if err != nil {
		return WorkItem{}, err
	}
	body := comment
	if claimed {
		body = claimReleaseBreadcrumb(winner) + "\n\n" + comment
	}
	if err := p.postComment(ctx, repo, id, body); err != nil {
		return WorkItem{}, err
	}
	if err := p.applyLabelChanges(ctx, repo, id, nil, removeLabels); err != nil {
		return WorkItem{}, err
	}
	final, err := p.GetWorkItem(ctx, repo, id)
	if err != nil {
		return WorkItem{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       issueRef(repo, id),
		URL:       final.URL,
		Operation: "claim-reconcile",
		Fields: map[string]FieldDigest{
			"claim":  {Before: digestString("orphaned"), After: digestString("released")},
			"labels": {Before: digestLabels(before.Labels), After: digestLabels(final.Labels)},
			"comment": {
				After: digestString(body),
			},
		},
	})
	return final, nil
}

// finishClaim loads the final item, records the claim mutation, and reports whether
// runID is the recognized winner.
func (p *GitHubProvider) finishClaim(ctx context.Context, repo RepositoryRef, id, runID, winner, label string) (ClaimResult, error) {
	item, err := p.GetWorkItem(ctx, repo, id)
	if err != nil {
		return ClaimResult{}, err
	}
	claimed := winner == runID
	if claimed && !item.HasLabel(label) {
		return ClaimResult{}, fmt.Errorf("claim label %q is not visible after write", label)
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
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

// applyLabelChanges adds labels (additive; GitHub ignores duplicates) and removes
// labels, tolerating a 404 when a removed label is not present.
func (p *GitHubProvider) applyLabelChanges(ctx context.Context, repo RepositoryRef, id string, add, remove []string) error {
	if add = uniqueStrings(add); len(add) > 0 {
		endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "labels")
		if err != nil {
			return err
		}
		if err := p.do(ctx, http.MethodPost, endpoint, map[string][]string{"labels": add}, nil); err != nil {
			return err
		}
	}
	for _, label := range uniqueStrings(remove) {
		endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "labels", label)
		if err != nil {
			return err
		}
		if err := doStatus(ctx, p, http.MethodDelete, endpoint, nil, nil, []int{http.StatusNotFound}); err != nil {
			return err
		}
	}
	return nil
}

func (p *GitHubProvider) postComment(ctx context.Context, repo RepositoryRef, id, body string) error {
	return postAttributedComment(ctx, p, p.BaseURL, p.attribution, repo, id, body, "comment")
}

// CreateWorkItemComment appends one issue comment and returns its provider
// identity. Retry-safe callers perform exact-marker adoption around this raw
// non-idempotent POST.
func (p *GitHubProvider) CreateWorkItemComment(ctx context.Context, repo RepositoryRef, id, body string) (Comment, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return Comment{}, err
	}
	if id == "" {
		return Comment{}, errIssueIDRequired
	}
	body, err := withAttribution(body, p.attribution, "comment")
	if err != nil {
		return Comment{}, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return Comment{}, err
	}
	var comment restComment
	if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{"body": body}, &comment); err != nil {
		return Comment{}, err
	}
	return mapGitHubComment(comment), nil
}

func mapGitHubComment(c restComment) Comment {
	return Comment{
		ID:         strconv.FormatInt(c.ID, 10),
		Author:     c.User.Login,
		AuthorType: c.User.Type,
		Body:       c.Body,
		CreatedAt:  c.CreatedAt,
		URL:        c.HTMLURL,
		Integrity:  apiintegrity.Unapproved,
	}
}

var (
	// claimBreadcrumbPattern matches the machine-parseable line in a claim comment.
	claimBreadcrumbPattern = regexp.MustCompile(`(?m)^goobers-claim:\s*run=(\S+)\s*$`)
	// claimReleaseBreadcrumbPattern matches the boundary ending one claim epoch.
	claimReleaseBreadcrumbPattern = regexp.MustCompile(`(?m)^goobers-claim-release:\s*run=(\S+)\s*$`)
)

// claimBreadcrumb renders a claim comment body: a machine-parseable marker line
// plus a human-readable note.
func claimBreadcrumb(runID string) string {
	return fmt.Sprintf("goobers-claim: run=%s\n\nClaimed by Goobers run `%s` for exactly-once processing.", runID, runID)
}

func claimReleaseBreadcrumb(runID string) string {
	return fmt.Sprintf("goobers-claim-release: run=%s\n\nReleased by Goobers run `%s`; a later run may claim this item.", runID, runID)
}

// claimRunID extracts the run id from a claim breadcrumb body, or "" if the body is
// not a claim breadcrumb.
func claimRunID(body string) string {
	m := claimBreadcrumbPattern.FindStringSubmatch(body)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

func claimReleaseRunID(body string) string {
	m := claimReleaseBreadcrumbPattern.FindStringSubmatch(body)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

func labelsChanged(req UpdateWorkItemRequest) bool {
	return len(uniqueStrings(req.AddLabels)) > 0 || len(uniqueStrings(req.RemoveLabels)) > 0
}

// applyLabelSet computes the resulting label set after add/remove, for digesting.
func applyLabelSet(current, add, remove []string) []string {
	removeSet := make(map[string]struct{}, len(remove))
	for _, r := range remove {
		removeSet[r] = struct{}{}
	}
	next := make([]string, 0, len(current)+len(add))
	for _, l := range current {
		if _, drop := removeSet[l]; drop {
			continue
		}
		next = append(next, l)
	}
	next = append(next, add...)
	return uniqueStrings(next)
}

// digestLabels digests a label set independent of order.
func digestLabels(labels []string) string {
	sorted := append([]string(nil), uniqueStrings(labels)...)
	sort.Strings(sorted)
	return digestString(strings.Join(sorted, ","))
}

func issueRef(repo RepositoryRef, id string) string {
	return fmt.Sprintf("%s/%s#%s", repo.Owner, repo.Name, id)
}

// updateOperation names the mutation for the journal by its dominant change.
func updateOperation(req UpdateWorkItemRequest) string {
	if strings.EqualFold(req.State, "closed") {
		return "close"
	}
	if req.Milestone != nil && req.Title == nil && req.Body == nil && req.Assignee == nil && req.State == "" && req.Comment == "" && !labelsChanged(req) {
		return "milestone"
	}
	if labelsChanged(req) && req.Title == nil && req.Body == nil && req.Assignee == nil && req.Milestone == nil && req.State == "" && req.Comment == "" {
		return "label"
	}
	if req.Comment != "" && req.Title == nil && req.Body == nil && req.Assignee == nil && req.Milestone == nil && req.State == "" && !labelsChanged(req) {
		return "comment"
	}
	return "update"
}
