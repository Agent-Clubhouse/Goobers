package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/desktopnotify"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/speechnotify"
	speechtest "github.com/goobers/goobers/test/testsupport/speechnotify"
)

type recordingDesktopNotifier struct {
	messages    []desktopnotify.Message
	deadline    time.Time
	hasDeadline bool
	err         error
}

type lifecycleSpeechSynthesizer struct {
	started chan struct{}
}

func (*lifecycleSpeechSynthesizer) Name() string {
	return "lifecycle"
}

func (*lifecycleSpeechSynthesizer) Preflight(context.Context, speechnotify.Config) (speechnotify.Preflight, error) {
	return speechnotify.Preflight{Engine: "lifecycle", AudioAvailable: true}, nil
}

func (s *lifecycleSpeechSynthesizer) Synthesize(ctx context.Context, _ speechnotify.Config, _ string) error {
	close(s.started)
	<-ctx.Done()
	return ctx.Err()
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
	notifier, err := buildTerminalNotifier(
		context.Background(),
		instance.NewLayout(t.TempDir()),
		&instance.Config{Notifications: true},
		journal.NewPatternScrubber(),
		schedulerSetupOptions{desktopNotifications: true, notificationWarnings: &warnings},
	)
	if err != nil {
		t.Fatalf("buildTerminalNotifier: %v", err)
	}
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
	notifier, err := buildTerminalNotifier(
		context.Background(),
		l,
		&instance.Config{},
		journal.NewPatternScrubber(),
		schedulerSetupOptions{
			desktopNotifications: true,
			notifyOverride:       notifyFlag{set: true, mode: notificationImportant},
			notificationWarnings: &bytes.Buffer{},
		},
	)
	if err != nil {
		t.Fatalf("buildTerminalNotifier: %v", err)
	}
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

func TestTerminalNotifierSpeechIsOptInExactAndReceipted(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	runID := "fedcba9876543210"
	jr, err := journal.Create(l.ForGaggle("goobers").RunsDir(), journal.RunIdentity{
		RunID:    runID,
		Workflow: "mission-control",
		Gaggle:   "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = jr.Close() }()
	if err := jr.Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: "run_failed", Message: "CPU is 91.2% — threshold 90%"},
	}); err != nil {
		t.Fatalf("append cause: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatalf("append terminal: %v", err)
	}

	fake := &speechtest.FakeSynthesizer{}
	installFakeSpeechSink(t, fake)
	notifier, err := buildTerminalNotifier(
		context.Background(),
		l,
		&instance.Config{Speech: &speechnotify.Config{Enabled: true}},
		journal.NewPatternScrubber(),
		schedulerSetupOptions{},
	)
	if err != nil {
		t.Fatalf("buildTerminalNotifier: %v", err)
	}
	if err := notifier(runID, journal.PhaseCompleted, "done"); err != nil {
		t.Fatalf("completed notification filter: %v", err)
	}
	if err := notifier(runID, journal.PhaseFailed, "implement"); err != nil {
		t.Fatalf("failed notification: %v", err)
	}
	want := "mission-control [fedcba98]\nCPU is 91.2% — threshold 90%"
	if got := fake.Utterances(); len(got) != 1 || got[0] != want {
		t.Fatalf("utterances = %#v, want %q", got, want)
	}
	raw, err := os.ReadFile(filepath.Join(l.SchedulerDir(), speechnotify.ReceiptFileName))
	if err != nil {
		t.Fatalf("read receipt log: %v", err)
	}
	if !strings.Contains(string(raw), `"notificationId":"`+runID+`"`) ||
		!strings.Contains(string(raw), `"status":"delivered"`) ||
		strings.Contains(string(raw), "CPU is 91.2%") {
		t.Fatalf("receipt log = %q", raw)
	}
}

func TestTerminalNotifierRejectsEnabledSpeechWithoutPreflight(t *testing.T) {
	fake := &speechtest.FakeSynthesizer{PreflightErr: errors.New("audio device unavailable")}
	installFakeSpeechSink(t, fake)
	notifier, err := buildTerminalNotifier(
		context.Background(),
		instance.NewLayout(t.TempDir()),
		&instance.Config{Speech: &speechnotify.Config{Enabled: true}},
		journal.NewPatternScrubber(),
		schedulerSetupOptions{},
	)
	if err == nil || !strings.Contains(err.Error(), "speech notifications unavailable") ||
		!strings.Contains(err.Error(), "audio device unavailable") {
		t.Fatalf("buildTerminalNotifier = (%v, %v)", notifier, err)
	}
}

func TestTerminalNotifierSpeechStopsWithLifecycleContext(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	runID := "0123456789abcdef"
	jr, err := journal.Create(l.ForGaggle("goobers").RunsDir(), journal.RunIdentity{
		RunID:    runID,
		Workflow: "implementation",
		Gaggle:   "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = jr.Close() }()
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatalf("append terminal: %v", err)
	}

	synthesizer := &lifecycleSpeechSynthesizer{started: make(chan struct{})}
	original := newNativeSpeechSink
	newNativeSpeechSink = func(config speechnotify.Config, recorder speechnotify.Recorder) (*speechnotify.Sink, error) {
		return speechnotify.New(config, synthesizer, recorder)
	}
	t.Cleanup(func() { newNativeSpeechSink = original })

	lifecycleCtx, cancel := context.WithCancel(context.Background())
	notifier, err := buildTerminalNotifier(
		lifecycleCtx,
		l,
		&instance.Config{Speech: &speechnotify.Config{Enabled: true}},
		journal.NewPatternScrubber(),
		schedulerSetupOptions{},
	)
	if err != nil {
		t.Fatalf("buildTerminalNotifier: %v", err)
	}
	delivered := make(chan error, 1)
	go func() {
		delivered <- notifier(runID, journal.PhaseFailed, "implement")
	}()
	select {
	case <-synthesizer.started:
	case <-time.After(time.Second):
		t.Fatal("speech synthesis did not start")
	}
	cancel()
	select {
	case err := <-delivered:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("notifier error = %v, want lifecycle cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("speech delivery did not stop after lifecycle cancellation")
	}

	raw, err := os.ReadFile(filepath.Join(l.SchedulerDir(), speechnotify.ReceiptFileName))
	if err != nil {
		t.Fatalf("read receipt log: %v", err)
	}
	if strings.Count(string(raw), `"status":"started"`) != 1 ||
		strings.Count(string(raw), `"status":"failed"`) != 1 {
		t.Fatalf("receipt log = %q", raw)
	}
}
