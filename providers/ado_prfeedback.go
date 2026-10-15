package providers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ADOFeedbackThreadPageLimit bounds how many thread pages
// ListPullRequestFeedbackComments reads for one pull request. ADO returns a
// pull request's threads in one response in practice; the bound only turns a
// runaway continuation chain into an explicit error instead of an unbounded
// read or a silently partial comment set.
const ADOFeedbackThreadPageLimit = 20

// ErrUnreadableFeedbackComment marks a feedback read that failed because one
// comment could not be mapped (no author id, an unparseable publish time): the
// comment's origin and order are unknowable, so the read refuses to return a
// set that silently omits it. A caller can tell this apart from a transport
// failure and fail closed on the comment rather than on the whole stage.
var ErrUnreadableFeedbackComment = errors.New("unreadable pull-request feedback comment")

// adoCodeReviewThreadTypeProperty is the thread property ADO sets on threads it
// synthesizes for pull-request events: votes, pushes, reviewer and policy
// status updates. Threads people write do not carry it.
const adoCodeReviewThreadTypeProperty = "CodeReviewThreadType"

// ListPullRequestFeedbackComments returns every live, person-or-tool-written
// comment on an Azure DevOps pull request: general conversation threads and
// file-anchored review threads, replies included. It is the comment read
// pr-comment-watch computes its human and Goobers watermarks from.
//
// It skips deleted threads, deleted comments, system comments and whole
// threads ADO synthesized (a CodeReviewThreadType property), so build, vote,
// push and policy events never count as feedback. The result is ordered by
// server publish time, then thread id, then comment id: a deterministic order
// a watermark can break ties on.
//
// It fails closed instead of returning a partial set: a thread read that
// needs more than ADOFeedbackThreadPageLimit pages, a comment with no author
// id (identity cannot be compared) and a comment whose publishedDate does not
// parse are errors.
func (p *ADOProvider) ListPullRequestFeedbackComments(ctx context.Context, repo RepositoryRef, pullID string) ([]Comment, error) {
	threads, err := p.adoFeedbackThreadEntries(ctx, repo, pullID)
	if err != nil {
		return nil, err
	}
	entries := make([]adoFeedbackEntry, 0)
	for _, thread := range threads {
		entries = append(entries, thread.entries...)
	}
	return sortedADOFeedbackComments(entries), nil
}

// PullRequestFeedbackThread is one live Azure DevOps pull-request thread as
// ListPullRequestFeedbackThreads reports it: its id, whether ADO reports it
// resolved (fixed, won't fix, closed, by design), its file anchor when it has
// one, and its live comments in thread order.
type PullRequestFeedbackThread struct {
	ID       string    `json:"id"`
	Resolved bool      `json:"resolved"`
	Path     string    `json:"path,omitempty"`
	Comments []Comment `json:"comments"`
}

// ListPullRequestFeedbackThreads returns every live thread on an Azure DevOps
// pull request — general conversation threads and file-anchored review threads
// alike, whoever opened them — with its resolution status. It applies
// ListPullRequestFeedbackComments' rules (deleted and ADO-synthesized threads,
// and deleted and system comments, are skipped; a comment with no author id or
// an unreadable publishedDate fails the read) but keeps each thread whole, so a
// caller can tell an open conversation from a resolved one. merge-pr's
// unanswered-feedback gate reads it (#6134): unlike
// ListPullRequestReviewThreads it keeps general threads and threads the
// credential's own identity opened, which under shared identity are a
// person's.
func (p *ADOProvider) ListPullRequestFeedbackThreads(ctx context.Context, repo RepositoryRef, pullID string) ([]PullRequestFeedbackThread, error) {
	threads, err := p.adoFeedbackThreadEntries(ctx, repo, pullID)
	if err != nil {
		return nil, err
	}
	out := make([]PullRequestFeedbackThread, 0, len(threads))
	for _, thread := range threads {
		comments := make([]Comment, len(thread.entries))
		for i, entry := range thread.entries {
			comments[i] = entry.comment
		}
		out = append(out, PullRequestFeedbackThread{
			ID:       pullID + "/" + strconv.Itoa(thread.thread.ID),
			Resolved: adoReviewThreadIsResolved(thread.thread.Status),
			Path:     adoReviewThreadPath(thread.thread),
			Comments: comments,
		})
	}
	return out, nil
}

