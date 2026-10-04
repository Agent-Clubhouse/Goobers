package readservice

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// A new dispatch proves an earlier unclosed dispatch in this scope ended.
// Keep its original ID and start anchor; never merge a recovery into it.
func closeInterruptedReviewerAttempts(attempts []StageAttempt, next journal.Event) {
	for i := range attempts {
		attempt := &attempts[i]
		if attempt.FinishedSeq != 0 || attempt.branch != next.Branch {
			continue
		}
		closure := next
		closure.AttemptClass = ""
		finishAttempt(attempt, closure, string(apiv1.ResultFailure), nil,
			&journal.ErrorDetail{Code: "reviewer_interrupted", Message: "Reviewer dispatch interrupted before completion was recorded"})
	}
}
