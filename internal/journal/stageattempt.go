package journal

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// StageAttemptID is the existing read-service identity for one durable task
// or reviewer dispatch. The start sequence anchors it across live reads,
// completion, replay, and recovery; attempt numbers alone repeat on repasses.
func StageAttemptID(runID string, branch int, stage string, startedSeq uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%d", runID, branch, stage, startedSeq)))
	return "sta_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// ReviewerAttemptEvent records dispatch lifecycle, not proof that an adapter
// launched a model. A successful dispatch may report a synthesized verdict.
func ReviewerAttemptEvent(kind EventType, gate string, attempt int, class AttemptClass) Event {
	return Event{Type: kind, Stage: gate, Gate: gate, Attempt: attempt, AttemptClass: class}
}

// ReviewerContinuation finds a dispatch interrupted before its gate settled.
// An explicit human rerun or new parallel block starts a new visit instead.
func ReviewerContinuation(events []Event, gate string, branch int) int {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type == EventRunFinished || (branch != 0 && event.Type == EventParallelStarted) {
			return 0
		}
		if event.Branch != branch {
			continue
		}
		if (event.Type == EventGateEvaluated && event.Gate == gate) ||
			(event.Type == EventStageRerunRequested && event.Stage == gate) {
			return 0
		}
		if event.Type == EventReviewerStarted && event.Gate == gate {
			return event.Attempt
		}
	}
	return 0
}
