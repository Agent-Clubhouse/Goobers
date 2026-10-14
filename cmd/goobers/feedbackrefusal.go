package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

// Unanswered-feedback refusals (#6134) route the PR to remediation once per
// feedback comment. If merge-pr refuses again over the SAME comment — a
// remediation pass ran and still did not answer it, or the comment is
// misclassified (an unrecognised automation account, an unmarked reply under
// shared identity) — the PR is parked for a human instead of cycling through
// review and remediation forever: refusals never count toward demotion, so
// nothing else would bound that loop.

// feedbackRefusalPayloadPrefix marks the sticky bookkeeping comment that
// records which feedback comment the last refusal routed to remediation.
const feedbackRefusalPayloadPrefix = "<!-- feedback-refusal: "

var (
	feedbackRefusalPattern   = regexp.MustCompile(`(?s)<!-- feedback-refusal: (.*?) -->`)
	feedbackRefusalKeyFormat = regexp.MustCompile(`\[feedback-comment id=(\S+) at=(\S*)\]`)
)

// feedbackRefusalState is the sticky record's payload.
type feedbackRefusalState struct {
	Comment    string    `json:"comment"`
	HeadSHA    string    `json:"headSha,omitempty"`
	RecordedAt time.Time `json:"recordedAt"`
}

// feedbackCommentToken renders the machine-readable identity of the refused
// comment merge-pr appends to its reason: a stable key (provider id, else
// URL, else author and time) and its creation time.
func feedbackCommentToken(comment providers.Comment) string {
	key := strings.TrimSpace(comment.ID)
	if key == "" {
		key = strings.TrimSpace(comment.URL)
	}
	at := ""
	if comment.CreatedAt != nil {
		at = comment.CreatedAt.UTC().Format(time.RFC3339)
	}
	if key == "" {
		key = comment.Author + "@" + at
	}
	return fmt.Sprintf("[feedback-comment id=%s at=%s]", strings.ReplaceAll(key, " ", "_"), at)
}

// parseFeedbackCommentToken recovers the comment key and time from a refusal
// reason; ok is false for a reason without one (an older merge-pr).
func parseFeedbackCommentToken(reason string) (key string, at time.Time, ok bool) {
	match := feedbackRefusalKeyFormat.FindStringSubmatch(reason)
	if match == nil {
		return "", time.Time{}, false
	}
	parsed, _ := time.Parse(time.RFC3339, match[2])
	return match[1], parsed, true
}

// feedbackRefusalTransport is the provider surface the routing needs: the PR's
// comments, a sticky record and a one-off comment, and the PR's labels. On
// Azure DevOps all of it is pull-request threads and labels, never the work
// item that shares the PR's number.
type feedbackRefusalTransport interface {
	comments(ctx context.Context) ([]providers.Comment, error)
	putSticky(ctx context.Context, existingID, body string) error
	post(ctx context.Context, body string) error
	labels(ctx context.Context, add, remove []string) error
}

func newFeedbackRefusalTransport(provider remediationProvider, repo providers.RepositoryRef, number int) feedbackRefusalTransport {
	if ado, ok := provider.(mergeReviewADOProvider); ok {
		return adoFeedbackRefusalTransport{provider: ado.ADOProvider, repo: repo, pullID: strconv.Itoa(number)}
	}
	return forgeFeedbackRefusalTransport{provider: provider, repo: repo, number: number}
}

type forgeFeedbackRefusalTransport struct {
	provider remediationProvider
	repo     providers.RepositoryRef
	number   int
}

func (t forgeFeedbackRefusalTransport) comments(ctx context.Context) ([]providers.Comment, error) {
	return t.provider.ListComments(ctx, t.repo, strconv.Itoa(t.number))
}

func (t forgeFeedbackRefusalTransport) putSticky(ctx context.Context, existingID, body string) error {
	return postOrRecreateRemediationComment(ctx, t.provider, t.repo, t.number, existingID, body)
}

func (t forgeFeedbackRefusalTransport) post(ctx context.Context, body string) error {
	_, err := t.provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{Repository: t.repo, ID: strconv.Itoa(t.number), Comment: body})
	return err
}

func (t forgeFeedbackRefusalTransport) labels(ctx context.Context, add, remove []string) error {
	_, err := t.provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{Repository: t.repo, ID: strconv.Itoa(t.number), AddLabels: add, RemoveLabels: remove})
	return err
}

type adoFeedbackRefusalTransport struct {
	provider *providers.ADOProvider
	repo     providers.RepositoryRef
	pullID   string
}

func (t adoFeedbackRefusalTransport) comments(ctx context.Context) ([]providers.Comment, error) {
	return t.provider.ListPullRequestThreadComments(ctx, t.repo, t.pullID)
}

