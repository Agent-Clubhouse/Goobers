package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

func TestRecoveryDeadlineExpiredProtectsTerminalWindowAndActiveRuns(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for _, mode := range []string{"expired", "boundary", "running", "recent-finish", "renewed", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			start := now.Add(-90 * 24 * time.Hour)
			finish := now.Add(-31 * 24 * time.Hour)
			if mode == "recent-finish" {
				finish = now.Add(-time.Hour)
			}
			if mode == "boundary" {
				finish = now.Add(-30 * 24 * time.Hour)
			}
			root := t.TempDir()
			identity := journal.RunIdentity{Schema: journal.RunSchema, RunID: "expiry-run", Workflow: "implementation", WorkflowVersion: 1, StartedAt: start}
			run, err := journal.Create(root, identity, nil, journal.WithClock(func() time.Time { return finish }))
			if err != nil {
				t.Fatal(err)
			}
			if mode != "running" {
				if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			reader, err := journal.OpenReadOnly(filepath.Join(root, identity.RunID))
			if err != nil {
				t.Fatal(err)
			}
			record := recovery.Record{RunID: identity.RunID, RetainUntil: now.Add(-60 * 24 * time.Hour)}
			if mode == "renewed" {
				record.RetainUntil = now.Add(time.Hour)
			}
			if mode == "foreign" {
				record.RunID = "another-run"
			}
			eligible, err := recoveryDeadlineExpired(reader, record, now)
			if (err != nil) != (mode == "foreign") {
				t.Fatalf("eligibility error: %v", err)
			}
			if want := mode == "expired" || mode == "boundary"; eligible != want {
				t.Fatalf("eligible=%t, want %t", eligible, want)
			}
		})
	}
}
