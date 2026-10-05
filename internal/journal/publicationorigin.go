package journal

import (
	"fmt"
	"path/filepath"
)

// PublicationOccurrenceKey binds deterministic event publication to a durable
// logical task visit. Retries keep this value; later visits acquire a new one.
const PublicationOccurrenceKey = "eventPublicationOccurrence"

// AppendPublicationStageStarted commits a publication occurrence atomically
// with its task start. The runner supplies continuation from retry state.
func (r *Run) AppendPublicationStageStarted(event Event, continuation bool) error {
	if event.Type != EventStageStarted || event.Stage == "" || event.Attempt < 1 {
		return fmt.Errorf("event publication requires a stage start")
	}
	return r.appendPrepared(event, func(started *Event, seq uint64) error {
		occurrence := StageAttemptID(r.id.RunID, started.Branch, started.Stage, seq)
		if continuation {
			prior, err := r.previousPublicationOccurrence(*started)
			if err != nil {
				return err
			}
			occurrence = prior
		}
		started.Runner = copyRunnerMeta(started.Runner)
		if started.Runner == nil {
			started.Runner = map[string]any{}
		}
		started.Runner[PublicationOccurrenceKey] = occurrence
		if _, present := started.Runner["artifactVisit"]; present {
			started.Runner["artifactVisit"] = seq
		}
		return nil
	})
}

func (r *Run) previousPublicationOccurrence(started Event) (string, error) {
	events, _, err := readEvents(filepath.Join(r.dir, fileEvents))
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != EventStageStarted || event.Stage != started.Stage || event.Branch != started.Branch {
			continue
		}
		return PublicationOccurrence(event)
	}
	return "", fmt.Errorf("event publication continuation has no bound occurrence")
}

// PublicationOccurrence validates the opaque host-only occurrence on a start.
func PublicationOccurrence(event Event) (string, error) {
	occurrence, _ := event.Runner[PublicationOccurrenceKey].(string)
	if event.Type != EventStageStarted || event.Seq == 0 || !childStageIdentity.MatchString(occurrence) {
		return "", fmt.Errorf("event publication origin is not bound to a stage start")
	}
	return occurrence, nil
}
