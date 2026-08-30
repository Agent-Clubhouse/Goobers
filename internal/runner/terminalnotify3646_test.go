package runner

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TestRunnerTerminalNotifierErrorJournaled is #3646's runner half: a
// TerminalNotifier failure (the instance circuit breaker failing to apply
// goobers:needs-human) used to be discarded outright, leaving no evidence that
// the protection had failed. It is now journaled as
// terminal_notification_failed while the run still reaches its terminal phase.
func TestRunnerTerminalNotifierErrorJournaled(t *testing.T) {
	machine := terminalFailMachine(t)
	byTask := map[string]stubTaskResult{
		"run-notify-3646:implement": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: "executor_error", Message: "boom"},
		},
	}
	r, runsDir := newTestRunner(t, byTask, nil)
	r.cfg.NotifyTerminal = func(string, journal.RunPhase, string) error {
		return errors.New("apply circuit breaker: provider unreachable")
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-notify-3646",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed even though the terminal notifier errored", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-notify-3646"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawNotifyFailure bool
	for _, e := range events {
		if e.Type == journal.EventError && e.Error != nil && e.Error.Code == "terminal_notification_failed" {
			sawNotifyFailure = true
			if e.Error.Message == "" {
				t.Fatal("terminal_notification_failed carries no diagnostic message")
			}
		}
	}
	if !sawNotifyFailure {
		t.Fatal("expected a terminal_notification_failed error event recording the notifier's own failure")
	}
	phase, err := rd.Phase()
	if err != nil {
		t.Fatalf("Phase: %v", err)
	}
	if phase != journal.PhaseFailed {
		t.Fatalf("reconstructed phase = %q, want failed: the trailing error event must not disturb terminalization", phase)
	}
}

// TestRunnerTerminalNotifierSuccessJournalsNothing pins that the successful
// path is unchanged: no error event is appended after run.finished.
func TestRunnerTerminalNotifierSuccessJournalsNothing(t *testing.T) {
	machine := terminalFailMachine(t)
	byTask := map[string]stubTaskResult{
		"run-notify-3646-ok:implement": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: "executor_error", Message: "boom"},
		},
	}
	r, runsDir := newTestRunner(t, byTask, nil)
	var calls int
	r.cfg.NotifyTerminal = func(string, journal.RunPhase, string) error {
		calls++
		return nil
	}

	if _, err := r.Start(context.Background(), StartInput{
		RunID:   "run-notify-3646-ok",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if calls != 1 {
		t.Fatalf("NotifyTerminal calls = %d, want 1", calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-notify-3646-ok"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, e := range events {
		if e.Type == journal.EventError && e.Error != nil && e.Error.Code == "terminal_notification_failed" {
			t.Fatal("a successful terminal notification must not journal terminal_notification_failed")
		}
	}
}
