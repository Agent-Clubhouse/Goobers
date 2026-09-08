package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

func TestSelectIssueRecoveryUsesNewestUnexpiredTerminalMatch(t *testing.T) {
	layout := instance.NewLayout(initDemo(t))
	now := time.Now().UTC()
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
	seedRecoverySelection(t, layout, repo, "older", "7", now.Add(-2*time.Hour), now.Add(time.Hour), true)
	want := seedRecoverySelection(t, layout, repo, "newer", "7", now.Add(-time.Hour), now.Add(time.Hour), true)
	seedRecoverySelection(t, layout, repo, "active", "7", now, now.Add(time.Hour), false)
	seedRecoverySelection(t, layout, repo, "expired", "7", now.Add(-time.Minute), now, true)
	seedRecoverySelection(t, layout, repo, "other-issue", "8", now, now.Add(time.Hour), true)
	got, err := selectIssueRecovery(context.Background(), layout, repo.CanonicalKey(), "7", now)
	if err != nil || got.RecordPath != want {
		t.Fatalf("selection: %+v %v, want %s", got, err, want)
	}
	if _, err := selectIssueRecovery(context.Background(), layout, repo.CanonicalKey(), "unknown", now); err == nil {
		t.Fatal("missing issue received a snapshot")
	}
	seedRecoverySelection(t, layout, repo, "same-time", "7", now.Add(-time.Hour), now.Add(time.Hour), true)
	if _, err := selectIssueRecovery(context.Background(), layout, repo.CanonicalKey(), "7", now); err == nil {
		t.Fatal("ambiguous newest snapshot was silently selected")
	}
}

func TestRecoveryCaptureOrderIgnoresRenewalOrderAndTimezoneRepresentation(t *testing.T) {
	layout := instance.NewLayout(initDemo(t))
	now := time.Now().In(time.FixedZone("fixture", -7*60*60))
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
	path := seedRecoverySelection(t, layout, repo, "capture-order", "7", now.Add(-time.Hour), now.Add(time.Hour), true)
	first, err := recovery.ReadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.SnapshotSHA = strings.Repeat("c", 40)
	second.Ref, err = recovery.RefForSnapshot(second.RunID, second.SnapshotSHA)
	if err != nil {
		t.Fatal(err)
	}
	var events []journal.Event
	for _, record := range []recovery.Record{first, second, first} {
		event, err := recovery.RetainedEvent(record)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) < 2 {
			event.Runner["recoveryCapture"] = true
		}
		events = append(events, event)
	}
	first.RetainUntil = first.RetainUntil.Add(24 * time.Hour)
	for index, record := range []recovery.Record{first, second} {
		got, err := recoveryCaptureOrder(context.Background(), events, record)
		if err != nil || got != index {
			t.Fatalf("capture %d ordered at %d: %v", index, got, err)
		}
	}
	conflict := second
	conflict.ArchiveBytes++
	if _, err := recoveryCaptureOrder(context.Background(), events, conflict); err == nil {
		t.Fatal("different archive identity accepted as ordering evidence")
	}
	// Returning to a previously captured state is a new capture, not a
	// renewal. It must outrank the intervening implementation snapshot.
	recaptured, err := recovery.RetainedEvent(first)
	if err != nil {
		t.Fatal(err)
	}
	recaptured.Runner["recoveryCapture"] = true
	events = append(events, recaptured)
	if got, err := recoveryCaptureOrder(context.Background(), events, first); err != nil || got != 3 {
		t.Fatalf("recapture order = %d, %v", got, err)
	}
}

func seedRecoverySelection(t *testing.T, layout instance.Layout, repo providers.RepositoryRef, runID, issueID string, created, deadline time.Time, terminal bool) string {
	t.Helper()
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: created.Add(-time.Hour)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if terminal {
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	seedItemRepositoryForTest(t, layout, runID, issueID, repo)
	ref, err := recovery.RefForRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	record := recovery.Record{Version: 1, RunID: runID, RepositoryKey: repo.CanonicalKey(), Ref: ref, BaseSHA: strings.Repeat("a", 40), SnapshotSHA: strings.Repeat("b", 40), PatchDigest: "sha256:" + strings.Repeat("c", 64), ArchiveDigest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("fixture"))), ArchiveBytes: 7, CreatedAt: created, RetainUntil: deadline}
	key := sha256.Sum256([]byte(record.RepositoryKey + "\x00" + record.RunID + "\x00" + record.SnapshotSHA))
	directory := filepath.Join(layout.Root, "recovery", fmt.Sprintf("%x", key))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, recovery.BundleFileName), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, recovery.RecordFileName)
	if err := recovery.PublishRecord(path, record); err != nil {
		t.Fatal(err)
	}
	return path
}
