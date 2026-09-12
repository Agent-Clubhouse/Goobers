package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/diagnostics"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// seedDiagnosticsRun writes a run journal shaped like the incident #2968 was
// filed about: a selector that found nothing, with its reason recorded as a
// scalar stage output rather than only in stdout.
func seedDiagnosticsRun(t *testing.T, root, runID, reason string, secret string) {
	t.Helper()
	layout := instance.NewLayout(root)
	if err := layout.EnsureGaggleRuntime("example"); err != nil {
		t.Fatalf("ensure gaggle runtime: %v", err)
	}
	// WithScrubber(journal.Chain()) is an explicit opt-out of the journal's own
	// write-time redaction, so the secret below is genuinely AT REST in the
	// fixture. That is what makes the assertion load-bearing: with the
	// journal's net in play the collector's own net would never be exercised,
	// and the test would pass whether or not it existed. It is also the real
	// case — a journal written by an older binary, or one whose registry never
	// saw that particular value.
	run, err := journal.Create(layout.ForGaggle("example").RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "merge-review",
		WorkflowVersion: 1,
		WorkflowDigest:  "sha256:deadbeef",
		Gaggle:          "example",
		Trigger:         journal.Trigger{Kind: journal.TriggerSchedule},
		StartedAt:       time.Date(2026, 9, 7, 6, 0, 0, 0, time.UTC),
	}, nil, journal.WithScrubber(journal.Chain()))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	defer func() { _ = run.Close() }()

	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "pr-select", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "pr-select", Attempt: 1, Status: "success",
		Outputs: map[string]any{"noWork": true, "noWorkReason": reason},
	}); err != nil {
		t.Fatal(err)
	}
	if secret != "" {
		if err := run.Append(journal.Event{
			Type: journal.EventError, Stage: "pr-select", Attempt: 1,
			Error: &journal.ErrorDetail{Code: "github_forbidden", Message: "auth failed using " + secret},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func readDiagnosticsArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { _ = file.Close() }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("open gzip: %v", err)
	}
	entries := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = body
	}
	return entries
}

// #2968's headline acceptance: the bundle explains a no-work cycle on its own,
// from a released binary and an instance directory, with no Goobers checkout
// anywhere in the picture.
func TestDiagnosticsBundleExplainsNoWorkFromTheInstanceAlone(t *testing.T) {
	root := initDemo(t)
	const reason = "queue parked: 7 of 7 matching pull request(s) excluded — escalated, human action required 7"
	seedDiagnosticsRun(t, root, "run-no-work", reason, "")

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "diagnostics", "bundle", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	archive := filepath.Join(workDir, "goobers-diagnostics-"+filepath.Base(root)+".tar.gz")
	entries := readDiagnosticsArchive(t, archive)

	var bundle diagnostics.Bundle
	if err := json.Unmarshal(entries[diagnostics.FileJSON], &bundle); err != nil {
		t.Fatalf("decode %s: %v", diagnostics.FileJSON, err)
	}
	if len(bundle.Runs) != 1 || bundle.Runs[0].RunID != "run-no-work" {
		t.Fatalf("runs = %+v", bundle.Runs)
	}
	// The decisive fact: the exclusion tally reached the bundle. Before this,
	// it existed only in the stage's stdout, which a redacted bundle cannot
	// carry — so the question "why no-work" was unanswerable from a bundle.
	found := false
	for _, decision := range bundle.Runs[0].Decisions {
		if strings.Contains(decision.Reason, "queue parked") {
			found = true
		}
	}
	if !found {
		t.Fatalf("run decisions = %+v, want the selector's own exclusion tally", bundle.Runs[0].Decisions)
	}
	// The generation facts an operator otherwise reads from a checkout.
	if bundle.Binary.Version == "" || bundle.Contract.JournalSchemaVersion == 0 ||
		len(bundle.Contract.StageCommands) == 0 || bundle.Instance.ConfigDigest == "" {
		t.Fatalf("bundle = %+v", bundle)
	}
	if bundle.Runs[0].WorkflowDigest != "sha256:deadbeef" {
		t.Errorf("workflowDigest = %q, want the run's own pinned definition", bundle.Runs[0].WorkflowDigest)
	}
	summary := string(entries[diagnostics.FileSummary])
	for _, want := range []string{"# Goobers diagnostics", "queue parked", "## Stage contract", "pr-select"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary is missing %q:\n%s", want, summary)
		}
	}
}

