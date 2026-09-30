package providers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ADOFeedbackThreadPageLimit bounds how many thread pages
// ListPullRequestFeedbackComments reads for one pull request. ADO returns a
// pull request's threads in one response in practice; the bound only turns a
// runaway continuation chain into an explicit error instead of an unbounded
// read or a silently partial comment set.
const ADOFeedbackThreadPageLimit = 20

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
	entries := make([]adoFeedbackEntry, 0)
	for _, thread := range threads {
		if adoSystemThread(thread) {
			continue
		}
		for _, comment := range thread.Comments {
			if comment.IsDeleted || strings.EqualFold(comment.CommentType, "system") {
				continue
			}
			entry, err := adoFeedbackCommentEntry(pullID, thread.ID, comment)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}
	}
	return sortedADOFeedbackComments(entries), nil
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
		return adoFeedbackEntry{}, fmt.Errorf("ado pull request %s thread %d comment %d has no author id", pullID, threadID, comment.ID)
	}
	if mapped.CreatedAt == nil {
		return adoFeedbackEntry{}, fmt.Errorf("ado pull request %s thread %d comment %d has an unreadable publishedDate %q", pullID, threadID, comment.ID, comment.PublishedDate)
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
