package mutationreceipt

import (
	"context"
	"errors"
	"testing"
)

func TestSessionRequiresCurrentEvidenceAndPreservesLineage(t *testing.T) {
	identity, err := New("github", "https://forge.test/repos/a/b", "comment", "issues/7", "body")
	if err != nil {
		t.Fatal(err)
	}
	prior := Receipt{Schema: 1, ID: "opaque source identity", RunID: "source", Mutation: identity, Phase: "intent"}
	for _, scenario := range []string{"observed", "missing", "foreign receipt", "read error", "completed"} {
		t.Run(scenario, func(t *testing.T) {
			source := prior
			if scenario == "completed" {
				source.Phase = "completed"
			}
			recorder := &captureRecorder{}
			history := []Receipt{source}
			session := NewSession("continuation", recorder, history)
			history[0].ID = "mutated caller slice"
			err := session.Execute(t.Context(), identity, func(ctx context.Context, receipts []Receipt) (*Receipt, error) {
				if !IsFreshRead(ctx) || len(receipts) != 1 || receipts[0] != source {
					t.Fatalf("evidence request=%v", receipts)
				}
				switch scenario {
				case "missing":
					return nil, nil
				case "read error":
					return nil, errors.New("provider unavailable")
				case "foreign receipt":
					foreign := source
					foreign.ID = "foreign"
					return &foreign, nil
				default:
					return &receipts[0], nil
				}
			}, func(context.Context, Receipt) error { t.Fatal("repeated prior mutation"); return nil })
			if scenario == "observed" {
				if err != nil || len(recorder.receipts) != 1 {
					t.Fatalf("completion=%v %v", recorder.receipts, err)
				}
				completed := recorder.receipts[0]
				if completed.ID != source.ID || completed.RunID != source.RunID || completed.Phase != "completed" {
					t.Fatal(completed)
				}
			} else if scenario == "completed" {
				if err != nil || len(recorder.receipts) != 0 {
					t.Fatalf("completed reconciliation=%v %v", recorder.receipts, err)
				}
			} else if err == nil || len(recorder.receipts) != 0 {
				t.Fatalf("uncertain evidence accepted: %v %v", recorder.receipts, err)
			}
		})
	}
}
