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

// This file implements pr.review.threads and pr.review.resolve on Azure DevOps
// (ADO-N20, docs/design/ado-parity-dsl-2-0.md §3.4). ADO pull-request threads
// are first-class: each thread carries a status, an optional file anchor and
// its comments, so gather-review-threads and resolve-review-threads run on ADO
// through the same PullRequestReviewThreadProvider/Mutator surfaces GitHub
// implements.

// adoResolvedThreadStatus is the status ResolvePullRequestReviewThread writes.
// "fixed" clears a comment-resolution branch policy (design §2 live probe).
const adoResolvedThreadStatus = "fixed"

// adoReviewThreadIsResolved maps an ADO thread status onto IsResolved. active
// and pending are unresolved; fixed, wontFix, closed and byDesign are
// resolved. Any other value — "unknown" or an absent status — is treated as
// unresolved, so the remediator sees the thread rather than silently losing
// it (the same fail-open direction as Gitea's always-live threads).
func adoReviewThreadIsResolved(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "fixed", "wontfix", "closed", "bydesign":
		return true
	default:
		return false
	}
}

// ListPullRequestReviewThreads returns every reviewer thread on an Azure DevOps
// pull request as inline comments. Reviews is always empty: ADO reviewer votes
// carry no body, so there is no native review to report.
//
// Threads are skipped when ADO synthesized them (a "system" comment type),
// when they are deleted, when they have no file anchor, or when the Goobers
// identity itself opened them (matched by authenticatedUser.id, never by
// display name — ADO-N5): the verdict, finding-history and sticky-state
// threads Goobers writes are not review feedback. A thread without a
// threadContext.filePath is a general PR conversation comment, not a review
// thread: GitHub review threads are always file-anchored, the remediation
// brief requires a path on every inline comment, and gather-pr-context's
// thread-comment read already surfaces general comments. Deleted and system
// comments inside a kept thread are dropped too.
//
// Each comment's ThreadID is the composite "<pullID>/<threadId>" so
// ResolvePullRequestReviewThread, which receives no pull id, can address the
// thread; ID is ADO's thread-local comment id and InReplyTo its
// parentCommentId. Path is threadContext.filePath without ADO's leading "/"
// (repository-relative, as on GitHub); Line is rightFileStart.line, or
// leftFileStart.line with Side "LEFT" for a comment on a deleted line.
//
// IsOutdated follows adoOutdatedThreads' rule and fails open.
func (p *ADOProvider) ListPullRequestReviewThreads(ctx context.Context, repo RepositoryRef, pullID string) (PullRequestReviewThreads, error) {
	if err := requireRepo(repo); err != nil {
		return PullRequestReviewThreads{}, err
	}
	if pullID == "" {
		return PullRequestReviewThreads{}, errPullIDRequired
	}
	threads, err := p.listADOPullRequestThreads(ctx, repo, pullID)
	if err != nil {
		return PullRequestReviewThreads{}, err
	}
	identity, err := p.AuthenticatedIdentity(ctx)
	if err != nil {
		return PullRequestReviewThreads{}, fmt.Errorf("read authenticated ADO identity to skip its own threads: %w", err)
	}
	kept := make([]adoPullRequestThread, 0, len(threads))
	for _, thread := range threads {
		if adoReviewThreadSkipped(thread, identity.ID) {
			continue
		}
		kept = append(kept, thread)
	}
	outdated := p.adoOutdatedThreads(ctx, repo, pullID, kept)
	comments := make([]PullRequestInlineComment, 0)
	for _, thread := range kept {
		for _, comment := range thread.Comments {
			if comment.IsDeleted || strings.EqualFold(comment.CommentType, "system") {
				continue
			}
			comments = append(comments, p.mapADOReviewThreadComment(repo, pullID, thread, comment, outdated[thread.ID]))
		}
	}
	return PullRequestReviewThreads{
		Reviews:        []PullRequestNativeReview{},
		InlineComments: comments,
		Integrity:      apiintegrity.Unapproved,
	}, nil
}

