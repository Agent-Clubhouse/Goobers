package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestHistoricalRunResultEncodingWithoutChild(t *testing.T) {
	// These are persisted history payloads: an absent extension must neither add
	// null nor change old field order/values on decode and re-encode.
	for _, payload := range []string{
		`{"status":"completed","steps":1}`,
		`{"status":"failed","finalState":"work","outputs":{"work":{"status":"failed"}},"steps":2,"failureCode":"failed","failureMessage":"cause"}`,
	} {
		var result RunResult
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(result)
		if err != nil || string(encoded) != payload {
			t.Fatalf("historical result changed: %s, %v", encoded, err)
		}
	}
}

func TestChildResultReturnOmitsOutputsAndBoundsWholeEnvelope(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	child := triggerqueue.ChildRecord{Identity: triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "web", ParentRunID: "parent"}, StageOccurrence: "occurrence", InvocationKey: "child-1"}, AcceptanceID: "accepted", RunID: "child", ProposalDigest: journal.Digest([]byte("source")), AcceptedAt: at}
	terminal := childresult.TerminalReturn{Version: 1, Identity: child.Identity, AcceptanceID: child.AcceptanceID, RunID: child.RunID, SourceDigest: child.ProposalDigest, Input: childresult.TerminalResultInput{State: triggerqueue.ChildCompleted, FinishedAt: at.Add(time.Minute), Summary: "done", References: []string{"artifact:report"}}, ResultRef: journal.Digest([]byte("result"))}
	original := RunResult{Status: StatusCompleted, Steps: 1, Outputs: map[string]apiv1.ResultEnvelope{"work": {}}}
	result, err := original.WithChildResult(child, terminal)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outputs != nil || original.Outputs == nil || result.ChildResult == nil {
		t.Fatal("result did not isolate bounded child return")
	}
	terminal.Input.References[0] = "tampered"
	if result.ChildResult.Input.References[0] != "artifact:report" {
		t.Fatal("caller can mutate returned references")
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), `"outputs"`) {
		t.Fatalf("unexpected wire result %s: %v", encoded, err)
	}
	original.FailureMessage = strings.Repeat("x", childresult.MaxTerminalReturnBytes)
	if _, err := original.WithChildResult(child, terminal); !errors.Is(err, childresult.ErrInvalidTerminalReturn) {
		t.Fatalf("oversize outer result: %v", err)
	}
	terminal.RunID = "another-epoch"
	if _, err := original.WithChildResult(child, terminal); !errors.Is(err, childresult.ErrInvalidTerminalReturn) {
		t.Fatalf("wrong execution: %v", err)
	}
}
