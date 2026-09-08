package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestOwnedScratchRecoveryPrecedesDeletion(t *testing.T) {
	for _, reap := range []bool{false, true} {
		t.Run(fmt.Sprint(reap), func(t *testing.T) {
			root, runs := t.TempDir(), t.TempDir()
			writer, err := journal.Create(runs, journal.RunIdentity{RunID: "scratch-owner", Workflow: "test", WorkflowVersion: 1, Gaggle: "test"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Close() }()
			ws, err := createOwnedScratch(root, runs, "scratch-owner")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(ws.path, "mutations.jsonl")
			data := []byte("{\"receiptId\":\"scratch-receipt\",\"provider\":\"github\",\"kind\":\"pr\",\"id\":\"9\"}\n")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			cleanup := func() error {
				if reap {
					return ReapScratchWorkspacesForRuns(root, runs)
				}
				return ws.Remove(context.Background())
			}
			if err := cleanup(); !errors.Is(err, journal.ErrRecoveryBusy) {
				t.Fatalf("busy journal lost: %v", err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
				t.Fatalf("receipt lost: %q %v", got, err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(ws.scratchContainer); !os.IsNotExist(err) {
				t.Fatalf("owner container remains: %v", err)
			}
			reader, err := journal.OpenReadOnly(filepath.Join(runs, "scratch-owner"))
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil || len(events) != 2 || events[1].Runner["mutationReceiptId"] != "scratch-receipt" {
				t.Fatalf("missing durable receipt: %+v %v", events, err)
			}
		})
	}
}

func TestReapScratchWorkspacesRemovesOwnedEntries(t *testing.T) {
	root := t.TempDir()
	orphan := filepath.Join(root, scratchWorkspacePrefix+"orphan")
	unowned := filepath.Join(root, "operator-notes")
	for _, path := range []string{orphan, unowned} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := ReapScratchWorkspaces(root); err != nil {
		t.Fatalf("ReapScratchWorkspaces: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan still exists: %v", err)
	}
	if _, err := os.Stat(unowned); err != nil {
		t.Fatalf("unowned entry was removed: %v", err)
	}
}

func TestReapScratchWorkspacesAllowsMissingRoot(t *testing.T) {
	if err := ReapScratchWorkspaces(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("ReapScratchWorkspaces: %v", err)
	}
}

func TestOwnedScratchRequiresHostRunRootForCleanup(t *testing.T) {
	ws, err := createOwnedScratch(t.TempDir(), "", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Remove(context.Background()); err == nil {
		t.Fatal("resolved journal relative to process working directory")
	}
	if _, err := os.Stat(ws.path); err != nil {
		t.Fatalf("removed unrouteable workspace: %v", err)
	}
}
