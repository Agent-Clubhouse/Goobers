package main

import (
	"errors"
	"fmt"
	"github.com/goobers/goobers/internal/journal"
)

func childPodStarted(reader *journal.Reader, stage string, attempt int, review bool) (journal.Event, error) {
	events, err := reader.Events()
	if err != nil {
		return journal.Event{}, err
	}
	startKind, endKind := journal.EventStageStarted, journal.EventStageFinished
	if review {
		startKind, endKind = journal.EventReviewerStarted, journal.EventReviewerFinished
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type == journal.EventRunFinished {
			return journal.Event{}, errors.New("child run already settled")
		}
		if e.Stage != stage || e.Attempt != attempt {
			continue
		}
		if e.Type == endKind {
			break
		}
		if e.Type == startKind && e.Branch == 0 && e.Seq > 0 && e.Seq <= 1<<31-1 && !e.Time.IsZero() {
			return e, nil
		}
	}
	return journal.Event{}, fmt.Errorf("child stage %q has no exact active journal attempt", stage)
}
