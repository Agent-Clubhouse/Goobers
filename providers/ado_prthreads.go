package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// This file houses the foundational Azure DevOps provider primitives the
// pr-remediation loop depends on (remediation-wiring-plan.md Part 2). Azure
// DevOps has no PR-comment transport equivalent to GitHub's issue-comment API,
// so the whole verdict/finding/head-SHA handoff must ride on PR *threads*. It
// also lacks a PR-label writer, an authenticated-identity read, and a
// single-PR summary read. Each primitive is a plain *ADOProvider method the
// per-stage ADO branches call directly (not through the Dispatcher), mirroring
// how the merge-review ADO stages call PollPullRequest directly.

// adoPRThreadCommentType is the ADO comment type for author-written text (as
// opposed to "system" threads ADO emits for vote/status/ref events, which the
// list reader skips).
const adoPRThreadCommentType = "text"

// PostPullRequestThreadComment opens a new Azure DevOps pull-request thread with
// a single top-level comment and returns it mapped to the provider-neutral
// Comment type. This is the ADO analog of a GitHub issue/PR comment — the
// carrier for the merge-review verdict-json, the finding-set history, and the
// sticky remediation-state comment (with the pre-remediation head SHA). ADO has
// no PR-comment transport otherwise, so this is the keystone of the ADO
// remediation handoff.
//
// The returned Comment.ID is the composite "<pullID>/<threadId>/<commentId>" so
// UpdatePullRequestThreadComment can address the exact comment later with no
// extra state — the ADO update endpoint needs all three, unlike GitHub's
// repo-wide comment ids. The author is mapped from the thread comment's
// displayName (ADO renders thread authors as displayName), matching what
// AuthenticatedLogin returns so a trusted-author filter recognizes a thread we
// posted.
func (p *ADOProvider) PostPullRequestThreadComment(ctx context.Context, repo RepositoryRef, pullID, body string) (Comment, error) {
	return p.postAttributedPullRequestThreadComment(ctx, repo, pullID, body, "pull-request-comment")
}

func (p *ADOProvider) postAttributedPullRequestThreadComment(ctx context.Context, repo RepositoryRef, pullID, body, action string) (Comment, error) {
	if err := requireRepo(repo); err != nil {
		return Comment{}, err
	}
	if pullID == "" {
		return Comment{}, errPullIDRequired
	}
	body, err := withAttribution(body, p.attribution, action)
	if err != nil {
		return Comment{}, err
	}
	endpoint, err := p.repoURL(repo, "pullrequests", pullID, "threads")
	if err != nil {
		return Comment{}, err
	}
	payload := map[string]interface{}{
		"comments": []map[string]interface{}{{
			"parentCommentId": 0,
			"content":         body,
			"commentType":     adoPRThreadCommentType,
		}},
		"status": "active",
	}
	var thread adoPullRequestThread
	if err := p.do(ctx, http.MethodPost, endpoint, payload, &thread); err != nil {
		return Comment{}, err
	}
	if len(thread.Comments) == 0 {
		return Comment{}, fmt.Errorf("ado pull request %s thread create returned no comments", pullID)
	}
	return mapADOPullRequestThreadComment(pullID, thread.ID, thread.Comments[0]), nil
}

// ListPullRequestThreadComments returns every author-written comment across a
// pull request's threads, oldest first, mapped to the provider-neutral Comment
// type. It is the ADO analog of GitHub's PR-comment list: gather-pr-context's
// verdict recovery, push-remediated's head-SHA read, and remediation-checkpoint's
// sticky-state read all consume it. System threads (vote/status/ref events ADO
// synthesizes, commentType "system") are skipped so only real comments reach the
// stage layer.
func (p *ADOProvider) ListPullRequestThreadComments(ctx context.Context, repo RepositoryRef, pullID string) ([]Comment, error) {
	if err := requireRepo(repo); err != nil {
		return nil, err
	}
	if pullID == "" {
		return nil, errPullIDRequired
	}
	endpoint, err := p.repoURL(repo, "pullrequests", pullID, "threads")
	if err != nil {
		return nil, err
	}
	var resp adoPullRequestThreadsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, err
	}
	comments := make([]Comment, 0)
	for _, thread := range resp.Value {
		for _, comment := range thread.Comments {
			if strings.EqualFold(comment.CommentType, "system") {
				continue
			}
			comments = append(comments, mapADOPullRequestThreadComment(pullID, thread.ID, comment))
		}
	}
	return comments, nil
}