// adoFeedbackThread is one live thread and its mapped live comments.
type adoFeedbackThread struct {
	thread  adoPullRequestThread
	entries []adoFeedbackEntry
}

// adoFeedbackThreadEntries reads the pull request's threads (bounded by
// ADOFeedbackThreadPageLimit) and maps every live comment of every live,
// person-or-tool-written thread, failing closed on a comment it cannot map.
func (p *ADOProvider) adoFeedbackThreadEntries(ctx context.Context, repo RepositoryRef, pullID string) ([]adoFeedbackThread, error) {
	if err := requireRepo(repo); err != nil {
		return nil, err
	}
	if pullID == "" {
		return nil, errPullIDRequired
	}
	threads, err := p.listADOPullRequestThreadPages(ctx, repo, pullID, ADOFeedbackThreadPageLimit)
	if err != nil {
		return nil, err
	}
	out := make([]adoFeedbackThread, 0, len(threads))
	for _, thread := range threads {
		if adoSystemThread(thread) {
			continue
		}
		mapped := adoFeedbackThread{thread: thread}
		for _, comment := range thread.Comments {
			if comment.IsDeleted || strings.EqualFold(comment.CommentType, "system") {
				continue
			}
			entry, err := adoFeedbackCommentEntry(pullID, thread.ID, comment)
			if err != nil {
				return nil, err
			}
			mapped.entries = append(mapped.entries, entry)
		}
		if len(mapped.entries) > 0 {
			out = append(out, mapped)
		}
	}
	return out, nil
}

// adoFeedbackEntry is one mapped comment plus the ids its order breaks ties on.
type adoFeedbackEntry struct {
	comment   Comment
	at        time.Time
	threadID  int
	commentID int
}

// adoSystemThread reports whether ADO synthesized the thread, or deleted it.
func adoSystemThread(thread adoPullRequestThread) bool {
	if thread.IsDeleted {
		return true
	}
	_, synthesized := thread.Properties[adoCodeReviewThreadTypeProperty]
	return synthesized
}

// adoFeedbackCommentEntry maps one live comment, failing on the fields the
// watermark cannot do without: the author's identity id and the publish time.
func adoFeedbackCommentEntry(pullID string, threadID int, comment adoPullRequestThreadComment) (adoFeedbackEntry, error) {
	mapped := mapADOPullRequestThreadComment(pullID, threadID, comment)
	if mapped.AuthorID == "" {
		return adoFeedbackEntry{}, fmt.Errorf("%w: ado pull request %s thread %d comment %d has no author id", ErrUnreadableFeedbackComment, pullID, threadID, comment.ID)
	}
	if mapped.CreatedAt == nil {
		return adoFeedbackEntry{}, fmt.Errorf("%w: ado pull request %s thread %d comment %d has an unreadable publishedDate %q", ErrUnreadableFeedbackComment, pullID, threadID, comment.ID, comment.PublishedDate)
	}
	return adoFeedbackEntry{comment: mapped, at: *mapped.CreatedAt, threadID: threadID, commentID: comment.ID}, nil
}

// sortedADOFeedbackComments orders entries by publish time, thread id and
// comment id, and returns their comments.
func sortedADOFeedbackComments(entries []adoFeedbackEntry) []Comment {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.at.Equal(b.at) {
			return a.at.Before(b.at)
		}
		if a.threadID != b.threadID {
			return a.threadID < b.threadID
		}
		return a.commentID < b.commentID
	})
	out := make([]Comment, len(entries))
	for i, entry := range entries {
		out[i] = entry.comment
	}
	return out
}
