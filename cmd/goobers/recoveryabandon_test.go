package main

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

func TestRecoveryAbandonCommandRequiresExactTerminalSnapshot(t *testing.T) {
	for _, mode := range []string{"success", "renewed-success", "running", "wrong-digest", "wrong-ref", "stage", "missing-confirmation"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GOOBERS_RUN_ID", "")
			layout := instance.NewLayout(initDemo(t))
			now := time.Now().UTC()
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
			path := seedRecoverySelection(t, layout, repo, "source", "7", now.Add(-time.Hour), now.Add(time.Hour), mode != "running")
			if mode == "renewed-success" {
				if _, err := recovery.RenewRetention(context.Background(), path, now.Add(2*time.Hour), 1<<20); err != nil {
					t.Fatal(err)
				}
			}
			record, err := recovery.ReadRetainedRecord(path)
			if err != nil {
				t.Fatal(err)
			}
			ref, digest := record.Ref, record.PatchDigest
			if mode == "wrong-digest" {
				digest = "sha256:wrong"
			}
			if mode == "wrong-ref" {
				ref = "refs/heads/main"
			}
			if mode == "stage" {
				t.Setenv("GOOBERS_RUN_ID", "stage-run")
			}
			args := []string{"--run", record.RunID, "--ref", ref, "--confirm-digest", digest, layout.Root}
			want := 1
			if mode == "success" || mode == "renewed-success" {
				want = 0
			}
			if mode == "missing-confirmation" {
				args = []string{"--run", record.RunID, "--ref", ref, layout.Root}
				want = 2
			}
			var stdout, stderr bytes.Buffer
			if code := runRecoveryAbandon(args, &stdout, &stderr); code != want {
				t.Fatalf("code=%d want=%d stderr=%s", code, want, stderr.String())
			}
			events, err := journal.ReadInstanceLog(layout.SchedulerDir())
			if err != nil {
				t.Fatal(err)
			}
			abandoned, err := recovery.ExplicitlyAbandoned(events, record)
			if err != nil || abandoned != (mode == "success" || mode == "renewed-success") {
				t.Fatalf("abandonment=%t err=%v", abandoned, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("command deleted archive metadata: %v", err)
			}
		})
	}
}
