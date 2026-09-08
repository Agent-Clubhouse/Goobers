package runner

import (
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

type mutationIssueJournal struct {
	events []journal.Event
	err    error
}

func (j *mutationIssueJournal) Append(event journal.Event) error {
	j.events = append(j.events, event)
	return j.err
}

func TestMutationSidecarIssueJournalFailureIsNotDiscarded(t *testing.T) {
	failure := errors.New("journal unavailable")
	jr := &mutationIssueJournal{err: failure}
	if err := recordMutationSidecarIssues(jr, "land", 2, journal.AttemptInfra, nil); err != nil || len(jr.events) != 0 {
		t.Fatalf("empty diagnostic wrote event: %v %+v", err, jr.events)
	}
	err := recordMutationSidecarIssues(jr, "land", 2, journal.AttemptInfra, []string{"truncated receipt", "missing provider"})
	if !errors.Is(err, failure) || len(jr.events) != 1 {
		t.Fatalf("journal failure swallowed: %v %+v", err, jr.events)
	}
	event := jr.events[0]
	if event.Type != journal.EventError || event.Stage != "land" || event.Attempt != 2 || event.AttemptClass != journal.AttemptInfra || event.Error == nil || event.Error.Code != "mutation_sidecar_read_failed" || event.Error.Message != "truncated receipt; missing provider" {
		t.Fatalf("lost diagnostic context: %+v", event)
	}
	jr.err = nil
	if err := recordMutationSidecarIssues(jr, "land", 2, journal.AttemptInfra, []string{"truncated receipt"}); err != nil {
		t.Fatalf("persisted diagnostic became fatal: %v", err)
	}
}
