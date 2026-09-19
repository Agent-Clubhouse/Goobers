package mutationsidecar

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestRecoveryRecognizesDerivedLandingURLWithoutWeakeningReceiptIdentity(t *testing.T) {
	fact := Fact{ReceiptID: "intent-receipt", Provider: "github", Kind: "pr", ID: "25", Operation: "merge-intent",
		LandingIntent: &providers.LandingIntent{ID: "intent", Operation: "merge", RepositoryAPIURL: "https://api.github.com/repos/acme/app", PullID: "25", ExpectedHeadSHA: "reviewed-head"}}
	// The durable pre-mutation sidecar has no URL. Normal projection enriches it.
	normal := recoveryEvent(fact)
	normal.ExternalRef.URL = "https://github.com/acme/app/pull/25"
	legacy, err := missingRecoveryEvents([]Fact{fact}, nil, "stage")
	if err != nil {
		t.Fatal(err)
	}
	for _, events := range [][]journal.Event{{normal}, legacy, append(legacy, normal)} {
		// Round-trip the journal: receipt fields become maps, not producer structs.
		data, err := json.Marshal(events)
		if err != nil {
			t.Fatal(err)
		}
		var recorded []journal.Event
		if err := json.Unmarshal(data, &recorded); err != nil {
			t.Fatal(err)
		}
		if pending, err := missingRecoveryEvents([]Fact{fact}, recorded, "stage"); err != nil || len(pending) != 0 {
			t.Fatalf("derived URL prevented durable handoff: pending=%v err=%v", pending, err)
		}
	}
	for _, change := range []func(*journal.Event){
		func(e *journal.Event) { e.ExternalRef.URL = "https://github.com/other/repo/pull/25" },
		func(e *journal.Event) { e.Runner["operation"] = "delete" },
		func(e *journal.Event) { e.ExternalRef.ID = "26" },
		func(e *journal.Event) {
			intent := *fact.LandingIntent
			intent.ExpectedHeadSHA = "different-head"
			e.Runner["landingIntent"] = &intent
		},
	} {
		conflict := recoveryEvent(fact)
		change(&conflict)
		if _, err := missingRecoveryEvents([]Fact{fact}, []journal.Event{conflict}, "stage"); err == nil {
			t.Fatal("accepted a genuinely different receipt")
		}
	}
}

