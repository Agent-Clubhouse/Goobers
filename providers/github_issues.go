package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Rate-limit retry tuning. GitHub signals primary limits via X-RateLimit-Remaining
// / X-RateLimit-Reset and secondary (abuse) limits via Retry-After; we honor those
// and otherwise fall back to capped exponential backoff.
const (
	defaultRateLimitRetries = 4
	rateLimitBackoffBase    = time.Second
	rateLimitBackoffMax     = 60 * time.Second
	// rateLimitResetSlack pads a server-directed wait (Retry-After or the
	// reset clock) so the retry fires just after the window actually rolls
	// over, not a clock-jitter hair before it — a too-early retry burns an
	// attempt on a guaranteed second 403.
	rateLimitResetSlack = 2 * time.Second
	// defaultRateLimitMaxWait bounds the TOTAL time send() spends sleeping
	// on rate-limit backoff within one request (#614). Honoring
	// X-RateLimit-Reset means a single wait can be minutes, not the old
	// sub-minute exponential — but a shell stage subprocess is killed at
	// internal/executor's 10-minute DefaultTimeout, and that kill surfaces
	// as "timeout", masking the rate-limit cause this machinery exists to
	// make visible. 5 minutes absorbs a near reset boundary transparently
	// while leaving the stage's own work room under that ceiling; a reset
	// further out fails fast with a typed RateLimitError instead.
	// WithRateLimitMaxWait overrides.
	defaultRateLimitMaxWait = 5 * time.Minute
)

// ErrorCodeRateLimited is the stable error code a GitHub rate-limited failure
// surfaces under (#614) — carried by RateLimitError and by the stage
// result-file error convention (internal/executor's OutputErrorCode), so a
// quota-exhausted tick journals as itself rather than the generic
// missing_result_file it used to hide behind.
const ErrorCodeRateLimited = "github_rate_limited"

// RateLimitError is the typed error send() returns when a rate-limited
// request cannot be absorbed by in-request backoff — the reset is further out
// than the wait budget, or the retry budget is exhausted (#614). Callers can
// errors.As it to learn when the quota recovers, instead of parsing the
// generic "status 403" string the non-2xx path used to fold this into.
type RateLimitError struct {
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
	msg := fmt.Sprintf("github rate limited (%s): %s: status %d, remaining %d", ErrorCodeRateLimited, e.Endpoint, e.Status, e.Remaining)
	if !e.Reset.IsZero() {
		msg += ", resets at " + e.Reset.UTC().Format(time.RFC3339)
	}
	return msg + retryGuidanceSuffix(e.RetryAfterRaw, e.RemainingRaw, e.ResetRaw)
}

// rateLimitErrorFrom builds the typed give-up error from the same decision
// record rateLimitPlan produced for telemetry.
func rateLimitErrorFrom(ev RateLimitEvent) *RateLimitError {
	return &RateLimitError{
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

// isRateLimited reports whether resp is a GitHub rate-limit response we should back
// off and retry. It inspects headers only, so the body stays available for the
// non-retry path.
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
	}
	return false
}

// rateLimitPlan computes how long to wait before retrying a rate-limited response
// and the event describing the decision.
func (p *GitHubProvider) rateLimitPlan(resp *http.Response, endpoint string, attempt int) (time.Duration, RateLimitEvent) {
	ev := RateLimitEvent{
		Provider: ProviderGitHub,
		Endpoint: endpoint,
		Status:   resp.StatusCode,
		Attempt:  attempt,
	}
	var wait time.Duration
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		ev.RetryAfterRaw = ra
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			wait = time.Duration(secs) * time.Second
			ev.RetryAfter = wait
			ev.Secondary = true
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
	if wait <= 0 {
		// No server-directed wait: capped exponential fallback.
		wait = backoffDuration(attempt)
	} else {
		// A server-directed wait (Retry-After or the reset clock) is honored
		// as-is plus slack — the old blanket rateLimitBackoffMax cap turned a
		// 21-minute reset into futile 60s sleeps that could never straddle
		// the window (#614). send() bounds the total via its wait budget
		// instead of capping each individual wait here.
		wait += rateLimitResetSlack
	}
	ev.Wait = wait
	return wait, ev
}

// backoffDuration returns capped exponential backoff for the given attempt.
func backoffDuration(attempt int) time.Duration {
	d := rateLimitBackoffBase << attempt
	if d <= 0 || d > rateLimitBackoffMax {
		return rateLimitBackoffMax
	}
	return d
}

// ListComments returns the comments on a GitHub issue, oldest first.
func (p *GitHubProvider) ListComments(ctx context.Context, repo RepositoryRef, id string) ([]Comment, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("issue id is required")
	}
	raw, err := p.allIssueComments(ctx, repo, id)
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
func (p *GitHubProvider) AuthenticatedLogin(ctx context.Context) (string, error) {
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
	return p.doStatus(ctx, http.MethodDelete, endpoint, nil, nil, []int{http.StatusNotFound})
}

// allIssueComments fetches every comment on an issue, following pagination
// (#139). Both ListComments and the claim protocol's claimWinner read the full
// comment set through here: a claim breadcrumb landing on page 2+ used to be
// invisible, so two racers each read "no claim" and both took the empty-read
// "we win" branch — a double claim on any issue with >30 comments.
func (p *GitHubProvider) allIssueComments(ctx context.Context, repo RepositoryRef, id string) ([]githubComment, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return nil, err
	}
	var all []githubComment
	err = p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageItems []githubComment
		if err := json.Unmarshal(page, &pageItems); err != nil {
			return fmt.Errorf("decode comments page: %w", err)
		}
		all = append(all, pageItems...)
		return nil
	})
	return all, err
}

