package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goobers/goobers/providers"
)

const commentWatchBudgetMarker = "<!-- comment-watch-budget: "
const commentWatchMaxRoutes = 3

// The provider thread is durable across watcher restarts and visible to pod
// stages. These records are bookkeeping, never acknowledgements of feedback.
// Reserving before routing is conservative: a failed label write spends an
// attempt, but a crash cannot route repeatedly without recording the budget.
type commentWatchBudget struct {
	HeadSHA   string `json:"headSha"`
	Routes    int    `json:"routes"`
	Escalated bool   `json:"escalated,omitempty"`
}

func readCommentWatchBudget(comments []providers.Comment, classifier commentClassifier, head string) (commentWatchBudget, error) {
	state := commentWatchBudget{HeadSHA: head}
	for _, comment := range comments {
		if !classifier.isSelf(comment) {
			continue
		}
		lines := strings.Split(comment.Body, "\n")
		fenced := closedFenceLines(lines)
		for i, line := range lines {
			if fenced[i] {
				continue
			}
			if !strings.HasPrefix(line, commentWatchBudgetMarker) || !strings.HasSuffix(line, " -->") {
				continue
			}
			var candidate commentWatchBudget
			if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(line, commentWatchBudgetMarker), " -->")), &candidate); err != nil {
				return state, fmt.Errorf("invalid comment-watch budget: %w", err)
			}
			if candidate.HeadSHA != head {
				continue
			}
			if candidate.Routes < 1 || candidate.Routes > commentWatchMaxRoutes || (candidate.Escalated && candidate.Routes != commentWatchMaxRoutes) {
				return state, fmt.Errorf("invalid comment-watch budget for head %s", head)
			}
			if candidate.Routes > state.Routes {
				state.Routes = candidate.Routes
			}
			state.Escalated = state.Escalated || candidate.Escalated
		}
	}
	return state, nil
}

func renderCommentWatchBudget(state commentWatchBudget) string {
	data, _ := json.Marshal(state)
	prose := fmt.Sprintf("Comment watcher: routing unaddressed feedback for head `%s` (attempt %d of %d).", state.HeadSHA, state.Routes, commentWatchMaxRoutes)
	if state.Escalated {
		prose = fmt.Sprintf("Comment watcher paused automatic remediation for head `%s` after %d routing attempts without a new commit. Unaddressed feedback remains and requires human review. This may be a shared-identity comment loop; unmarked comments still count as human feedback. Push a new commit after addressing the feedback to reset this head's budget.", state.HeadSHA, commentWatchMaxRoutes)
	}
	return prose + "\n\n" + commentWatchBudgetMarker + string(data) + " -->"
}

// reserveCommentWatchRoute bounds shared-identity reroutes without guessing
// whether an unmarked comment was posted by a person or an agent. Exhaustion
// parks visibly; it never treats the feedback as answered or permits landing.
func reserveCommentWatchRoute(ctx context.Context, forge prCommentWatchForge, pr providers.PullRequestSummary, comments []providers.Comment, classifier commentClassifier) (bool, error) {
	if classifier.dedicated {
		return false, nil
	}
	if strings.TrimSpace(pr.HeadSHA) == "" {
		return false, fmt.Errorf("cannot bound comment routing without PR head SHA")
	}
	// Landing scans normally read only open ADO feedback. Budget records are
	// closed informational threads, so read the complete thread for this check.
	if hasAnyLabel(pr.Labels, []string{mergeReadyLabel}) {
		all := pr
		all.Labels = nil
		var err error
		comments, err = forge.listComments(ctx, all, classifier)
		if err != nil {
			return false, fmt.Errorf("read comment routing budget: %w", err)
		}
	}
	state, err := readCommentWatchBudget(comments, classifier, pr.HeadSHA)
	if err != nil {
		return false, err
	}
	exhausted := state.Routes >= commentWatchMaxRoutes
	if !state.Escalated {
		if exhausted {
			state.Escalated = true
		} else {
			state.Routes++
		}
		if err := forge.recordRouting(ctx, pr, renderCommentWatchBudget(state)); err != nil {
			return false, fmt.Errorf("record comment routing budget: %w", err)
		}
	}
	return exhausted, nil
}