func TestRecoveryRecognizesNormalProjectionWhileWriterIsHeld(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	writer, err := journal.Create(root, journal.RunIdentity{RunID: "owner", Workflow: "implementation", WorkflowVersion: 1, Gaggle: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	fact := Fact{ReceiptID: "already-committed", Provider: "github", Kind: "pr", ID: "9", Operation: "merge"}
	data, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "mutations.jsonl"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	// Ordinary projection has no recovery-specific metadata.
	if err := writer.Append(recoveryEvent(fact)); err != nil {
		t.Fatal(err)
	}
	if err := RecoverBeforeCleanup(context.Background(), workspace, "owner-stage", "owner", filepath.Join(root, "owner")); err != nil {
		t.Fatalf("cleanup tried to reacquire the live writer: %v", err)
	}
}

func TestRecoveredReceiptsDoNotDuplicateWorkflowConformance(t *testing.T) {
	for _, outcome := range []string{"", "failure", "conflict"} {
		fact := Fact{ReceiptID: "receipt", Provider: "github", Kind: "pr", ID: "9", Operation: "merge", Outcome: outcome}
		normal := recoveryEvent(fact)
		recovered, err := missingRecoveryEvents([]Fact{fact}, nil, "worker-stage")
		if err != nil || len(recovered) != 1 {
			t.Fatalf("prepare recovered receipt: %v %v", recovered, err)
		}
		if recovered[0].Type != journal.EventRunnerMutationRecovered || recovered[0].IsConformanceNormative() {
			t.Fatal("custody copy appeared as a second workflow outcome")
		}
		if got := journal.ConformanceView(append(recovered, normal)); len(got) != 1 || got[0].Type != normal.Type {
			t.Fatalf("normal projection changed conformance: %+v", got)
		}
		if pending, err := missingRecoveryEvents([]Fact{fact}, recovered, "worker-stage"); err != nil || len(pending) != 0 {
			t.Fatalf("custody receipt was not recognized: %v %v", pending, err)
		}
		normalFingerprint, err := mutationFingerprint(normal)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := mutationFingerprint(recovered[0]); err != nil || got != normalFingerprint {
			t.Fatalf("custody changed receipt identity: %s %v", got, err)
		}
	}
}

func TestRecoveryRecomputesJournalReceiptFingerprint(t *testing.T) {
	fact := Fact{ReceiptID: "receipt", Provider: "github", Kind: "pr", ID: "9", Operation: "merge"}
	expected := recoveryEvent(fact)
	fingerprint, err := mutationFingerprint(expected)
	if err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []any{fingerprint, 17, map[string]any{"invalid": true}} {
		altered := recoveryEvent(fact)
		altered.Runner["operation"] = "comment"
		altered.Runner["mutationRecoveryFingerprint"] = stamp
		if _, err := missingRecoveryEvents([]Fact{fact}, []journal.Event{altered}, "worktree"); err == nil {
			t.Fatal("a stamped fingerprint hid different receipt contents")
		}
	}
	expected.Runner["mutationRecoveryFingerprint"] = fingerprint
	if missing, err := missingRecoveryEvents([]Fact{fact}, []journal.Event{expected}, "worktree"); err != nil || len(missing) != 0 {
		t.Fatalf("matching recovered receipt was not acknowledged: %v %v", missing, err)
	}
}

func TestRecoveryDoesNotBorrowAnIdenticalReceiptFromAnotherAttempt(t *testing.T) {
	fact := Fact{ReceiptID: "current", Provider: "github", Kind: "pr", ID: "9", Operation: "merge"}
	old := fact
	old.ReceiptID = "previous"
	pending, err := missingRecoveryEvents([]Fact{fact}, []journal.Event{recoveryEvent(old)}, "owner-stage")
	if err != nil || len(pending) != 1 {
		t.Fatalf("borrowed prior attempt receipt: pending=%+v err=%v", pending, err)
	}
	pending, err = missingRecoveryEvents([]Fact{fact, fact}, pending, "owner-stage")
	if err != nil || len(pending) != 0 {
		t.Fatalf("same durable receipt not idempotent: pending=%+v err=%v", pending, err)
	}
	conflict := fact
	conflict.ID = "another-pr"
	if _, err := missingRecoveryEvents([]Fact{conflict}, []journal.Event{recoveryEvent(fact)}, "owner-stage"); err == nil {
		t.Fatal("accepted reused identity with different evidence")
	}
	legacy := fact
	legacy.ReceiptID = ""
	if _, err := missingRecoveryEvents([]Fact{legacy}, []journal.Event{recoveryEvent(legacy)}, "owner-stage"); err == nil {
		t.Fatal("silently discarded ambiguous legacy receipt")
	}
}

func TestRecoveryRejectsUnsafeHandoffWithoutAppendingPrefix(t *testing.T) {
	const first = `{"receiptId":"same","provider":"github","kind":"pr","id":"9"}`
	for _, tc := range []struct {
		name, data, owner string
	}{
		{"malformed-tail", first + "\n{broken\n", "owner"},
		{"conflicting-identity", first + "\n" + `{"receiptId":"same","provider":"github","kind":"pr","id":"10"}` + "\n", "owner"},
		{"escaping-owner", first + "\n", "../owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, workspace := t.TempDir(), t.TempDir()
			writer, err := journal.Create(root, journal.RunIdentity{RunID: "owner", Workflow: "implementation", WorkflowVersion: 1, Gaggle: "test"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(workspace, "mutations.jsonl")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "owner")
			if err := RecoverBeforeCleanup(context.Background(), workspace, "owner-stage", tc.owner, dir); err == nil {
				t.Fatal("unsafe handoff accepted")
			}
			reader, err := journal.OpenReadOnly(dir)
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil || len(events) != 1 {
				t.Fatalf("unsafe handoff appended a prefix: events=%+v err=%v", events, err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != tc.data {
				t.Fatalf("handoff evidence changed: %q %v", got, err)
			}
		})
	}
}

func TestRecoveryImportsMissingReceiptsOnceAndRefusesBusyOwner(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	writer, err := journal.Create(root, journal.RunIdentity{RunID: "owner", Workflow: "implementation", WorkflowVersion: 1, Gaggle: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	dir := filepath.Join(root, "owner")
	data := []byte("{\"receiptId\":\"unique-receipt\",\"provider\":\"github\",\"kind\":\"pull-request\",\"id\":\"9\",\"operation\":\"claim\",\"runId\":\"other-claim-owner\"}\n")
	if err := os.WriteFile(filepath.Join(workspace, "mutations.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverBeforeCleanup(context.Background(), workspace, "owner-stage", "owner", dir); !errors.Is(err, journal.ErrRecoveryBusy) {
		t.Fatalf("busy owner not preserved: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := RecoverBeforeCleanup(context.Background(), workspace, "owner-stage", "owner", dir); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == journal.EventRunnerMutationRecovered {
			count++
			if event.Runner["claimRunId"] != "other-claim-owner" || event.Runner["recoveredFromWorktree"] != "owner-stage" {
				t.Fatalf("lost ownership distinction: %+v", event)
			}
		}
	}
	if count != 1 {
		t.Fatalf("imported %d receipts, want one", count)
	}
	if err := RecoverBeforeCleanup(context.Background(), workspace, "owner-stage", "wrong-owner", dir); err == nil {
		t.Fatal("accepted wrong journal owner")
	}
}
