package gate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/goobers/goobers/providers"
)

// Commenter is the minimal provider seam EscalationNotifier needs.
// providers.BacklogProvider satisfies it directly via UpdateWorkItem.
// UpdateWorkItem (not UpdateWorkItemStatus) is deliberate: it takes a
// comment-only request with no other field set, so it cannot accidentally
// touch the item's processing-status label — UpdateWorkItemStatus's entire
// purpose is mirroring that label, making it the wrong seam for a pure
// annotation (flagged in #63 QA review, confirmed against #12's provider).
type Commenter interface {
	ListComments(ctx context.Context, repository providers.RepositoryRef, itemID string) ([]providers.Comment, error)
	UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error)
	UpdateComment(ctx context.Context, repository providers.RepositoryRef, commentID, body string) error
}

// EscalationNotifier surfaces a run's escalation to whoever is watching the
// driving backlog item — issue #20's "Escalate target behavior at V0 ...
// surfaced via ... a provider comment on the driving issue/PR if one exists."
// CLI status surfacing (`goobers status`) is the local runner's (#17) job;
// this covers the provider-comment half.
type EscalationNotifier struct {
	Poster Commenter
}

// NotifyEscalated posts a comment on itemID explaining which gate escalated
// the run and why.
func (n *EscalationNotifier) NotifyEscalated(ctx context.Context, repository providers.RepositoryRef, itemID, runID string, seq uint64, r Result, reason string) error {
	comment := fmt.Sprintf(
		"Goobers run escalated at gate %q after %d repass attempt(s) (last outcome: %q). %s",
		r.Gate, r.Attempt, r.Outcome, reason,
	)
	return n.post(ctx, repository, itemID, runID, seq, comment)
}

// NotifyStageEscalated posts a comment on itemID explaining which stage
// directly escalated the run and why.
func (n *EscalationNotifier) NotifyStageEscalated(ctx context.Context, repository providers.RepositoryRef, itemID, runID string, seq uint64, stage, reason string) error {
	comment := fmt.Sprintf("Goobers run escalated at stage %q. %s", stage, reason)
	return n.post(ctx, repository, itemID, runID, seq, comment)
}

// post is a no-op without a poster or driving item: not every run has one.
func (n *EscalationNotifier) post(ctx context.Context, repository providers.RepositoryRef, itemID, runID string, seq uint64, comment string) error {
	if n == nil || n.Poster == nil || itemID == "" {
		return nil
	}
	if err := PostRunComment(ctx, n.Poster, repository, itemID, runID, seq, comment); err != nil {
		return fmt.Errorf("notify escalation on %s#%s: %w", repository.Name, itemID, err)
	}
	return nil
}

// PostRunComment posts one comment for a journal event. The marker makes a
// repeated call a no-op and reconciles a POST whose response was lost.
func PostRunComment(ctx context.Context, poster Commenter, repository providers.RepositoryRef, itemID, runID string, seq uint64, comment string) error {
	marker := runCommentMarker(runID, seq)
	body := strings.TrimSpace(comment) + "\n\n" + marker
	exists, err := markedCommentExists(ctx, poster, repository, itemID, marker)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repository,
		ID:         itemID,
		Comment:    body,
	}); err != nil {
		exists, reconcileErr := markedCommentExists(ctx, poster, repository, itemID, marker)
		if reconcileErr != nil {
			return errors.Join(err, fmt.Errorf("reconcile run comment after failed post: %w", reconcileErr))
		}
		if !exists {
			return err
		}
	}
	return nil
}

func markedCommentExists(ctx context.Context, poster Commenter, repository providers.RepositoryRef, itemID, marker string) (bool, error) {
	comments, err := poster.ListComments(ctx, repository, itemID)
	if err != nil {
		return false, fmt.Errorf("list comments for run notification: %w", err)
	}
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			return true, nil
		}
	}
	return false, nil
}

func runCommentMarker(runID string, seq uint64) string {
	return "<!-- goobers:run-notification run=" + url.QueryEscape(runID) + " seq=" + strconv.FormatUint(seq, 10) + " -->"
}

const failureStreakMarker = "<!-- goobers:failure-streak"

// HumanParkLabels are the labels that hold an item for a human decision and
// that an operator removes to send it back to automated retry: needs-human
// (the circuit breaker, blocked handler and curator park) and merge-escalated
// (merge review and pr-remediation escalating a PR, #5430). The failure-streak
// comment names whichever of these actually holds the item instead of a fixed
// needs-human, which told the operator of every merge-review escalation to
// remove a label the PR did not carry.
var HumanParkLabels = []string{providers.LabelNeedsHuman, providers.LabelMergeEscalated}

// WorkItemReader is the optional Commenter extension UpsertFailureComment
// uses to read the item's current labels. A poster without it (or a failed
// read) yields a hedged instruction rather than a guessed label.
type WorkItemReader interface {
	GetWorkItem(ctx context.Context, repository providers.RepositoryRef, itemID string) (providers.WorkItem, error)
}

// failurePark is the human-park state the failure-streak comment reports.
// Labels are the park labels known to hold the item; Complete is false when
// the item's labels could not be read, so Labels may be missing some.
type failurePark struct {
	Labels   []string
	Complete bool
}

// resolveFailurePark combines the park labels the caller is applying now with
// the human-park labels the item already carries.
func resolveFailurePark(ctx context.Context, poster Commenter, repository providers.RepositoryRef, itemID string, applying []string) failurePark {
	var present []string
	complete := false
	if reader, ok := poster.(WorkItemReader); ok {
		if item, err := reader.GetWorkItem(ctx, repository, itemID); err == nil {
			present, complete = item.Labels, true
		}
	}
	var labels []string
	for _, park := range HumanParkLabels {
		if label, ok := matchLabel(present, park); ok {
			labels = append(labels, label)
		} else if _, ok := matchLabel(applying, park); ok {
			labels = append(labels, park)
		}
	}
	return failurePark{Labels: labels, Complete: complete}
}