// The bundle is handed to a support channel, so it must be safe by
// construction: no credential value can reach it, from the config or from a
// journal that recorded one in an error message.
//
// The fixture journal opts OUT of write-time redaction, so the collector's own
// net is the only thing between the secret and the bundle. Without that, the
// journal would have scrubbed the value on the way in and this test would pass
// whether or not the collector scrubbed anything.
func TestDiagnosticsBundleCarriesNoCredentialValue(t *testing.T) {
	root := initDemo(t)
	const leaked = "ghp_0123456789abcdefghijklmnopqrstuvwxyzA"
	t.Setenv("GOOBERS_GITHUB_TOKEN", leaked)
	seedDiagnosticsRun(t, root, "run-leaky", "no eligible PR", leaked)

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, _, stderr := runArgs(t, "diagnostics", "bundle", "--json", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	_, stdout, _ := runArgs(t, "diagnostics", "bundle", "--json", root)
	if strings.Contains(stdout, leaked) {
		t.Fatalf("bundle carries the token:\n%s", stdout)
	}
	// Presence must still be reported — a bundle that cannot say whether a
	// credential was configured is not diagnostic.
	var bundle diagnostics.Bundle
	if err := json.Unmarshal([]byte(stdout), &bundle); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if len(bundle.Credentials) == 0 {
		t.Fatal("bundle records no credentials at all")
	}
	for _, cred := range bundle.Credentials {
		if strings.Contains(cred.SourceName, leaked) {
			t.Fatalf("credential source carries a value: %+v", cred)
		}
	}
}

// Two collections of the same instance state must be byte-identical apart from
// the collection timestamp, which is what makes "reproduce it over there" a
// claim rather than a hope. This runs on every platform CI covers.
// TestDiagnosticsBundleDistinguishesForegroundRunFromDaemonCrash is #4833's
// end-to-end regression: a foreground `goobers run` (acquireInstanceLock with
// a nil identity, exactly like cmd/goobers/run.go:197) leaves its up.lock
// file behind on release without a daemon ever starting — the demo
// onboarding path (`init --demo` then `run demo`). `diagnostics bundle` must
// not read that as a crashed daemon.
func TestDiagnosticsBundleDistinguishesForegroundRunFromDaemonCrash(t *testing.T) {
	root := initDemo(t)
	workDir := t.TempDir()
	t.Chdir(workDir)

	lockPath := filepath.Join(layoutFor(root).SchedulerDir(), "up.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := acquireInstanceLock(lockPath)
	if err != nil {
		t.Fatalf("acquire manual lock: %v", err)
	}
	release()
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file did not persist after release, want it to (matching a real foreground run): %v", err)
	}

	out := filepath.Join(workDir, "bundle.tar.gz")
	if code, _, stderr := runArgs(t, "diagnostics", "bundle", "--output", out, root); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	entries := readDiagnosticsArchive(t, out)
	var doc struct {
		Daemon struct {
			Running        bool   `json:"running"`
			LockPresent    bool   `json:"lockPresent"`
			LockHolderKind string `json:"lockHolderKind"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(entries[diagnostics.FileJSON], &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Daemon.Running || !doc.Daemon.LockPresent || doc.Daemon.LockHolderKind != "manual" {
		t.Fatalf("daemon info = %+v, want running=false lockPresent=true lockHolderKind=manual", doc.Daemon)
	}
	summary := string(entries[diagnostics.FileSummary])
	if strings.Contains(summary, "previous daemon exited") {
		t.Fatalf("summary falsely claims a daemon crashed:\n%s", summary)
	}
	if !strings.Contains(summary, "no daemon crash indicated") {
		t.Fatalf("summary does not state the manual-lock case:\n%s", summary)
	}
}

func TestDiagnosticsBundleIsReproducibleOnThisPlatform(t *testing.T) {
	root := initDemo(t)
	seedDiagnosticsRun(t, root, "run-stable", "no eligible PR", "")
	workDir := t.TempDir()
	t.Chdir(workDir)

	documents := make([]map[string]any, 0, 2)
	for i := range 2 {
		out := filepath.Join(workDir, "bundle-"+string(rune('a'+i))+".tar.gz")
		if code, _, stderr := runArgs(t, "diagnostics", "bundle", "--output", out, root); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr)
		}
		entries := readDiagnosticsArchive(t, out)
		var doc map[string]any
		if err := json.Unmarshal(entries[diagnostics.FileJSON], &doc); err != nil {
			t.Fatal(err)
		}
		// generatedAt is the ONLY field allowed to differ between two
		// collections of the same state.
		delete(doc, "generatedAt")
		documents = append(documents, doc)
	}
	first, _ := json.Marshal(documents[0])
	second, _ := json.Marshal(documents[1])
	if !bytes.Equal(first, second) {
		t.Fatalf("two bundles of the same state differ:\n%s\n%s", first, second)
	}
}

// Scope flags narrow the bundle rather than filtering it after the fact, and a
// named run that does not exist is refused rather than answered with an empty
// bundle the operator would read as evidence.
func TestDiagnosticsBundleScopes(t *testing.T) {
	root := initDemo(t)
	seedDiagnosticsRun(t, root, "run-one", "no eligible PR", "")
	seedDiagnosticsRun(t, root, "run-two", "no eligible PR", "")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "diagnostics", "bundle", "--run", "run-two", "--json", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var scoped diagnostics.Bundle
	if err := json.Unmarshal([]byte(stdout), &scoped); err != nil {
		t.Fatal(err)
	}
	if len(scoped.Runs) != 1 || scoped.Runs[0].RunID != "run-two" {
		t.Fatalf("runs = %+v, want only run-two", scoped.Runs)
	}

	if code, _, stderr := runArgs(t, "diagnostics", "bundle", "--run", "run-absent", "--json", root); code != 1 {
		t.Fatalf("absent run: code = %d, stderr = %q, want 1", code, stderr)
	}
	if code, _, _ := runArgs(t, "diagnostics", "bundle", "--run", "x", "--pr", "5", root); code != 2 {
		t.Fatal("--run and --pr select different scopes and must not be combined")
	}
	if code, _, _ := runArgs(t, "diagnostics", "bundle", "--max-runs", "0", root); code != 2 {
		t.Fatal("--max-runs 0 must be a usage error")
	}
}
