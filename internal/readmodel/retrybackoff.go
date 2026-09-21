package readmodel

import (
	"slices"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// MaxRetryBackoffs bounds retained parallel retry observations per run.
const MaxRetryBackoffs = 64

// RetryBackoff is an observed timer for one failed attempt. An expired deadline
// does not prove progress or a stall; callers stop treating it as a live wait.
type RetryBackoff struct {
	Stage      string               `json:"stage"`
	Branch     int                  `json:"branch,omitempty"`
	Attempt    int                  `json:"attempt"`
	Driver     string               `json:"driver"`
	Class      journal.AttemptClass `json:"class"`
	ObservedAt time.Time            `json:"observedAt"`
	Deadline   time.Time            `json:"deadline"`
}

// RetryBackoffState is folded into the existing operator-facts JSON column.
type RetryBackoffState struct {
	// Parallel records incomplete branch coverage even between stage events.
	Parallel  bool           `json:"parallel,omitempty"`
	Waits     []RetryBackoff `json:"waits,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
}

// After returns an independent snapshot, clearing waits when execution moves on.
func (s RetryBackoffState) After(event journal.Event) RetryBackoffState {
	if !event.KnownSchema() {
		return s
	}
	if event.Branch != 0 || event.Type == journal.EventBranchStarted || event.Type == journal.EventParallelStarted {
		s.Parallel = true
	}
	switch event.Type {
	case journal.EventRunFinished:
		return RetryBackoffState{}
	case journal.EventRunResumed, journal.EventGateOverridden:
		return RetryBackoffState{Parallel: s.Parallel}
	case journal.EventRunnerAnnotation:
		if event.Runner["kind"] == journal.RetryBackoffResetKind {
			return RetryBackoffState{Parallel: s.Parallel}
		}
	}
	if event.Type == journal.EventStageStarted || event.Type == journal.EventStageRerunRequested || event.Type == journal.EventBranchFinished {
		s.Waits = slices.DeleteFunc(slices.Clone(s.Waits), func(wait RetryBackoff) bool {
			return wait.Branch == event.Branch && (event.Type == journal.EventBranchFinished || wait.Stage == event.Stage) && !event.Time.Before(wait.ObservedAt) && (event.Type != journal.EventStageStarted || event.Attempt == 0 || event.Attempt >= wait.Attempt)
		})
		return s
	}
	wait, ok := retryBackoffObservation(event)
	if !ok {
		return s
	}
	s.Waits = slices.Clone(s.Waits)
	for i, prior := range s.Waits {
		if prior.Stage == wait.Stage && prior.Branch == wait.Branch {
			if !wait.ObservedAt.Before(prior.ObservedAt) {
				s.Waits[i] = wait
			}
			return s
		}
	}
	if len(s.Waits) >= MaxRetryBackoffs {
		s.Truncated = true
		return s
	}
	s.Waits = append(s.Waits, wait)
	return s
}

func retryBackoffObservation(event journal.Event) (RetryBackoff, bool) {
	if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != journal.RetryBackoffKind || event.Stage == "" || len(event.Stage) > 256 || event.Attempt <= 0 || event.Time.IsZero() {
		return RetryBackoff{}, false
	}
	driver, _ := event.Runner["driver"].(string)
	class, _ := event.Runner["retryClass"].(string)
	observed, _ := event.Runner["observedAt"].(string)
	deadline, _ := event.Runner["deadline"].(string)
	at, err := time.Parse(time.RFC3339Nano, observed)
	until, untilErr := time.Parse(time.RFC3339Nano, deadline)
	if err != nil || untilErr != nil || at.IsZero() || at.After(event.Time) || !until.After(at) || (driver != "local" && driver != "engine") || (class != string(journal.AttemptInfra) && class != string(journal.AttemptPolicy)) {
		return RetryBackoff{}, false
	}
	return RetryBackoff{Stage: event.Stage, Branch: event.Branch, Attempt: event.Attempt, Driver: driver, Class: journal.AttemptClass(class), ObservedAt: at, Deadline: until}, true
}
