package mutationsidecar

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

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
		if event.Type == journal.EventRefTouched {
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
