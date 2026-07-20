package main

import (
	"bytes"
	"context"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/desktopnotify"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

type recordingDesktopNotifier struct {
	messages    []desktopnotify.Message
	deadline    time.Time
	hasDeadline bool
	err         error
}

func (n *recordingDesktopNotifier) Notify(ctx context.Context, message desktopnotify.Message) error {
	n.messages = append(n.messages, message)
	n.deadline, n.hasDeadline = ctx.Deadline()
	return n.err
}

func TestNotifyFlagModes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want notificationMode
	}{
		{name: "bare", args: []string{"--notify"}, want: notificationImportant},
		{name: "all", args: []string{"--notify=all"}, want: notificationAll},
		{name: "false", args: []string{"--notify=false"}, want: notificationOff},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var value notifyFlag
			flags := flag.NewFlagSet("notify", flag.ContinueOnError)
			flags.SetOutput(&bytes.Buffer{})
			flags.Var(&value, "notify", "")
			if err := flags.Parse(test.args); err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !value.set || value.mode != test.want {
				t.Fatalf("notify flag = %+v, want mode %v", value, test.want)
			}
		})
	}
}

func TestNotifyFlagOverridesConfig(t *testing.T) {
	if got := (notifyFlag{}).resolve(true); got != notificationImportant {
		t.Fatalf("configured notifications resolved to %v, want important", got)
	}
	override := notifyFlag{set: true, mode: notificationOff}
	if got := override.resolve(true); got != notificationOff {
		t.Fatalf("false flag override resolved to %v, want off", got)
	}
	if notificationImportant.includes(journal.PhaseCompleted) {
		t.Fatal("important mode included completed run")
	}
	if !notificationImportant.includes(journal.PhaseFailed) || !notificationImportant.includes(journal.PhaseEscalated) {
		t.Fatal("important mode omitted failed or escalated run")
	}
	if !notificationAll.includes(journal.PhaseCompleted) || !notificationAll.includes(journal.PhaseAborted) {
		t.Fatal("all mode omitted a terminal outcome")
	}
}

func TestTerminalNotificationMessageScrubsCause(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	runID := "1234567890abcdef"
	jr, err := journal.Create(l.ForGaggle("goobers").RunsDir(), journal.RunIdentity{
		RunID:    runID,
		Workflow: "implementation",
		Gaggle:   "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = jr.Close() }()
	if err := jr.Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: "run_failed", Message: "provider rejected super-secret-token"},
	}); err != nil {
		t.Fatalf("append cause: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatalf("append terminal: %v", err)
	}

	registry := journal.NewRegistryScrubber()
	registry.Register([]byte("super-secret-token"))
	message, err := terminalNotificationMessage(l, runID, journal.PhaseFailed, "implement", registry)
	if err != nil {
		t.Fatalf("terminalNotificationMessage: %v", err)
	}
	if message.Title != "Goobers run failed" {
		t.Fatalf("title = %q", message.Title)
	}
	if !strings.Contains(message.Body, "implementation [12345678]") {
		t.Fatalf("body does not contain workflow and short run id: %q", message.Body)
	}
	if strings.Contains(message.Body, "super-secret-token") || !strings.Contains(message.Body, journal.Redacted) {
		t.Fatalf("body was not scrubbed: %q", message.Body)
	}
}

func TestTerminalNotificationCauseUsesEscalatingGate(t *testing.T) {
	events := []journal.Event{{
		Type:    journal.EventGateEvaluated,
		Gate:    "review",
		Verdict: "needs-changes",
		Target:  "@escalate",
	}}
	got := terminalNotificationCause(events, journal.PhaseEscalated, "review")
	if got != "gate review: needs-changes -> @escalate" {
		t.Fatalf("cause = %q", got)
	}
}

func TestTerminalNotificationCauseUsesStalledRunError(t *testing.T) {
	events := []journal.Event{{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: runner.RunStalledErrorCode, Message: "no journal progress for 45m"},
	}}
	got := terminalNotificationCause(events, journal.PhaseEscalated, "implement")
	if got != "no journal progress for 45m" {
		t.Fatalf("cause = %q", got)
	}
}

func TestTerminalNotificationCauseDoesNotReuseClearedFailure(t *testing.T) {
	events := []journal.Event{
		{
			Type:   journal.EventStageFinished,
			Stage:  "implement",
			Status: "failure",
			Error:  &journal.ErrorDetail{Code: "FAILED", Message: "temporary failure"},
		},
		{
			Type:    journal.EventGateEvaluated,
			Gate:    "review",
			Verdict: "pass",
		},
	}
	got := terminalNotificationCause(events, journal.PhaseCompleted, "review")
	if got != "run completed" {
		t.Fatalf("cause = %q, want completed terminal outcome", got)
	}
}

func TestBuildTerminalNotifierWarnsOnceWhenUnsupported(t *testing.T) {
	original := newNativeNotifier
	newNativeNotifier = func() (desktopnotify.Notifier, bool) { return nil, false }
	t.Cleanup(func() { newNativeNotifier = original })

	var warnings bytes.Buffer
	notifier := buildTerminalNotifier(
		instance.NewLayout(t.TempDir()),
		&instance.Config{Notifications: true},
		journal.NewPatternScrubber(),
		schedulerSetupOptions{desktopNotifications: true, notificationWarnings: &warnings},
	)
	if notifier != nil {
		t.Fatal("unsupported platform returned a terminal notifier")
	}
	if got := strings.Count(warnings.String(), "desktop notifications are not supported"); got != 1 {
		t.Fatalf("startup warning count = %d, output %q", got, warnings.String())
	}
}

func TestTerminalNotifierFiltersCompletedAndSendsEscalated(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	runID := "abcdef0123456789"
	jr, err := journal.Create(l.ForGaggle("goobers").RunsDir(), journal.RunIdentity{
		RunID:    runID,
		Workflow: "implementation",
		Gaggle:   "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = jr.Close() }()
	if err := jr.Append(journal.Event{
		Type:    journal.EventGateEvaluated,
		Gate:    "review",
		Verdict: "fail",
		Target:  "@escalate",
	}); err != nil {
		t.Fatalf("append gate: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatalf("append terminal: %v", err)
	}

	native := &recordingDesktopNotifier{}
	original := newNativeNotifier
	newNativeNotifier = func() (desktopnotify.Notifier, bool) { return native, true }
	t.Cleanup(func() { newNativeNotifier = original })
	notifier := buildTerminalNotifier(
		l,
		&instance.Config{},
		journal.NewPatternScrubber(),
		schedulerSetupOptions{
			desktopNotifications: true,
			notifyOverride:       notifyFlag{set: true, mode: notificationImportant},
			notificationWarnings: &bytes.Buffer{},
		},
	)
	if err := notifier("does-not-exist", journal.PhaseCompleted, "done"); err != nil {
		t.Fatalf("completed notification filter: %v", err)
	}
	if err := notifier(runID, journal.PhaseEscalated, "review"); err != nil {
		t.Fatalf("escalated notification: %v", err)
	}
	if len(native.messages) != 1 {
		t.Fatalf("notification count = %d, want 1", len(native.messages))
	}
	if !native.hasDeadline || native.deadline.After(time.Now().Add(desktopNotificationTimeout)) {
		t.Fatalf("native notification deadline = %v, want a deadline within %s", native.deadline, desktopNotificationTimeout)
	}
}