func (t adoFeedbackRefusalTransport) putSticky(ctx context.Context, existingID, body string) error {
	return postOrRecreateRemediationThreadComment(ctx, t.provider, t.repo, t.pullID, existingID, body)
}

func (t adoFeedbackRefusalTransport) post(ctx context.Context, body string) error {
	_, err := t.provider.PostPullRequestThreadComment(ctx, t.repo, t.pullID, body)
	return err
}

func (t adoFeedbackRefusalTransport) labels(ctx context.Context, add, remove []string) error {
	if len(add) > 0 {
		if err := t.provider.AddPullRequestLabels(ctx, t.repo, t.pullID, add); err != nil {
			return err
		}
	}
	for _, label := range remove {
		if err := t.provider.RemovePullRequestLabel(ctx, t.repo, t.pullID, label); err != nil {
			return err
		}
	}
	return nil
}

// latestFeedbackRefusal returns the newest sticky refusal record and its id.
func latestFeedbackRefusal(comments []providers.Comment) (feedbackRefusalState, string, bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		match := feedbackRefusalPattern.FindStringSubmatch(comments[i].Body)
		if match == nil {
			continue
		}
		var state feedbackRefusalState
		if json.Unmarshal([]byte(match[1]), &state) == nil {
			return state, comments[i].ID, true
		}
	}
	return feedbackRefusalState{}, "", false
}

// routeUnansweredFeedbackRefusal handles a merge-pr refusal for open,
// unanswered human review feedback (#6134). The first refusal over a comment
// routes the PR to remediation: goobers:needs-remediation added (pr-remediation
// selects on it, pr-select excludes it until remediation has answered) and
// goobers:merge-ready removed (it would keep pr-comment-watch from scanning a
// "landing" PR), with a sticky record naming the comment. A second refusal
// over the same comment parks the PR goobers:needs-human with a comment that
// acknowledges the feedback up to that comment, so pr-comment-watch un-parks
// it only when a person comments again. Neither counts toward demotion: the
// head is not stuck, and answering may not move it at all.
func routeUnansweredFeedbackRefusal(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, selectedNumber int, headSHA, reason string, stdout, stderr io.Writer) int {
	transport := newFeedbackRefusalTransport(provider, repo, selectedNumber)
	fail := func(what string, err error) int {
		return failProviderStage(stderr, fmt.Sprintf("%s PR #%d for unanswered review feedback", what, selectedNumber), err, "")
	}
	comments, err := transport.comments(ctx)
	if err != nil {
		return fail("read the refusal record of", err)
	}
	key, at, keyed := parseFeedbackCommentToken(reason)
	prior, priorID, found := latestFeedbackRefusal(comments)
	if keyed && found && prior.Comment == key {
		if err := transport.labels(ctx, []string{providers.LabelNeedsHuman}, []string{needsRemediationLabel, mergeReadyLabel}); err != nil {
			return fail("park", err)
		}
		if err := transport.post(ctx, renderFeedbackRefusalEscalation(reason, at)); err != nil {
			return fail("explain the park of", err)
		}
		pf(stdout, "PR #%d: merge refused again over the same unanswered review feedback after remediation — parked %s\n", selectedNumber, providers.LabelNeedsHuman)
		return 0
	}
	if err := transport.labels(ctx, []string{needsRemediationLabel}, []string{mergeReadyLabel}); err != nil {
		return fail("route", err)
	}
	if keyed {
		payload, _ := json.Marshal(feedbackRefusalState{Comment: key, HeadSHA: headSHA, RecordedAt: time.Now().UTC()})
		body := "Not merging yet: open review feedback has no answer from Goobers, so this pull request was routed to remediation (`" +
			needsRemediationLabel + "`). Reason: " + reason + "\n\n" + feedbackRefusalPayloadPrefix + string(payload) + " -->"
		if err := transport.putSticky(ctx, priorID, body); err != nil {
			return fail("record the refusal of", err)
		}
	}
	pf(stdout, "PR #%d: unanswered human review feedback — routed to remediation (%s), not recorded toward demotion\n", selectedNumber, needsRemediationLabel)
	return 0
}

// renderFeedbackRefusalEscalation explains the park. Its feedback-ack frontier
// covers the refused comment: the escalation is Goobers' answer to it ("a
// person must decide"), so neither pr-comment-watch nor the merge gate sees
// it as unanswered again; a new comment from a person is new feedback.
func renderFeedbackRefusalEscalation(reason string, at time.Time) string {
	body := "**Parked for a human** (`" + providers.LabelNeedsHuman + "`): merge-review refused to land this pull request " +
		"again over the same review feedback after a remediation pass did not answer it. Please answer or resolve the " +
		"feedback (or reply to it), then remove the label.\n\nRefusal: " + reason
	return withFeedbackAck(body, feedbackAckMarker(at.Truncate(time.Second)))
}
