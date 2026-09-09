package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

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
			cfg := instance.RetentionConfig{Enabled: mode == "delete" || mode == "failure" || mode == "enabled-dry-run", DryRun: mode == "dry-run" || mode == "enabled-dry-run"}
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
