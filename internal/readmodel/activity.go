package readmodel

import (
	"slices"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// MaxActiveStageTimings bounds malformed/unbalanced journals as well as normal
// parallel execution. Overflow is explicit, never a fabricated single owner.
const MaxActiveStageTimings = 1024

// ActiveStage records a start, not a frozen elapsed duration. The client clock
// can advance a quiet run without waiting for another journal event.
type ActiveStage struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Branch    int       `json:"branch,omitempty"`
	Attempt   int       `json:"attempt,omitempty"`
	Goober    string    `json:"goober,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

// StageActivity is folded identically by SQLite projection and journal reads.
type StageActivity struct {
	Active    []ActiveStage
	Truncated bool
}

// After applies one known-schema event without mutating the prior snapshot.
// Heartbeats leave start times unchanged; retries replace only their own key.
func (s StageActivity) After(e journal.Event) StageActivity {
	if !e.KnownSchema() {
		return s
	}
	switch e.Type {
	case journal.EventRunFinished, journal.EventRunResumed, journal.EventGateOverridden:
		return StageActivity{}
	}
	name, kind, start, finish := activityEvent(e)
	if name == "" || (!start && !finish) {
		return s
	}
	index := -1
	for i, active := range s.Active {
		if active.Name == name && active.Kind == kind && active.Branch == e.Branch {
			index = i
			break
		}
	}
	if finish {
		if index < 0 {
			return s
		}
		// A delayed terminal record from an older attempt cannot erase its retry.
		if e.Attempt != 0 && s.Active[index].Attempt != 0 && e.Attempt != s.Active[index].Attempt {
			return s
		}
		s.Active = slices.Delete(slices.Clone(s.Active), index, index+1)
		return s
	}
	owner, _ := e.Runner["goober"].(string)
	active := ActiveStage{Name: name, Kind: kind, Branch: e.Branch, Attempt: e.Attempt, Goober: owner, StartedAt: e.Time}
	s.Active = slices.Clone(s.Active)
	if index >= 0 {
		s.Active[index] = active
	} else if len(s.Active) < MaxActiveStageTimings {
		s.Active = append(s.Active, active)
	} else {
		s.Truncated = true
	}
	return s
}

func activityEvent(e journal.Event) (name, kind string, start, finish bool) {
	switch e.Type {
	case journal.EventStageStarted:
		return e.Stage, "stage", true, false
	case journal.EventStageFinished:
		return e.Stage, "stage", false, true
	case journal.EventGateStarted:
		return e.Gate, "gate", true, false
	case journal.EventGateEvaluated:
		return e.Gate, "gate", false, true
	case journal.EventError:
		if e.Error != nil && e.Error.Code == "executor_error" {
			return e.Stage, "stage", false, true
		}
	}
	return "", "", false, false
}