func matchLabel(labels []string, want string) (string, bool) {
	for _, label := range labels {
		if strings.EqualFold(label, want) {
			return label, true
		}
	}
	return "", false
}

func codeList(labels []string) string {
	quoted := make([]string, len(labels))
	for i, label := range labels {
		quoted[i] = "`" + label + "`"
	}
	return strings.Join(quoted, " and ")
}

// retryInstruction tells the operator how to send the item back to retry,
// naming only labels known to hold it.
func (p failurePark) retryInstruction() string {
	switch {
	case len(p.Labels) > 0 && p.Complete:
		return fmt.Sprintf("Remove %s and re-approve to retry.", codeList(p.Labels))
	case len(p.Labels) > 0:
		return fmt.Sprintf("Remove %s (and any other human park label on this item: %s) and re-approve to retry.", codeList(p.Labels), codeList(HumanParkLabels))
	case p.Complete:
		return fmt.Sprintf("No human park label (%s) is on this item, so no label removal is needed to retry.", codeList(HumanParkLabels))
	default:
		return fmt.Sprintf("If a human park label (%s) is on this item, remove it and re-approve to retry.", codeList(HumanParkLabels))
	}
}

func failureStreakBody(count int, stage, latestRunID, latestRunURL string, park failurePark) string {
	stageInfo := ""
	if stage != "" {
		stageInfo = fmt.Sprintf(" at stage `%s`", stage)
	}
	return fmt.Sprintf(
		"Goobers: **%d consecutive terminal failure(s)**%s. Latest run: [`%s`](%s). "+
			"Classification: **genuine/work failure** (infra/transient failures are excluded from this streak). "+
			"%s\n\n"+
			"<!-- goobers:failure-streak data-count=\"%d\" -->",
		count, stageInfo, latestRunID, latestRunURL, park.retryInstruction(), count,
	)
}

// failureStreakDataCountPattern extracts the marker's data-count attribute.
// Used only for Goobers#3025's one-release migration-on-read: an instance
// upgrading to the durable scheduler-state key still has its prior streak
// recorded solely in this comment marker, and that value must seed the new
// key rather than silently resetting to zero.
var failureStreakDataCountPattern = regexp.MustCompile(`data-count="(\d+)"`)

// ParseFailureStreakCount extracts the count recorded in a failure-streak
// marker comment, or false if body carries no marker or the marker is
// malformed. A malformed count (missing digits, or a value regexp cannot have
// produced) is reported as false, never as zero — the caller must treat an
// unparsable legacy value as "no legacy value to migrate", not "count is
// zero", so a corrupted comment cannot silently reset an item's streak.
func ParseFailureStreakCount(body string) (int, bool) {
	if !strings.Contains(body, failureStreakMarker) {
		return 0, false
	}
	m := failureStreakDataCountPattern.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	count, err := strconv.Atoi(m[1])
	if err != nil || count < 0 {
		return 0, false
	}
	return count, true
}

// UpsertFailureComment creates or updates the single failure-streak tracking
// comment on an item. Instead of posting one comment per failed run (which
// buries the issue thread), it maintains one rolling comment with the current
// count. applying lists park labels the caller is adding in the same
// transition (the circuit breaker's needs-human at threshold), which the item's
// current labels do not show yet.
func UpsertFailureComment(ctx context.Context, poster Commenter, repository providers.RepositoryRef, itemID string, count int, stage, runID, runURL string, applying []string) error {
	body := failureStreakBody(count, stage, runID, runURL, resolveFailurePark(ctx, poster, repository, itemID, applying))
	comments, err := poster.ListComments(ctx, repository, itemID)
	if err != nil {
		return fmt.Errorf("list comments for failure upsert: %w", err)
	}
	for _, c := range comments {
		if strings.Contains(c.Body, failureStreakMarker) {
			if err := poster.UpdateComment(ctx, repository, c.ID, body); err == nil {
				return nil
			}
			// ADO cannot edit work-item comments. Posting a new marker keeps
			// the latest count durable; CountFailureStreak uses that marker.
			break
		}
	}
	if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repository,
		ID:         itemID,
		Comment:    body,
	}); err != nil {
		return fmt.Errorf("post failure streak comment: %w", err)
	}
	return nil
}

// ResetFailureComment records a successful terminal run without creating a
// marker comment for items that have never failed.
func ResetFailureComment(ctx context.Context, poster Commenter, repository providers.RepositoryRef, itemID, runID, runURL string) error {
	comments, err := poster.ListComments(ctx, repository, itemID)
	if err != nil {
		return fmt.Errorf("list comments for failure reset: %w", err)
	}
	body := fmt.Sprintf(
		"Goobers: failure streak reset after successful terminal run [`%s`](%s).\n\n"+
			"<!-- goobers:failure-streak data-count=\"0\" -->",
		runID, runURL,
	)
	foundMarker := false
	for _, c := range comments {
		if strings.Contains(c.Body, failureStreakMarker) {
			foundMarker = true
			if err := poster.UpdateComment(ctx, repository, c.ID, body); err == nil {
				return nil
			}
			break
		}
	}
	if !foundMarker {
		return nil
	}
	if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repository,
		ID:         itemID,
		Comment:    body,
	}); err != nil {
		return fmt.Errorf("post failure streak reset: %w", err)
	}
	return nil
}
