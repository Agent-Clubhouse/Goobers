package gate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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

// CountFailureStreak returns the number of consecutive terminal failures
// recorded on an item by scanning for the failure-streak comment marker.
// Returns 0 when no streak comment exists.
func CountFailureStreak(ctx context.Context, poster Commenter, repository providers.RepositoryRef, itemID string) (int, string, error) {
	comments, err := poster.ListComments(ctx, repository, itemID)
	if err != nil {
		return 0, "", fmt.Errorf("list comments for failure streak: %w", err)
	}
	var latestCount int
	var latestID string
	foundMarker := false
	for _, c := range comments {
		if strings.Contains(c.Body, failureStreakMarker) {
			// Providers without comment editing may retain older marker
			// comments after each update. The newest marker is authoritative.
			latestCount = parseStreakCount(c.Body)
			latestID = c.ID
			foundMarker = true
		}
	}
	if foundMarker {
		return latestCount, latestID, nil
	}
	return 0, "", nil
}

func parseStreakCount(body string) int {
	const prefix = "data-count=\""
	idx := strings.Index(body, prefix)
	if idx < 0 {
		return 0
	}
	rest := body[idx+len(prefix):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}

func failureStreakBody(count int, stage, latestRunID, latestRunURL string) string {
	stageInfo := ""
	if stage != "" {
		stageInfo = fmt.Sprintf(" at stage `%s`", stage)
	}
	return fmt.Sprintf(
		"Goobers: **%d consecutive terminal failure(s)**%s. Latest run: [`%s`](%s). "+
			"Remove `%s` and re-approve to retry.\n\n"+
			"<!-- goobers:failure-streak data-count=\"%d\" -->",
		count, stageInfo, latestRunID, latestRunURL, providers.LabelNeedsHuman, count,
	)
}

// UpsertFailureComment creates or updates the single failure-streak tracking
// comment on an item. Instead of posting one comment per failed run (which
// buries the issue thread), it maintains one rolling comment with the current
// count.
func UpsertFailureComment(ctx context.Context, poster Commenter, repository providers.RepositoryRef, itemID string, count int, stage, runID, runURL string) error {
	body := failureStreakBody(count, stage, runID, runURL)
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
