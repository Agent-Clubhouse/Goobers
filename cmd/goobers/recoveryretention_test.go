package main

import (
	"bytes"
	"context"
	"io"
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

func TestPrioritizeAbandonedRecoveryUsesRenewedRecord(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
	now := time.Now().UTC()
	seedRecoverySelection(t, layout, repo, "ordinary", "1", now.Add(-time.Hour), now.Add(time.Hour), true)
	abandonedPath := seedRecoverySelection(t, layout, repo, "abandoned", "2", now.Add(-time.Hour), now.Add(time.Hour), true)
	renewed, err := recovery.RenewRetention(context.Background(), abandonedPath, now.Add(2*time.Hour), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	event, err := recovery.AbandonedEvent(renewed)
	if err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(event); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := recovery.ReadInventory(context.Background(), filepath.Join(layout.Root, "recovery"), 128)
	if err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	entries, err = prioritizeAbandonedRecovery(entries, events)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].RecordPath != abandonedPath {
		t.Fatalf("abandoned recovery was not prioritized: %#v", entries)
	}
}

func TestConfiguredRetentionReapsInterruptedRecoveryRetirement(t *testing.T) {
	for _, mode := range []string{"disabled", "dry-run", "enabled-dry-run", "delete", "failure"} {
		t.Run(mode, func(t *testing.T) {
			layout := instance.NewLayout(t.TempDir())
			// A crash after removing the last file but before removing the
			// committed retirement directory is a valid reconciliation input.
			retired := filepath.Join(layout.Root, "recovery", ".retired-"+strings.Repeat("a", 64))
			if err := os.MkdirAll(retired, 0o700); err != nil {
				t.Fatal(err)
			}
			// FirstEnable: immediate keeps these modes testing enforcement rather
			// than #4253's first-enable grace window, which would otherwise make
			// every mode a dry run on this fixture's first pass.
			cfg := instance.RetentionConfig{
				Enabled:     boolPtr(mode == "delete" || mode == "failure" || mode == "enabled-dry-run"),
				DryRun:      mode == "dry-run" || mode == "enabled-dry-run",
				FirstEnable: "immediate",
			}
			if mode == "failure" {
				if err := os.WriteFile(filepath.Join(retired, "unknown"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			setup := &schedulerSetup{Config: &instance.Config{Retention: cfg}}
			var stdout, stderr bytes.Buffer
			err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr)
			if (err != nil) != (mode == "failure") {
				t.Fatalf("configured sweep: %v", err)
			}
			_, statErr := os.Stat(retired)
			if mode == "delete" {
				if !os.IsNotExist(statErr) || !strings.Contains(stdout.String(), "retention deleted kind=retired-recovery") {
					t.Fatalf("configured sweep did not reap: %v %s", statErr, stdout.String())
				}
			} else if statErr != nil {
				t.Fatalf("non-deleting sweep lost retirement: %v", statErr)
			}
			if cfg.DryRun && !strings.Contains(stdout.String(), "retention candidate kind=retired-recovery") {
				t.Fatalf("dry-run omitted candidate: %s", stdout.String())
			}
			if mode == "disabled" && (stdout.Len() != 0 || stderr.Len() != 0) {
				t.Fatal("disabled sweep produced output")
			}
			if mode == "failure" {
				if stderr.Len() == 0 {
					t.Fatal("failed deletion omitted warning")
				}
				if err := pruneConfiguredRetention(context.Background(), layout, setup, io.Discard, io.Discard); err == nil {
					t.Fatal("background sweep swallowed deletion failure")
				}
			}
		})
	}
}
