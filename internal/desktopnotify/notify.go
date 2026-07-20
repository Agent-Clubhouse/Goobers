// Package desktopnotify delivers best-effort native desktop notifications.
package desktopnotify

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Message is the native notification content.
type Message struct {
	Title string
	Body  string
}

// Notifier delivers a desktop notification.
type Notifier interface {
	Notify(context.Context, Message) error
}

type commandRunner interface {
	Run(context.Context, string, ...string) error
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

type macOSNotifier struct {
	run commandRunner
}

// NewNative returns the notifier for the current platform. The boolean is
// false when the platform has no implementation.
func NewNative() (Notifier, bool) {
	return newForPlatform(runtime.GOOS, execRunner{})
}

func newForPlatform(platform string, run commandRunner) (Notifier, bool) {
	if platform != "darwin" {
		return nil, false
	}
	return &macOSNotifier{run: run}, true
}

func (n *macOSNotifier) Notify(ctx context.Context, message Message) error {
	script := fmt.Sprintf(
		`display notification "%s" with title "%s"`,
		escapeAppleScriptString(message.Body),
		escapeAppleScriptString(message.Title),
	)
	if err := n.run.Run(ctx, "osascript", "-e", script); err != nil {
		return fmt.Errorf("desktop notification: %w", err)
	}
	return nil
}

func escapeAppleScriptString(value string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\r", "",
		"\n", `\n`,
	).Replace(value)
}
