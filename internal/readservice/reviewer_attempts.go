package readservice

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// A new dispatch proves an earlier unclosed dispatch in this scope ended.
// Keep its original ID and start anchor; never merge a recovery into it.
func closeInterruptedReviewerAttempts(attempts []StageAttempt, next journal.Event) {
	closeInterruptedAttempts(
		attempts,
		next,
		"reviewer_interrupted",
		"Reviewer dispatch interrupted before completion was recorded",
	)
}

func closeInterruptedStageAttempts(attempts []StageAttempt, next journal.Event) {
	closeInterruptedAttempts(
		attempts,
		next,
		"stage_interrupted",
		"Stage dispatch interrupted before completion was recorded",
	)
}

func closeInterruptedAttempts(attempts []StageAttempt, next journal.Event, code, message string) {
	for i := range attempts {
		attempt := &attempts[i]
		if attempt.FinishedSeq != 0 || attempt.branch != next.Branch {
			continue
		}
		closure := next
		closure.AttemptClass = ""
		finishAttempt(attempt, closure, string(apiv1.ResultFailure), nil,
			&journal.ErrorDetail{Code: code, Message: message})
	}
}
