package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/providers"
)

// Open human review feedback (#6134) is the flat, time-ordered list of a pull
// request's general comments, submitted review bodies and review-thread
// comments in which a resolved thread contributes only Goobers' own comments:
// a resolved conversation is no longer open feedback, but a Goobers response
// posted in it still acknowledges what it answered. merge-pr refuses to land
// while the newest human entry is unacknowledged by any Goobers response
// (latestUnaddressedHumanComment, the rule pr-comment-watch routes on), and on
// GitHub and Gitea rebase-pr reads the same list for its human-comment cause.
// A PR the gate refuses is therefore always one remediation acts on, never one
// it clears and hands straight back.

// feedbackThread is one review conversation: its comments, oldest first, and
// whether the forge reports it resolved.
type feedbackThread struct {
	resolved bool
	comments []providers.Comment
}

// openFeedbackComments returns general followed by every thread's comments,
// dropping a resolved thread's comments that are not Goobers' own, ordered by
// creation time. The sort is stable and places untimestamped entries last, so
// ties keep their provider order.
func openFeedbackComments(general []providers.Comment, threads []feedbackThread, classifier commentClassifier) []providers.Comment {
	out := append([]providers.Comment(nil), general...)
	for _, thread := range threads {
		for _, comment := range thread.comments {
			if thread.resolved && classifier.origin(comment) != commentFromGoobers {
				continue
			}
			out = append(out, comment)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].CreatedAt, out[j].CreatedAt
		switch {
		case a == nil:
			return false
		case b == nil:
			return true
		default:
			return a.Before(*b)
		}
	})
	return out
}

// reviewThreadFeedback maps a GitHub or Gitea review listing: a submitted
// review with a body is a general entry (a change requested in a review
// summary is feedback like any comment), and inline comments are grouped into
// their threads. Pending (unsubmitted, visible to no one else), dismissed and
// approving reviews are not feedback that waits on an answer: an approval's
// body ("LGTM") asks for nothing. The provider's account type marks
// automation (GitHub's "Bot", e.g. a code-review bot whose login carries no
// "[bot]" suffix); a "[bot]" login is automation where no type is reported.
func reviewThreadFeedback(listing providers.PullRequestReviewThreads) (general []providers.Comment, threads []feedbackThread) {
	for _, review := range listing.Reviews {
		state := strings.ToUpper(strings.TrimSpace(review.State))
		if strings.TrimSpace(review.Body) == "" || state == "PENDING" || state == "DISMISSED" || state == "APPROVED" {
			continue
		}
		general = append(general, providers.Comment{
			ID: "review-" + strconv.FormatInt(review.ID, 10), Author: review.Author, AuthorType: feedbackAuthorType(review.AuthorType, review.Author),
			Body: review.Body, CreatedAt: review.SubmittedAt, URL: review.URL,
		})
	}
	index := map[string]int{}
	for _, inline := range listing.InlineComments {
		key := inline.ThreadID
		if key == "" {
			key = "comment-" + strconv.FormatInt(inline.ID, 10)
		}
		at, ok := index[key]
		if !ok {
			at = len(threads)
			index[key] = at
			threads = append(threads, feedbackThread{})
		}
		threads[at].resolved = threads[at].resolved || inline.IsResolved
		threads[at].comments = append(threads[at].comments, providers.Comment{
			ID: strconv.FormatInt(inline.ID, 10), Author: inline.Author, AuthorType: feedbackAuthorType(inline.AuthorType, inline.Author),
			Body: inline.Body, CreatedAt: inline.CreatedAt, URL: inline.URL,
		})
	}
	return general, threads
}

// feedbackAuthorType is the provider-reported account type, or "Bot" for a
// "[bot]" login when the provider reports none (Gitea).
func feedbackAuthorType(reported, login string) string {
	if reported != "" {
		return reported
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(login)), "[bot]") {
		return "Bot"
	}
	return ""
}

// adoFeedbackThreads maps Azure DevOps threads: general and file threads are
// both review conversations with a status.
func adoFeedbackThreads(threads []providers.PullRequestFeedbackThread) []feedbackThread {
	out := make([]feedbackThread, 0, len(threads))
	for _, thread := range threads {
		out = append(out, feedbackThread{resolved: thread.Resolved, comments: thread.Comments})
	}
	return out
}

// forgeReviewFeedbackProvider is the GitHub and Gitea feedback surface: the
// PR conversation, the native reviews and review threads, and the login.
type forgeReviewFeedbackProvider interface {
	ListComments(ctx context.Context, repo providers.RepositoryRef, id string) ([]providers.Comment, error)
	ListPullRequestReviewThreads(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestReviewThreads, error)
	AuthenticatedLogin(ctx context.Context) (string, error)
}

// readForgeFeedback is the GitHub and Gitea feedback read shared by merge-pr
// and rebase-pr: the PR conversation plus submitted review bodies as general
// entries, and the review threads.
func readForgeFeedback(ctx context.Context, provider forgeReviewFeedbackProvider, repo providers.RepositoryRef, pullID string) ([]providers.Comment, []feedbackThread, error) {
	comments, err := provider.ListComments(ctx, repo, pullID)
	if err != nil {
		return nil, nil, fmt.Errorf("list comments on PR #%s: %w", pullID, err)
	}
	listing, err := provider.ListPullRequestReviewThreads(ctx, repo, pullID)
	if err != nil {
		return nil, nil, fmt.Errorf("list review threads on PR #%s: %w", pullID, err)
	}
	reviews, threads := reviewThreadFeedback(listing)
	return append(comments, reviews...), threads, nil
}
