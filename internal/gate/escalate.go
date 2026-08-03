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
