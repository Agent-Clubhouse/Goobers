package scheduling

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func newClaimTestLayout(t *testing.T) instance.Layout {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return layout
}

func TestStartupLocalClaimProbeUsesJournalOwnershipAndTerminalEvents(t *testing.T) {
	for _, tt := range []struct {
		name     string
		driver   journal.RunDriver
		terminal bool
		want     bool
	}{
		{name: "local", want: true}, {name: "engine", driver: journal.DriverEngine}, {name: "terminal", terminal: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := newClaimTestLayout(t)
			jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: "owner", Workflow: "implementation", WorkflowVersion: 1, Driver: tt.driver}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.terminal {
				if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := jr.Close(); err != nil {
				t.Fatal(err)
			}
			live, err := (StartupLocalRunLiveness{Layout: l}).RunLive(context.Background(), "owner")
			if err != nil || live != tt.want {
				t.Fatalf("live=%v err=%v want=%v", live, err, tt.want)
			}
			for _, missing := range []string{"missing", "owner/backlog-reconcile/1/1"} {
				live, err := (StartupLocalRunLiveness{Layout: l}).RunLive(context.Background(), missing)
				if live || err != nil {
					t.Fatalf("%s live=%v err=%v", missing, live, err)
				}
			}
		})
	}
}

func TestStartupLocalClaimProbeReportsInvalidEvidence(t *testing.T) {
	l := newClaimTestLayout(t)
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: "owner", Workflow: "implementation", WorkflowVersion: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(l.RunsDir(), "owner"), filepath.Join(l.RunsDir(), "wrong-owner")); err != nil {
		t.Fatal(err)
	}
	probe := StartupLocalRunLiveness{Layout: l}
	if live, err := probe.RunLive(context.Background(), "wrong-owner"); err == nil || live {
		t.Fatalf("identity mismatch live=%v err=%v", live, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := probe.RunLive(ctx, "wrong-owner"); err == nil {
		t.Fatal("cancelled probe succeeded")
	}
}