// UpdatePullRequestThreadComment edits an existing pull-request thread comment's
// content in place — the ADO analog of GitHub's UpdateComment and the sticky-
// comment update remediation-checkpoint relies on. commentID is the composite
// "<pullID>/<threadId>/<commentId>" a prior Post/List returned; it is parsed
// back into the three path segments ADO's PATCH endpoint requires.
func (p *ADOProvider) UpdatePullRequestThreadComment(ctx context.Context, repo RepositoryRef, commentID, body string) error {
	if err := requireRepo(repo); err != nil {
		return err
	}
	pullID, threadID, cID, err := parseADOThreadCommentID(commentID)
	if err != nil {
		return err
	}
	body, err = withAttribution(body, p.attribution, "pull-request-comment-update")
	if err != nil {
		return err
	}
	endpoint, err := p.repoURL(repo, "pullrequests", pullID, "threads", threadID, "comments", cID)
	if err != nil {
		return err
	}
	return p.do(ctx, http.MethodPatch, endpoint, map[string]interface{}{"content": body}, nil)
}

// AddPullRequestLabels applies one or more native Azure DevOps PR labels — the
// hazard-free carrier for the goobers:needs-remediation selector signal.
// ListPullRequests already reads PR labels, so writing them here drives the
// existing selection tiers unmodified, without any wit/workitems write (the
// PR-as-work-item wrong-object hazard). ADO's label endpoint takes one label
// per POST, so this issues one call per name.
func (p *ADOProvider) AddPullRequestLabels(ctx context.Context, repo RepositoryRef, pullID string, names []string) error {
	if err := requireRepo(repo); err != nil {
		return err
	}
	if pullID == "" {
		return errPullIDRequired
	}
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		// The PR-labels endpoint is published only under the -preview version;
		// a plain "7.1" is rejected (VssInvalidPreviewVersionException).
		endpoint, err := p.repoURLVersion(repo, "7.1-preview.1", "pullrequests", pullID, "labels")
		if err != nil {
			return err
		}
		if err := p.do(ctx, http.MethodPost, endpoint, map[string]interface{}{"name": name}, nil); err != nil {
			return err
		}
	}
	return nil
}

