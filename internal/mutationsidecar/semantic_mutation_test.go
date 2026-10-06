package mutationsidecar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/mutationreceipt"
)

func TestSemanticMutationRecoveryPreservesUnknownAndCompletion(t *testing.T) {
	identity, err := mutationreceipt.New("github", "https://api.example.test/repos/a/b", "comment", "issue/7", "body")
	if err != nil {
		t.Fatal(err)
	}
	intent := mutationreceipt.Receipt{Schema: 1, ID: "invocation", RunID: "source", Mutation: identity, Phase: "intent"}
	completion := intent
	completion.Phase = "completed"
	facts := []Fact{{ReceiptID: "intent-custody", Provider: "github", Kind: "issue", ID: "7", Operation: "comment", SemanticMutation: &intent}, {ReceiptID: "completed-custody", Provider: "github", Kind: "issue", ID: "7", Operation: "comment", SemanticMutation: &completion}}
	pending, err := missingRecoveryEvents(facts, nil, "worktree")
	if err != nil || len(pending) != 2 {
		t.Fatalf("recover: %v %#v", err, pending)
	}
	for _, event := range pending {
		if event.IsReferenceTouch() {
			t.Fatal("capture receipt counted as external touch")
		}
	}
	raw, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	var recorded []journal.Event
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	replay, err := missingRecoveryEvents(facts, recorded, "worktree")
	if err != nil || len(replay) != 0 {
		t.Fatalf("custody replay changed receipts: %v %#v", err, replay)
	}
	// Intent and completion must have separate custody identities. Content is
	// part of their fingerprints even though neither is an external touch.
	changed := completion
	changed.Mutation.Target = "issue/8"
	conflict := facts[1]
	conflict.SemanticMutation = &changed
	if _, err := missingRecoveryEvents([]Fact{conflict}, recorded, "worktree"); err == nil {
		t.Fatal("semantic payload excluded from custody fingerprint")
	}
	normal := []journal.Event{recoveryEvent(facts[0]), recoveryEvent(facts[1])}
	if replay, err := missingRecoveryEvents(facts, normal, "worktree"); err != nil || len(replay) != 0 {
		t.Fatalf("normal projection failed dedup: %v %#v", err, replay)
	}
}

func TestSemanticMutationHandoffRejectsUnknownReceiptSchema(t *testing.T) {
	identity, _ := mutationreceipt.New("github", "https://api.example.test/repos/a/b", "comment", "issue/7", "body")
	receipt := mutationreceipt.Receipt{Schema: 2, ID: "invocation", RunID: "source", Mutation: identity, Phase: "completed"}
	raw, err := json.Marshal(Fact{Provider: "github", Kind: "issue", ID: "7", SemanticMutation: &receipt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRecoveryFacts(raw); err == nil {
		t.Fatal("unknown semantic receipt version accepted")
	}
}

func TestSemanticMutationPublicationAcknowledgesDurableMetadata(t *testing.T) {
	runs, workspace := t.TempDir(), t.TempDir()
	const owner = "0af7651916cd43dd8448eb211c80319c"
	run, err := journal.Create(runs, journal.RunIdentity{RunID: owner, Gaggle: "g", Workflow: "wf", WorkflowVersion: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	identity, _ := mutationreceipt.New("github", "https://api.example.test/repos/a/b", "comment", "issue/7", "body")
	receipt := mutationreceipt.Receipt{Schema: 1, ID: "invocation", RunID: owner, Mutation: identity, Phase: "intent"}
	fact := Fact{ReceiptID: "intent-custody", Provider: "github", Kind: "issue", ID: "7", Operation: "comment", SemanticMutation: &receipt}
	raw, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "mutations.jsonl"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return runs, gaggle == "g" })
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := PublishBeforeCleanup(t.Context(), workspace, "stage", owner, "g", journal.NewPatternScrubber(), lostReceiptAck{writer}); err == nil {
		t.Fatal("lost intent acknowledgment was accepted")
	}
	if err := PublishBeforeCleanup(t.Context(), workspace, "stage", owner, "g", journal.NewPatternScrubber(), writer); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runs, owner))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Runner["semanticMutation"] == nil {
			continue
		}
		count++
		if event.IsReferenceTouch() {
			t.Fatal("durable intent became an external touch")
		}
	}
	if count != 1 {
		t.Fatalf("replayed intent produced %d durable metadata records", count)
	}
	if err := RecoverBeforeCleanup(t.Context(), workspace, "stage", owner, filepath.Join(runs, owner)); err != nil {
		t.Fatal(err)
	}
}
