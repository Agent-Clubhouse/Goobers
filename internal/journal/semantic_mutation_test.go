package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/mutationreceipt"
)

func semanticReceipt(t *testing.T, id, runID, phase, body string) mutationreceipt.Receipt {
	t.Helper()
	identity, err := mutationreceipt.New("github", "https://api.example.test/repos/acme/app", "comment", "issue/7", body)
	if err != nil {
		t.Fatal(err)
	}
	return mutationreceipt.Receipt{Schema: 1, ID: id, RunID: runID, Phase: phase, Mutation: identity}
}

func TestContinuationSemanticMutationLineage(t *testing.T) {
	root := t.TempDir()
	runIDs := []string{"0af7651916cd43dd8448eb211c80319c", "1af7651916cd43dd8448eb211c80319c", "2af7651916cd43dd8448eb211c80319c"}
	source, err := Create(root, RunIdentity{RunID: runIDs[0], Workflow: "wf", WorkflowVersion: 2, WorkflowDigest: Digest([]byte("wf")), Gaggle: "g", Trigger: Trigger{Kind: TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipts := []mutationreceipt.Receipt{semanticReceipt(t, "done", runIDs[0], "intent", "original"), semanticReceipt(t, "done", runIDs[0], "completed", "original"), semanticReceipt(t, "uncertain", runIDs[0], "intent", "different content")}
	for _, receipt := range receipts {
		if err := source.Append(WithSemanticMutation(Event{}, &receipt)); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Append(Event{Type: EventRunFinished, Status: string(PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	terminalSeq := source.Seq()
	// A late custody copy is still evidence, never an extra successful touch.
	late := semanticReceipt(t, "late", runIDs[0], "completed", "late recovered")
	event := WithSemanticMutation(Event{}, &late)
	event.Type = EventRunnerMutationRecovered
	if err := source.Append(event); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotJournalTree(t, filepath.Join(root, runIDs[0]))
	var firstHistory []mutationreceipt.Receipt
	for index := 1; index < 3; index++ {
		run, err := CreateContinuation(root, ContinuationRequest{RunID: runIDs[index], SourceRunID: runIDs[index-1], ExpectedTerminalSeq: terminalSeq, Operator: "operator", Target: "post"})
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(Event{Type: EventRunFinished, Status: string(PhaseFailed)}); err != nil {
			t.Fatal(err)
		}
		terminalSeq = run.Seq()
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		reader, err := OpenReadOnly(filepath.Join(root, runIDs[index]))
		if err != nil {
			t.Fatal(err)
		}
		identity, err := reader.Identity()
		if err != nil {
			t.Fatal(err)
		}
		if len(identity.MutationHistory) != 3 {
			t.Fatalf("lost lineage: %#v", identity.MutationHistory)
		}
		if index == 1 {
			firstHistory = identity.MutationHistory
		} else if !reflect.DeepEqual(firstHistory, identity.MutationHistory) {
			t.Fatal("ancestor evidence changed")
		}
		if identity.MutationHistory[0].Phase != "completed" || identity.MutationHistory[2].Phase != "intent" {
			t.Fatalf("incorrect outcome collapse: %#v", identity.MutationHistory)
		}
		raw, err := os.ReadFile(filepath.Join(root, runIDs[index], fileRunYAML))
		if err != nil {
			t.Fatal(err)
		}
		document, err := yaml.YAMLToJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		validator, err := validate.New()
		if err != nil {
			t.Fatal(err)
		}
		if err := validator.ValidateJSON("journal-run.schema.json", document); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(before, snapshotJournalTree(t, filepath.Join(root, runIDs[0]))) {
		t.Fatal("source journal mutated")
	}
}

func TestSemanticMutationHistoryRejectsConflictAndUnknownVersion(t *testing.T) {
	receipt := semanticReceipt(t, "same", "source", "intent", "original")
	completion := receipt
	completion.Phase = "completed"
	history, err := continuationMutationHistory([]mutationreceipt.Receipt{completion}, []Event{WithSemanticMutation(Event{}, &receipt)})
	if err != nil || len(history) != 1 || history[0].Phase != "completed" {
		t.Fatalf("late intent downgraded completion: %v %#v", err, history)
	}
	for _, change := range []func(*mutationreceipt.Receipt){func(r *mutationreceipt.Receipt) { r.Schema++ }, func(r *mutationreceipt.Receipt) { r.RunID = "other" }, func(r *mutationreceipt.Receipt) { r.Mutation.Target = "issue/8" }, func(r *mutationreceipt.Receipt) { r.Phase = "failed" }} {
		bad := receipt
		change(&bad)
		if _, err := continuationMutationHistory([]mutationreceipt.Receipt{receipt}, []Event{WithSemanticMutation(Event{}, &bad)}); err == nil {
			t.Fatalf("accepted conflicting/unsupported receipt %#v", bad)
		}
	}
	// A legacy event without semantic evidence supplies no proof, even when its
	// operation and item match a newly captured invocation.
	history, err = continuationMutationHistory(nil, []Event{{Type: EventRefTouched, Runner: map[string]any{"operation": "comment"}, ExternalRef: &ExternalRef{Provider: "github", Kind: "issue", ID: "7"}}})
	if err != nil || len(history) != 0 {
		t.Fatal("invented semantic evidence for legacy fact")
	}
	for _, phase := range []string{"intent", "completed"} {
		receipt.Phase = phase
		event := WithSemanticMutation(Event{Type: EventRefTouched, ExternalRef: &ExternalRef{ID: "7"}}, &receipt)
		raw, _ := json.Marshal(event)
		var decoded Event
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if event.IsReferenceTouch() || decoded.IsReferenceTouch() {
			t.Fatal("capture metadata counted as successful mutation")
		}
	}
}