// adoReviewThreadSkipped reports whether a thread is not file-anchored
// reviewer feedback: deleted, a general (unanchored) conversation thread,
// synthesized by ADO, empty, or opened by the Goobers identity.
func adoReviewThreadSkipped(thread adoPullRequestThread, selfID string) bool {
	if thread.IsDeleted || adoReviewThreadPath(thread) == "" {
		return true
	}
	root, ok := adoThreadRootComment(thread)
	if !ok || strings.EqualFold(root.CommentType, "system") {
		return true
	}
	return selfID != "" && strings.EqualFold(strings.TrimSpace(root.Author.ID), selfID)
}

// adoReviewThreadPath is a thread's repository-relative file anchor, or ""
// for a general thread with no threadContext.filePath.
func adoReviewThreadPath(thread adoPullRequestThread) string {
	if thread.ThreadContext == nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(thread.ThreadContext.FilePath), "/")
}

// adoThreadRootComment is the comment that opened a thread: the first comment
// with no parent, falling back to the first comment ADO returned.
func adoThreadRootComment(thread adoPullRequestThread) (adoPullRequestThreadComment, bool) {
	if len(thread.Comments) == 0 {
		return adoPullRequestThreadComment{}, false
	}
	for _, comment := range thread.Comments {
		if comment.ParentCommentID == 0 {
			return comment, true
		}
	}
	return thread.Comments[0], true
}

func (p *ADOProvider) mapADOReviewThreadComment(repo RepositoryRef, pullID string, thread adoPullRequestThread, comment adoPullRequestThreadComment, outdated bool) PullRequestInlineComment {
	author := comment.Author.DisplayName
	if author == "" {
		author = comment.Author.UniqueName
	}
	var createdAt *time.Time
	if parsed, err := time.Parse(time.RFC3339Nano, comment.PublishedDate); err == nil {
		utc := parsed.UTC()
		createdAt = &utc
	}
	out := PullRequestInlineComment{
		ID:         int64(comment.ID),
		ThreadID:   formatADOReviewThreadID(pullID, thread.ID),
		Author:     author,
		Body:       comment.Content,
		InReplyTo:  int64(comment.ParentCommentID),
		IsResolved: adoReviewThreadIsResolved(thread.Status),
		IsOutdated: outdated,
		CreatedAt:  createdAt,
		Integrity:  apiintegrity.Unapproved,
	}
	if web := p.entityWebURL(repo, "pr", pullID); web != "" {
		out.URL = web + "?discussionId=" + strconv.Itoa(thread.ID)
	}
	if ctxt := thread.ThreadContext; ctxt != nil {
		out.Path = adoReviewThreadPath(thread)
		switch {
		case ctxt.RightFileStart != nil:
			out.Line, out.Side = ctxt.RightFileStart.Line, "RIGHT"
		case ctxt.LeftFileStart != nil:
			out.Line, out.Side = ctxt.LeftFileStart.Line, "LEFT"
		}
	}
	return out
}

