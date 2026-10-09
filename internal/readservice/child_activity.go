package readservice

import (
	"slices"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// ChildActivity exposes recorded lineage and current durable waits. It is not a
// list of all accepted children, nor evidence that any child writer has stopped.
type ChildActivity struct {
	Status string           `json:"status"`
	Parent *ChildParentLink `json:"parent,omitempty"`
	Waits  []ChildStageWait `json:"waits"`
	Parked bool             `json:"parked"`
}

// ChildParentLink identifies the parent recorded in a generated run's identity.
type ChildParentLink struct {
	RunID           string `json:"runId"`
	Workflow        string `json:"workflow"`
	StageOccurrence string `json:"stageOccurrence"`
}

// ChildStageWait is one validated host wait, independent of sibling activity.
type ChildStageWait struct {
	RunID    string    `json:"runId"`
	Stage    string    `json:"stage"`
	Branch   int       `json:"branch"`
	Action   string    `json:"action"`
	Since    time.Time `json:"since"`
	Sequence uint64    `json:"sequence"`
}

func recordedChildActivity(id journal.RunIdentity, events []journal.Event) *ChildActivity {
	unavailable := &ChildActivity{Status: "unavailable", Waits: []ChildStageWait{}}
	if id.ValidateChildLineage() != nil {
		return unavailable
	}
	projection, err := journal.ProjectChildWaits(events)
	if err != nil || len(projection.Waits) > 128 {
		return unavailable
	}
	if id.Child == nil && len(projection.Waits) == 0 {
		return nil
	}
	out := &ChildActivity{Status: "recorded", Waits: []ChildStageWait{}, Parked: projection.Parked()}
	if id.Child != nil {
		out.Parent = &ChildParentLink{RunID: id.Child.ParentRunID, Workflow: id.Child.ParentWorkflow, StageOccurrence: id.Child.StageOccurrence}
	}
	for branch, wait := range projection.Waits {
		request := wait.Header.Request
		if branch < 0 || branch > 128 || request.ParentRunID != id.RunID || request.Gaggle != id.Gaggle {
			return unavailable
		}
		out.Waits = append(out.Waits, ChildStageWait{RunID: request.ChildRunID, Stage: wait.Marker.Stage, Branch: branch, Action: request.Action, Since: wait.Marker.Time, Sequence: wait.Marker.Seq})
	}
	slices.SortFunc(out.Waits, func(a, b ChildStageWait) int { return a.Branch - b.Branch })
	return out
}
