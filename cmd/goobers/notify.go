package main

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/desktopnotify"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
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
}

type schedulerSetupOption func(*schedulerSetupOptions)

func withDesktopNotifications(override notifyFlag, warnings io.Writer) schedulerSetupOption {
	return func(options *schedulerSetupOptions) {
		options.desktopNotifications = true
		options.notifyOverride = override
		options.notificationWarnings = warnings
	}
}

var newNativeNotifier = desktopnotify.NewNative

const desktopNotificationTimeout = 5 * time.Second

func buildTerminalNotifier(
	l instance.Layout,
	cfg *instance.Config,
	scrubber journal.Scrubber,
	options schedulerSetupOptions,
) runner.TerminalNotifier {
	if !options.desktopNotifications {
		return nil
	}
	mode := options.notifyOverride.resolve(cfg.Notifications)
	if mode == notificationOff {
		return nil
	}
	native, supported := newNativeNotifier()
	if !supported {
		if options.notificationWarnings != nil {
			pf(options.notificationWarnings, "warning: desktop notifications are not supported on %s; continuing without notifications\n", runtime.GOOS)
		}
		return nil
	}
	return func(runID string, phase journal.RunPhase, finalState string) error {
		if !mode.includes(phase) {
			return nil
		}
		message, err := terminalNotificationMessage(l, runID, phase, finalState, scrubber)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), desktopNotificationTimeout)
		defer cancel()
		return native.Notify(ctx, message)
	}
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
