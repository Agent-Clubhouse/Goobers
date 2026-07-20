package localscheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTriggerEvaluationsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	alpha := WorkflowIdentity{Gaggle: "alpha", Workflow: "deploy"}
	beta := WorkflowIdentity{Gaggle: "beta", Workflow: "deploy"}
	evaluations := map[WorkflowIdentity]time.Time{
		alpha: time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC),
		beta:  time.Date(2026, time.July, 20, 9, 30, 0, 0, time.UTC),
	}
	if err := writeTriggerEvaluations(dir, evaluations); err != nil {
		t.Fatal(err)
	}

	got, err := ReadTriggerEvaluations(dir)
	if err != nil {
		t.Fatal(err)
	}
	for identity, want := range evaluations {
		if !got[identity].Equal(want) {
			t.Fatalf("%+v LastEval = %s, want %s", identity, got[identity], want)
		}
	}
}

func TestReadTriggerEvaluationsMissingAndMalformed(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadTriggerEvaluations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("missing state = %+v, want empty", got)
	}

	if err := os.WriteFile(filepath.Join(dir, triggerEvaluationsFileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTriggerEvaluations(dir); err == nil {
		t.Fatal("malformed trigger evaluation state succeeded")
	}
}