// adoOutdatedThreads decides IsOutdated for each file-anchored thread. ADO
// re-anchors a thread across later iterations itself, so an older iteration
// alone does not make a thread outdated. The rule is: a thread is outdated
// when pullRequestThreadContext.iterationContext says it was written against
// an iteration older than the pull request's latest one AND the file it
// anchors to is no longer part of the pull request's latest-iteration diff —
// its anchor cannot appear in the current diff any more, which is what
// GitHub's isOutdated means. A thread with no iteration context, no file
// anchor, or written on the latest iteration is live, and so is every thread
// when either iteration read fails: the rule fails open, so feedback is never
// hidden on a guess.
func (p *ADOProvider) adoOutdatedThreads(ctx context.Context, repo RepositoryRef, pullID string, threads []adoPullRequestThread) map[int]bool {
	candidates := make([]adoPullRequestThread, 0)
	for _, thread := range threads {
		if adoThreadIteration(thread) > 0 && adoReviewThreadPath(thread) != "" {
			candidates = append(candidates, thread)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	latest, err := p.latestPullRequestIteration(ctx, repo, pullID)
	if err != nil || latest == 0 {
		return nil
	}
	older := make([]adoPullRequestThread, 0, len(candidates))
	for _, thread := range candidates {
		if adoThreadIteration(thread) < latest {
			older = append(older, thread)
		}
	}
	if len(older) == 0 {
		return nil
	}
	files, err := p.PullRequestFiles(ctx, repo, pullID)
	if err != nil {
		return nil
	}
	changed := make(map[string]bool, len(files))
	for _, file := range files {
		changed[file.Path] = true
	}
	outdated := make(map[int]bool, len(older))
	for _, thread := range older {
		if !changed[adoReviewThreadPath(thread)] {
			outdated[thread.ID] = true
		}
	}
	return outdated
}

// adoThreadIteration is the iteration a thread was written against: the
// second (right-hand) comparing iteration of its iteration context, or 0 when
// the context is absent.
func adoThreadIteration(thread adoPullRequestThread) int {
	if thread.PullRequestThreadContext == nil || thread.PullRequestThreadContext.IterationContext == nil {
		return 0
	}
	return thread.PullRequestThreadContext.IterationContext.SecondComparingIteration
}

// listADOPullRequestThreads reads every thread on a pull request, following
// x-ms-continuationtoken when ADO pages the response.
func (p *ADOProvider) listADOPullRequestThreads(ctx context.Context, repo RepositoryRef, pullID string) ([]adoPullRequestThread, error) {
	base, err := p.repoURL(repo, "pullrequests", pullID, "threads")
	if err != nil {
		return nil, err
	}
	var all []adoPullRequestThread
	seen := map[string]bool{}
	endpoint := base
	for {
		resp, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
		if err != nil {
			return nil, err
		}
		var page adoPullRequestThreadsResponse
		if err := readJSONResponse(resp, http.MethodGet, endpoint, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
		next := strings.TrimSpace(resp.Header.Get("x-ms-continuationtoken"))
		if next == "" {
			return all, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("ado pull request %s threads: continuation token repeated", pullID)
		}
		seen[next] = true
		endpoint, err = addQuery(base, url.Values{"continuationToken": []string{next}})
		if err != nil {
			return nil, err
		}
	}
}

// ReplyPullRequestReviewThread posts a reply into an existing Azure DevOps
// pull-request thread. ADO comment ids are thread-local, so the reply is
// addressed by req.ThreadID — the composite "<pullID>/<threadId>" the list
// returned — and req.CommentID names the parent comment inside that thread.
func (p *ADOProvider) ReplyPullRequestReviewThread(ctx context.Context, req PullRequestReviewThreadReply) (PullRequestInlineComment, error) {
	if err := requireRepo(req.Repository); err != nil {
		return PullRequestInlineComment{}, err
	}
	if req.PullID == "" || req.CommentID < 1 || strings.TrimSpace(req.Body) == "" {
		return PullRequestInlineComment{}, fmt.Errorf("pull id, comment id, and reply body are required")
	}
	pullID, threadID, err := parseADOReviewThreadID(req.ThreadID)
	if err != nil {
		return PullRequestInlineComment{}, err
	}
	if pullID != req.PullID {
		return PullRequestInlineComment{}, fmt.Errorf("review thread %q does not belong to pull request %s", req.ThreadID, req.PullID)
	}
	body, err := withAttribution(req.Body, p.attribution, "review-thread-reply")
	if err != nil {
		return PullRequestInlineComment{}, err
	}
	endpoint, err := p.repoURL(req.Repository, "pullrequests", pullID, "threads", threadID, "comments")
	if err != nil {
		return PullRequestInlineComment{}, err
	}
	payload := map[string]interface{}{
		"parentCommentId": req.CommentID,
		"content":         body,
		"commentType":     adoPRThreadCommentType,
	}
	var created adoPullRequestThreadComment
	if err := p.do(ctx, http.MethodPost, endpoint, payload, &created); err != nil {
		return PullRequestInlineComment{}, err
	}
	p.recordMutation(ctx, "pr", pullID, "review-thread-reply", req.Repository)
	return PullRequestInlineComment{
		ID:        int64(created.ID),
		ThreadID:  req.ThreadID,
		Body:      created.Content,
		InReplyTo: int64(created.ParentCommentID),
	}, nil
}

// ResolvePullRequestReviewThread sets an Azure DevOps pull-request thread's
// status to "fixed", which clears a comment-resolution branch policy. threadID
// is the composite "<pullID>/<threadId>" the list returned. The thread status
// ADO echoes back must be the one written, or the resolution is unconfirmed.
func (p *ADOProvider) ResolvePullRequestReviewThread(ctx context.Context, repo RepositoryRef, threadID string) error {
	if err := requireRepo(repo); err != nil {
		return err
	}
	pullID, id, err := parseADOReviewThreadID(threadID)
	if err != nil {
		return err
	}
	endpoint, err := p.repoURL(repo, "pullrequests", pullID, "threads", id)
	if err != nil {
		return err
	}
	var updated adoPullRequestThread
	if err := p.do(ctx, http.MethodPatch, endpoint, map[string]interface{}{"status": adoResolvedThreadStatus}, &updated); err != nil {
		return err
	}
	if !strings.EqualFold(updated.Status, adoResolvedThreadStatus) {
		return fmt.Errorf("ado did not confirm review thread %q as resolved (status %q)", threadID, updated.Status)
	}
	p.recordMutation(ctx, "pr", pullID, "review-thread-resolve", repo)
	return nil
}

// formatADOReviewThreadID encodes the pull request and thread ids into the
// one opaque review-thread id the stage layer round-trips.
func formatADOReviewThreadID(pullID string, threadID int) string {
	return fmt.Sprintf("%s/%d", pullID, threadID)
}

// parseADOReviewThreadID splits a composite "<pullID>/<threadId>" back into
// its path segments; the thread id must be a positive integer.
func parseADOReviewThreadID(id string) (pullID, threadID string, err error) {
	parts := strings.Split(strings.TrimSpace(id), "/")
	if len(parts) != 2 || parts[0] == "" {
		return "", "", fmt.Errorf("invalid ado review thread id %q: want \"<pullId>/<threadId>\"", id)
	}
	if n, convErr := strconv.Atoi(parts[1]); convErr != nil || n < 1 {
		return "", "", fmt.Errorf("invalid ado review thread id %q: thread id is not a positive integer", id)
	}
	return parts[0], parts[1], nil
}

// adoThreadContext is a thread's file anchor (GitCommentThreadContext).
type adoThreadContext struct {
	FilePath       string           `json:"filePath"`
	RightFileStart *adoCommentPoint `json:"rightFileStart"`
	LeftFileStart  *adoCommentPoint `json:"leftFileStart"`
}

// adoCommentPoint is one line/offset position in a file.
type adoCommentPoint struct {
	Line   int `json:"line"`
	Offset int `json:"offset"`
}

// adoPullRequestThreadContext carries the iterations a thread was written
// against (GitPullRequestCommentThreadContext).
type adoPullRequestThreadContext struct {
	IterationContext *struct {
		FirstComparingIteration  int `json:"firstComparingIteration"`
		SecondComparingIteration int `json:"secondComparingIteration"`
	} `json:"iterationContext"`
}

var _ PullRequestReviewThreadProvider = (*ADOProvider)(nil)
var _ PullRequestReviewThreadMutator = (*ADOProvider)(nil)
