package gate

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type activityJournal struct{ event journal.Event }

func (j *activityJournal) Append(event journal.Event) error { j.event = event; return nil }
func (*activityJournal) RecordArtifact(string, []byte) (journal.Ref, error) {
	return journal.Ref{}, nil
}

func TestActivityGateStartPinsReviewer(t *testing.T) {
	var recorder activityJournal
	g := apiv1.Gate{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "pinned-reviewer"}}
	if err := recordStart(&recorder, g, 2); err != nil {
		t.Fatal(err)
	}
	if recorder.event.Runner["goober"] != "pinned-reviewer" || recorder.event.Runner["repassAttempt"] != 2 {
		t.Fatal(recorder.event)
	}
	g.Evaluator = apiv1.EvaluatorAutomated
	if err := recordStart(&recorder, g, 1); err != nil {
		t.Fatal(err)
	}
	if _, exists := recorder.event.Runner["goober"]; exists {
		t.Fatal("automated gate gained an owner")
	}
}