// pullRequestLabelsWithIDs fetches a PR's labels as a lowercased-name -> id
// map. ADO returns labels only from this dedicated sub-endpoint for a single
// PR (the PR object and $expand=labels both omit them — verified live); the
// id is needed to delete a label whose name contains a colon.
func (p *ADOProvider) pullRequestLabelsWithIDs(ctx context.Context, repo RepositoryRef, pullID string) (map[string]string, error) {
	endpoint, err := p.repoURLVersion(repo, "7.1-preview.1", "pullrequests", pullID, "labels")
	if err != nil {
		return nil, err
	}
	var out struct {
		Value []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"value"`
	}
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	m := make(map[string]string, len(out.Value))
	for _, l := range out.Value {
		m[strings.ToLower(l.Name)] = l.ID
	}
	return m, nil
}

// PullRequestLabelNames returns a PR's active label names via the dedicated
// labels sub-endpoint (the single-PR GET omits them — verified live).
func (p *ADOProvider) PullRequestLabelNames(ctx context.Context, repo RepositoryRef, pullID string) ([]string, error) {
	labels, err := p.pullRequestLabelsWithIDs(ctx, repo, pullID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	return names, nil
}

// RemovePullRequestLabel deletes one native Azure DevOps PR label. It is the
// re-entry trigger clearing the goobers:needs-remediation marker so merge-review
// re-selects a reworked PR (push-remediated), and the clean-rebase clear
// (rebase-pr). Absence is treated as benign: a label already gone is not an
// error. ADO's delete-by-name endpoint 400s on a colon-containing name, so this
// resolves the label id first and deletes by id.
func (p *ADOProvider) RemovePullRequestLabel(ctx context.Context, repo RepositoryRef, pullID, name string) error {
	if err := requireRepo(repo); err != nil {
		return err
	}
	if pullID == "" {
		return errPullIDRequired
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("label name is required")
	}
	// ADO's delete-by-name endpoint 400s on a label whose name contains a
	// colon (e.g. goobers:needs-remediation) — verified live. Resolve the
	// label's id and delete by id, which ADO accepts.
	labels, err := p.pullRequestLabelsWithIDs(ctx, repo, pullID)
	if err != nil {
		return err
	}
	id, present := labels[strings.ToLower(name)]
	if !present {
		// Already absent — benign, mirror GitHub's 404-is-not-an-error removal.
		return nil
	}
	endpoint, err := p.repoURLVersion(repo, "7.1-preview.1", "pullrequests", pullID, "labels", id)
	if err != nil {
		return err
	}
	resp, err := p.send(ctx, http.MethodDelete, endpoint, nil, "")
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil
	}
	return readJSONResponse(resp, http.MethodDelete, endpoint, nil)
}

// AuthenticatedLogin returns the display name of the identity the provider's
// credential authenticates as, via the Azure DevOps connectionData endpoint. It
// matches the GitHub AuthenticatedLogin signature so stage code can call it
// uniformly. The display name (not the UPN) is returned deliberately: ADO PR
// *thread* comment authors render as displayName, so this is what a trusted-
// author filter must compare a posted thread against. (PR reviewers/authors,
// by contrast, are keyed on UPN — a known ADO identity inconsistency.)
func (p *ADOProvider) AuthenticatedLogin(ctx context.Context) (string, error) {
	endpoint, err := joinURL(p.BaseURL, p.Organization, "_apis", "connectionData")
	if err != nil {
		return "", err
	}
	endpoint, err = addQuery(endpoint, url.Values{"api-version": []string{"7.1-preview"}})
	if err != nil {
		return "", err
	}
	var data adoConnectionData
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &data); err != nil {
		return "", err
	}
	login := strings.TrimSpace(data.AuthenticatedUser.ProviderDisplayName)
	if login == "" {
		login = strings.TrimSpace(data.AuthenticatedUser.CustomDisplayName)
	}
	if login == "" {
		return "", fmt.Errorf("authenticated ADO identity has no display name")
	}
	return login, nil
}

// GetPullRequest returns a single Azure DevOps pull request as a
// PullRequestSummary, matching the GitHub provider's GetPullRequest signature so
// the pr-remediation stage ADO branches (push-remediated, remediation-checkpoint,
// pr-claim guards) can read a PR's live head SHA, state, and labels uniformly.
// It is a thin adapter over PollPullRequest — ADO's single-PR read — mirroring
// apply-verdict's currentPullRequest helper.
func (p *ADOProvider) GetPullRequest(ctx context.Context, repo RepositoryRef, pullID string) (PullRequestSummary, error) {
	if err := requireRepo(repo); err != nil {
		return PullRequestSummary{}, err
	}
	if pullID == "" {
		return PullRequestSummary{}, errPullIDRequired
	}
	poll, err := p.PollPullRequest(ctx, PullRequestPollRequest{Repository: repo, PullID: pullID})
	if err != nil {
		return PullRequestSummary{}, err
	}
	// GetPullRequest deliberately does NOT fetch labels: no ADO caller reads them
	// off this path (the checkpoint re-fetches via ListPullRequests, apply-verdict
	// reads them via PullRequestLabelNames, and the rest use only head/base/state/
	// body), so an extra /labels round-trip on every call would be pure overhead.
	// A caller that needs a PR's labels calls PullRequestLabelNames directly.
	return PullRequestSummary{
		ID:                 pullID,
		Number:             poll.Number,
		URL:                poll.URL,
		Author:             poll.Author,
		RequestedReviewers: poll.RequestedReviewers,
		State:              poll.State,
		Merged:             poll.Merged,
		Head:               poll.HeadBranch,
		Base:               poll.BaseBranch,
		HeadSHA:            poll.HeadSHA,
		BaseSHA:            poll.BaseSHA,
		Draft:              poll.Draft,
		Labels:             poll.Labels,
		CheckState:         poll.CheckState,
		Body:               poll.Body,
		Integrity:          poll.Integrity,
	}, nil
}

// mapADOPullRequestThreadComment flattens one ADO PR-thread comment into the
// provider-neutral Comment shape. The ID is the composite
// "<pullID>/<threadId>/<commentId>" (see PostPullRequestThreadComment).
func mapADOPullRequestThreadComment(pullID string, threadID int, comment adoPullRequestThreadComment) Comment {
	author := comment.Author.DisplayName
	if author == "" {
		author = comment.Author.UniqueName
	}
	var createdAt *time.Time
	if parsed, err := time.Parse(time.RFC3339Nano, comment.PublishedDate); err == nil {
		utc := parsed.UTC()
		createdAt = &utc
	}
	return Comment{
		ID:         formatADOThreadCommentID(pullID, threadID, comment.ID),
		Author:     author,
		AuthorType: "user",
		Body:       comment.Content,
		CreatedAt:  createdAt,
		Integrity:  apiintegrity.Unapproved,
	}
}

// formatADOThreadCommentID encodes the three identifiers ADO's PATCH/DELETE
// thread-comment endpoints need into one opaque Comment.ID the stage layer can
// round-trip.
func formatADOThreadCommentID(pullID string, threadID, commentID int) string {
	return fmt.Sprintf("%s/%d/%d", pullID, threadID, commentID)
}

// parseADOThreadCommentID splits a composite "<pullID>/<threadId>/<commentId>"
// back into its three path segments, validating each is present.
func parseADOThreadCommentID(id string) (pullID, threadID, commentID string, err error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("invalid ado thread comment id %q: want \"<pullId>/<threadId>/<commentId>\"", id)
	}
	if _, convErr := strconv.Atoi(parts[1]); convErr != nil {
		return "", "", "", fmt.Errorf("invalid ado thread comment id %q: thread id is not numeric", id)
	}
	if _, convErr := strconv.Atoi(parts[2]); convErr != nil {
		return "", "", "", fmt.Errorf("invalid ado thread comment id %q: comment id is not numeric", id)
	}
	return parts[0], parts[1], parts[2], nil
}

type adoPullRequestThreadsResponse struct {
	Value []adoPullRequestThread `json:"value"`
}

type adoPullRequestThread struct {
	ID       int                           `json:"id"`
	Status   string                        `json:"status"`
	Comments []adoPullRequestThreadComment `json:"comments"`
}

type adoPullRequestThreadComment struct {
	ID              int         `json:"id"`
	ParentCommentID int         `json:"parentCommentId"`
	Content         string      `json:"content"`
	CommentType     string      `json:"commentType"`
	Author          adoIdentity `json:"author"`
	PublishedDate   string      `json:"publishedDate"`
}

// adoConnectionData is the minimal shape of the ADO connectionData response —
// the authenticated identity's id and display names.
type adoConnectionData struct {
	AuthenticatedUser struct {
		ID                  string `json:"id"`
		ProviderDisplayName string `json:"providerDisplayName"`
		CustomDisplayName   string `json:"customDisplayName"`
	} `json:"authenticatedUser"`
}
