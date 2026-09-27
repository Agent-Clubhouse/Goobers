package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/recovery"
)

// seedIncompleteRecoveryReservation writes the debris a crashed publish leaves
// on a production instance: a reservation directory holding only
// `.publish.lock` and `snapshot.bundle.lock`, with no record.json (#5177).
// age backdates it, so nothing here depends on elapsed wall-clock time.
func seedIncompleteRecoveryReservation(t *testing.T, root string, index int, age time.Duration) string {
	t.Helper()
	directory := filepath.Join(root, fmt.Sprintf("%064x", index))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	for _, file := range []string{".publish.lock", "snapshot.bundle.lock"} {
		path := filepath.Join(directory, file)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(directory, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestConfiguredRetentionReconcilesIncompleteRecoveryReservations(t *testing.T) {
	for _, mode := range []string{"disabled", "dry-run", "delete"} {
		t.Run(mode, func(t *testing.T) {
			layout := instance.NewLayout(t.TempDir())
			root := filepath.Join(layout.Root, "recovery")
			stale := seedIncompleteRecoveryReservation(t, root, 1, 4*recovery.IncompleteReservationGrace)
			fresh := seedIncompleteRecoveryReservation(t, root, 2, 0)
			// FirstEnable: immediate keeps these modes testing enforcement
			// rather than #4253's first-enable grace window.
			cfg := instance.RetentionConfig{
				Enabled:     boolPtr(mode == "delete"),
				DryRun:      mode == "dry-run",
				FirstEnable: "immediate",
			}
			setup := &schedulerSetup{Config: &instance.Config{Retention: cfg}}
			var stdout, stderr bytes.Buffer
			// The pass's own error is deliberately not asserted here: the
			// expiry sweep still reads the inventory strictly and fails on
			// this fixture, which is the remaining half of #5177 AC3 and
			// belongs to its own change. Reconciliation is joined into the
			// same pass, so it runs either way — that is what this pins.
			_ = pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr)
			if strings.Contains(stderr.String(), "incomplete recovery reservation cleanup failed") {
				t.Fatalf("reconciliation warned: %s", stderr.String())
			}
			// An incomplete reservation that may still be an in-flight publish
			// is never reclaimed, in any mode, and keeps its capacity.
			if _, err := os.Stat(fresh); err != nil {
				t.Fatalf("in-flight reservation was reclaimed: %v", err)
			}
			_, staleErr := os.Stat(stale)
			if mode == "delete" {
				if !errors.Is(staleErr, os.ErrNotExist) {
					t.Fatalf("stale debris survived the sweep: %v", staleErr)
				}
				if !strings.Contains(stdout.String(), "retention deleted kind=incomplete-recovery-reservation") {
					t.Fatalf("reclamation was not reported: %s", stdout.String())
				}
				return
			}
			if staleErr != nil {
				t.Fatalf("non-deleting sweep reclaimed debris: %v", staleErr)
			}
			if mode == "dry-run" && !strings.Contains(stdout.String(), "retention candidate kind=incomplete-recovery-reservation") {
				t.Fatalf("dry run omitted the candidate: %s", stdout.String())
			}
			if mode == "disabled" && stdout.Len() != 0 {
				t.Fatalf("disabled sweep produced output: %s", stdout.String())
			}
		})
	}
}