// UpdateWorkItem applies title/body edits, label add/remove, open/close, and an
// optional comment to a GitHub issue. Only the fields the caller set are touched.
// Each applied change is recorded as an external-ref mutation with before/after
// field digests so the run journal can trace it.
func (p *GitHubProvider) UpdateWorkItem(ctx context.Context, req UpdateWorkItemRequest) (WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	if req.ID == "" {
		return WorkItem{}, fmt.Errorf("issue id is required")
	}
	before, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
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

	if labelsChanged(req) {
		if err := p.applyLabelChanges(ctx, req.Repository, req.ID, req.AddLabels, req.RemoveLabels); err != nil {
			return WorkItem{}, err
		}
		after := applyLabelSet(before.Labels, req.AddLabels, req.RemoveLabels)
		fields["labels"] = FieldDigest{Before: digestLabels(before.Labels), After: digestLabels(after)}
	}

	if req.Comment != "" {
		if err := p.postComment(ctx, req.Repository, req.ID, req.Comment); err != nil {
			return WorkItem{}, err
		}
		fields["comment"] = FieldDigest{After: digestString(req.Comment)}
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
// id; because those ids are monotonic and a racer only reads after its own comment
// persists, exactly one run is recognized as the winner. The runner's lease ledger
// remains the claim source of truth (BL-005); this marker only mirrors it.
func (p *GitHubProvider) ClaimWorkItem(ctx context.Context, req ClaimWorkItemRequest) (ClaimResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return ClaimResult{}, err
	}
	if req.ID == "" {
		return ClaimResult{}, fmt.Errorf("issue id is required")
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
	if winner, ok, err := p.claimWinner(ctx, req.Repository, req.ID); err != nil {
		return ClaimResult{}, err
	} else if ok {
		return p.finishClaim(ctx, req.Repository, req.ID, req.RunID, winner)
	}

	// No existing claim: stake ours with a breadcrumb comment, then re-read to settle
	// the race deterministically by minimum comment id.
	if err := p.postComment(ctx, req.Repository, req.ID, claimBreadcrumb(req.RunID)); err != nil {
		return ClaimResult{}, err
	}
	winner, ok, err := p.claimWinner(ctx, req.Repository, req.ID)
	if err != nil {
		return ClaimResult{}, err
	}
	if !ok {
		// Our own breadcrumb must be visible; treat an empty read as us winning.
		winner = req.RunID
	}
	if winner == req.RunID {
		if err := p.applyLabelChanges(ctx, req.Repository, req.ID, []string{label}, nil); err != nil {
			return ClaimResult{}, err
		}
	}
	return p.finishClaim(ctx, req.Repository, req.ID, req.RunID, winner)
}

// finishClaim loads the final item, records the claim mutation, and reports whether
// runID is the recognized winner.
func (p *GitHubProvider) finishClaim(ctx context.Context, repo RepositoryRef, id, runID, winner string) (ClaimResult, error) {
	item, err := p.GetWorkItem(ctx, repo, id)
	if err != nil {
		return ClaimResult{}, err
	}
	claimed := winner == runID
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

// claimWinner reads the issue comments and returns the run id of the recognized
// claimer: the claim breadcrumb with the smallest comment id. ok is false when no
// claim breadcrumb exists yet.
func (p *GitHubProvider) claimWinner(ctx context.Context, repo RepositoryRef, id string) (string, bool, error) {
	raw, err := p.allIssueComments(ctx, repo, id)
	if err != nil {
		return "", false, err
	}
	claims := make([]githubComment, 0, len(raw))
	for _, c := range raw {
		if claimRunID(c.Body) != "" {
			claims = append(claims, c)
		}
	}
	if len(claims) == 0 {
		return "", false, nil
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].ID < claims[j].ID })
	return claimRunID(claims[0].Body), true, nil
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
		if err := p.doStatus(ctx, http.MethodDelete, endpoint, nil, nil, []int{http.StatusNotFound}); err != nil {
			return err
		}
	}
	return nil
}

func (p *GitHubProvider) postComment(ctx context.Context, repo RepositoryRef, id, body string) error {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return err
	}
	return p.do(ctx, http.MethodPost, endpoint, map[string]string{"body": body}, nil)
}

type githubComment struct {
	ID        int64      `json:"id"`
	Body      string     `json:"body"`
	User      githubUser `json:"user"`
	HTMLURL   string     `json:"html_url"`
	CreatedAt *time.Time `json:"created_at"`
}

func mapGitHubComment(c githubComment) Comment {
	return Comment{
		ID:        strconv.FormatInt(c.ID, 10),
		Author:    c.User.Login,
		Body:      c.Body,
		CreatedAt: c.CreatedAt,
		URL:       c.HTMLURL,
	}
}

// claimBreadcrumbPattern matches the machine-parseable line in a claim comment.
var claimBreadcrumbPattern = regexp.MustCompile(`(?m)^goobers-claim:\s*run=(\S+)\s*$`)

// claimBreadcrumb renders a claim comment body: a machine-parseable marker line
// plus a human-readable note.
func claimBreadcrumb(runID string) string {
	return fmt.Sprintf("goobers-claim: run=%s\n\nClaimed by Goobers run `%s` for exactly-once processing.", runID, runID)
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
	if labelsChanged(req) && req.Title == nil && req.Body == nil && req.State == "" && req.Comment == "" {
		return "label"
	}
	if req.Comment != "" && req.Title == nil && req.Body == nil && req.State == "" && !labelsChanged(req) {
		return "comment"
	}
	return "update"
}
