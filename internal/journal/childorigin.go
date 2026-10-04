package journal

import (
	"fmt"
	"path/filepath"
	"regexp"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	ChildWorkflowOccurrenceKey = "childWorkflowOccurrence"
	ChildWorkflowAttemptKey    = "childWorkflowAttemptId"
)

var childStageIdentity = regexp.MustCompile(`^sta_[A-Za-z0-9_-]{43}$`)

// ChildWorkflowOriginForEvent reads a bound start and verifies its attempt
// against the journal's actual sequence. Missing historical metadata refuses;
// a run must not acquire new child authority by guessing its old occurrence.
func ChildWorkflowOriginForEvent(runID string, event Event) (*apiv1.ChildWorkflowOrigin, error) {
	occurrence, _ := event.Runner[ChildWorkflowOccurrenceKey].(string)
	attempt, _ := event.Runner[ChildWorkflowAttemptKey].(string)
	if event.Type != EventStageStarted || event.Seq == 0 || !childStageIdentity.MatchString(occurrence) ||
		attempt != StageAttemptID(runID, event.Branch, event.Stage, event.Seq) {
		return nil, fmt.Errorf("child workflow origin is not bound to the committed stage start")
	}
	return &apiv1.ChildWorkflowOrigin{StageOccurrence: occurrence, AttemptID: attempt}, nil
}

// AppendChildStageStarted atomically binds identity to the committed start.
// continuation retains the last bound occurrence for this task and branch;
// false begins a new visit. The caller determines continuation from durable
// runner retry/rerun state, not from agent-authored inputs. The returned sequence
// and origin are usable only after the event and checkpoint both commit.
func (r *Run) AppendChildStageStarted(event Event, continuation bool) (uint64, *apiv1.ChildWorkflowOrigin, error) {
	if event.Type != EventStageStarted || event.Stage == "" || event.Attempt < 1 {
		return 0, nil, fmt.Errorf("child workflow origin requires a stage start")
	}
	var sequence uint64
	var origin *apiv1.ChildWorkflowOrigin
	err := r.appendPrepared(event, func(started *Event, seq uint64) error {
		attempt := StageAttemptID(r.id.RunID, started.Branch, started.Stage, seq)
		occurrence := attempt
		if continuation {
			var err error
			occurrence, err = r.previousChildOccurrence(*started)
			if err != nil {
				return err
			}
		}
		started.Runner = copyRunnerMeta(started.Runner)
		if started.Runner == nil {
			started.Runner = map[string]any{}
		}
		started.Runner[ChildWorkflowOccurrenceKey] = occurrence
		started.Runner[ChildWorkflowAttemptKey] = attempt
		if _, present := started.Runner["artifactVisit"]; present {
			started.Runner["artifactVisit"] = seq
		}
		sequence = seq
		origin = &apiv1.ChildWorkflowOrigin{StageOccurrence: occurrence, AttemptID: attempt}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return sequence, origin, nil
}

// Caller holds the writer mutex. Parallel siblings cannot append between
// reading the prior binding and committing its replacement attempt.
func (r *Run) previousChildOccurrence(started Event) (string, error) {
	events, _, err := readEvents(filepath.Join(r.dir, fileEvents))
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != EventStageStarted || event.Stage != started.Stage || event.Branch != started.Branch {
			continue
		}
		origin, err := ChildWorkflowOriginForEvent(r.id.RunID, event)
		if err != nil {
			return "", err
		}
		return origin.StageOccurrence, nil
	}
	return "", fmt.Errorf("child workflow continuation has no prior bound stage occurrence")
}
