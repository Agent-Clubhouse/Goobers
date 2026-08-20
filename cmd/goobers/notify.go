package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/desktopnotify"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/speechnotify"
)

type notificationMode uint8

const (
	notificationOff notificationMode = iota
	notificationImportant
	notificationAll
)

type notifyFlag struct {
	set  bool
	mode notificationMode
}

func (f *notifyFlag) Set(value string) error {
	f.set = true
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		f.mode = notificationImportant
	case "all":
		f.mode = notificationAll
	case "false":
		f.mode = notificationOff
	default:
		return fmt.Errorf("invalid notify mode %q (want true, false, or all)", value)
	}
	return nil
}

func (f *notifyFlag) String() string {
	switch f.mode {
	case notificationImportant:
		return "true"
	case notificationAll:
		return "all"
	default:
		return "false"
	}
}

func (*notifyFlag) IsBoolFlag() bool { return true }

func (f notifyFlag) resolve(configured bool) notificationMode {
	if f.set {
		return f.mode
	}
	if configured {
		return notificationImportant
	}
	return notificationOff
}

func (m notificationMode) includes(phase journal.RunPhase) bool {
	if m == notificationAll {
		return phase != journal.PhaseRunning
	}
	return m == notificationImportant && (phase == journal.PhaseFailed || phase == journal.PhaseEscalated)
}

type schedulerSetupOptions struct {
	desktopNotifications bool
	notifyOverride       notifyFlag
	notificationWarnings io.Writer
	startupProgress      func(string)
}

type schedulerSetupOption func(*schedulerSetupOptions)

func withDesktopNotifications(override notifyFlag, warnings io.Writer) schedulerSetupOption {
	return func(options *schedulerSetupOptions) {
		options.desktopNotifications = true
		options.notifyOverride = override
		options.notificationWarnings = warnings
	}
}

func withStartupProgress(report func(string)) schedulerSetupOption {
	return func(options *schedulerSetupOptions) {
		options.startupProgress = report
	}
}

var newNativeNotifier = desktopnotify.NewNative

const desktopNotificationTimeout = 5 * time.Second

func buildTerminalNotifier(
	ctx context.Context,
	l instance.Layout,
	cfg *instance.Config,
	scrubber journal.Scrubber,
	options schedulerSetupOptions,
) (runner.TerminalNotifier, error) {
	var (
		desktop     desktopnotify.Notifier
		desktopMode notificationMode
	)
	if options.desktopNotifications {
		desktopMode = options.notifyOverride.resolve(cfg.Notifications)
		if desktopMode != notificationOff {
			native, supported := newNativeNotifier()
			if !supported {
				if options.notificationWarnings != nil {
					pf(options.notificationWarnings, "warning: desktop notifications are not supported on %s; continuing without desktop notifications\n", runtime.GOOS)
				}
			} else {
				desktop = native
			}
		}
	}

	speechConfig := cfg.EffectiveSpeechConfig()
	var speech *speechnotify.Sink
	if speechConfig.Enabled {
		recorder := speechnotify.NewFileRecorder(filepath.Join(l.SchedulerDir(), speechnotify.ReceiptFileName))
		var err error
		speech, err = newNativeSpeechSink(speechConfig, recorder)
		if err != nil {
			return nil, fmt.Errorf("configure speech notifications: %w", err)
		}
		if _, err := preflightSpeech(ctx, speech, speechConfig); err != nil {
			return nil, fmt.Errorf("speech notifications unavailable: %w", err)
		}
	}
	if desktop == nil && speech == nil {
		return nil, nil
	}

	return func(runID string, phase journal.RunPhase, finalState string) error {
		deliverDesktop := desktop != nil && desktopMode.includes(phase)
		deliverSpeech := speech != nil && notificationImportant.includes(phase)
		if !deliverDesktop && !deliverSpeech {
			return nil
		}
		message, err := terminalNotificationMessage(l, runID, phase, finalState, scrubber)
		if err != nil {
			return err
		}
		var deliveryErrors []error
		if deliverDesktop {
			ctx, cancel := context.WithTimeout(context.Background(), desktopNotificationTimeout)
			if err := desktop.Notify(ctx, message); err != nil {
				deliveryErrors = append(deliveryErrors, err)
			}
			cancel()
		}
		if deliverSpeech {
			timeout, _ := speechConfig.TimeoutDuration()
			deliveryCtx, cancel := context.WithTimeout(ctx, timeout)
			if _, err := speech.Deliver(deliveryCtx, speechnotify.Request{NotificationID: runID, Text: message.Body}); err != nil {
				deliveryErrors = append(deliveryErrors, err)
			}
			cancel()
		}
		return errors.Join(deliveryErrors...)
	}, nil
}

func terminalNotificationMessage(
	l instance.Layout,
	runID string,
	phase journal.RunPhase,
	finalState string,
	scrubber journal.Scrubber,
) (desktopnotify.Message, error) {
	runDir, err := l.FindRunDir(runID)
	if err != nil {
		return desktopnotify.Message{}, err
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return desktopnotify.Message{}, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return desktopnotify.Message{}, err
	}
	events, err := reader.Events()
	if err != nil {
		return desktopnotify.Message{}, err
	}
	cause := terminalNotificationCause(events, phase, finalState)
	cause = oneLine(string(scrubber.Scrub([]byte(cause))))
	if cause == "" {
		cause = fmt.Sprintf("run %s", phase)
	}
	body := fmt.Sprintf("%s [%s]\n%s", identity.Workflow, shortRunID(runID), cause)
	body = string(scrubber.Scrub([]byte(body)))
	return desktopnotify.Message{
		Title: fmt.Sprintf("Goobers run %s", phase),
		Body:  body,
	}, nil
}

func terminalNotificationCause(events []journal.Event, phase journal.RunPhase, finalState string) string {
	if phase == journal.PhaseCompleted {
		return "run completed"
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type == journal.EventRunFinished && event.Error != nil && event.Error.Message != "" {
			return event.Error.Message
		}
		if event.Type == journal.EventError && event.Error != nil {
			if event.Error.Code == "run_failed" && phase == journal.PhaseFailed {
				return event.Error.Message
			}
			if (event.Error.Code == "blocked_by_agent" || event.Error.Code == runner.RunStalledErrorCode) &&
				phase == journal.PhaseEscalated {
				return event.Error.Message
			}
		}
		if event.Type == journal.EventStageFinished && event.Status == "failure" && event.Error != nil {
			return event.Error.Message
		}
		if (phase == journal.PhaseEscalated || phase == journal.PhaseAborted) && event.Type == journal.EventGateEvaluated {
			return fmt.Sprintf("gate %s: %s -> %s", event.Gate, event.Verdict, event.Target)
		}
	}
	if finalState != "" {
		return fmt.Sprintf("terminal state: %s", finalState)
	}
	return fmt.Sprintf("run %s", phase)
}

func shortRunID(runID string) string {
	const length = 8
	if len(runID) <= length {
		return runID
	}
	return runID[:length]
}

func oneLine(value string) string {
	const maxRunes = 240
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "..."
}
