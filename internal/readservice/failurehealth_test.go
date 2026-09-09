package readservice

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestAttemptFailureOutcomePreservesStartClass(t *testing.T) {
	at := time.Now()
	attempt := StageAttempt{Class: "initial", Number: 1, StartedAt: &at}
	detail := &journal.ErrorDetail{Code: "executor_error", Message: "pod vanished"}
	event := journal.Event{Time: at.Add(time.Second), Error: detail, Runner: map[string]any{
		"retryFailureClass": "infra", "errorCode": "infra.failure", "errorClass": "infra",
	}}
	finishAttempt(&attempt, event, "failure", nil, detail)
	if attempt.Class != "initial" || attempt.Status != "failure" || attempt.Error.Code != "executor_error" || attempt.RetryFailureClass != "infra" || attempt.ErrorCode != "infra.failure" || attempt.ErrorClass != "infra" {
		t.Fatalf("attempt = %+v", attempt)
	}
	legacy := StageAttempt{Class: "initial"}
	finishAttempt(&legacy, journal.Event{Time: at}, "failure", nil, &journal.ErrorDetail{Code: "executor_error", Message: "GoobersInfrastructureFailure"})
	if legacy.ErrorClass != "executor" || legacy.RetryFailureClass != "" {
		t.Fatalf("message guessed as infrastructure: %+v", legacy)
	}
}
