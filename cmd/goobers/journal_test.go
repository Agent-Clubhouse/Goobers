package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// cliLeak is a plaintext secret shaped so the default pattern net does NOT catch
// it — it reaches disk and must be remediated by `goobers journal redact`.
const cliLeak = "PLAINTEXT-CLI-LEAK-do-not-store-7c1a"

// writeRunWithLeakedArtifact creates a run under root's instance layout whose
// stored artifact holds cliLeak at rest, returning the run id and the artifact's
// journal-relative path.
func writeRunWithLeakedArtifact(t *testing.T, root string) (runID, blobPath string) {
	t.Helper()
	l := instance.NewLayout(root)
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID:           "redact-fixture-1",
		Workflow:        "default-implement",
		WorkflowVersion: 1,
		Gaggle:          "example",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create fixture run: %v", err)
	}
	ref, err := jr.RecordArtifact("config.env", []byte("TOKEN="+cliLeak+"\n"))
	if err != nil {
		t.Fatalf("record leaked artifact: %v", err)
	}
	_ = jr.Close()
	return "redact-fixture-1", ref.Path
}

// runDirContainsLeak walks a run dir and reports whether any file holds needle.
func runDirContainsLeak(t *testing.T, dir string, needle []byte) bool {
	t.Helper()
	var found bool
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, needle) {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return found
}

// TestJournalRedactRemovesLeakedSecret drives the full command: a leaked secret
// at rest in a run's artifact is removed, the redaction is traced, and the raw
// value survives in no file.
func TestJournalRedactRemovesLeakedSecret(t *testing.T) {
	root := initDemo(t)
	runID, blobPath := writeRunWithLeakedArtifact(t, root)
	runDir := filepath.Join(instance.NewLayout(root).RunsDir(), runID)

	if !runDirContainsLeak(t, runDir, []byte(cliLeak)) {
		t.Fatal("precondition: leak should be at rest before redaction")
	}

	// The secret is supplied out-of-band (never a flag) — here via --secret-file.
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte(cliLeak), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "journal", "redact",
		"--run", runID, "--path", blobPath, "--reason", "token pasted into the issue body",
		"--secret-file", secretFile, root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "redacted "+blobPath) || !strings.Contains(stdout, "new digest:") {
		t.Fatalf("unexpected stdout: %q", stdout)
	}

	// The leak is gone from every file at rest.
	if runDirContainsLeak(t, runDir, []byte(cliLeak)) {
		t.Fatal("leak survived `journal redact`")
	}

	// A redaction event was appended so even the exception leaves a trace (§4).
	rd, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Type != journal.EventRedaction || last.Redaction == nil {
		t.Fatalf("expected a trailing redaction event, got %+v", last)
	}
	if last.Redaction.Target != blobPath || last.Redaction.Reason != "token pasted into the issue body" {
		t.Fatalf("redaction event details wrong: %+v", last.Redaction)
	}
}

// TestJournalRedactNothingRedactedOnWrongSecret proves the command fails loudly
// (exit 1) rather than silently rewriting an identical blob when the supplied
// value is not actually present.
func TestJournalRedactNothingRedactedOnWrongSecret(t *testing.T) {
	root := initDemo(t)
	runID, blobPath := writeRunWithLeakedArtifact(t, root)

	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("this-value-is-not-in-the-blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runArgs(t, "journal", "redact",
		"--run", runID, "--path", blobPath, "--reason", "x", "--secret-file", secretFile, root)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "nothing redacted") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestJournalRedactRequiresFlags checks the required-flag guard (exit 2) and,
// importantly, that it fires before any secret is read.
func TestJournalRedactRequiresFlags(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "journal", "redact", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "required") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestJournalUnknownSubcommand(t *testing.T) {
	code, _, stderr := runArgs(t, "journal", "bogus")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown subcommand") {
		t.Fatalf("stderr = %q", stderr)
	}
}
