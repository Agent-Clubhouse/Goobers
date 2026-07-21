//go:build !windows

// Package winsvc adapts the goobers daemon's context-cancellation shutdown path
// to the Windows Service Control Manager (SCM).
//
// The daemon's single graceful-shutdown path is context cancellation: on unix,
// SIGINT/SIGTERM cancel the root context (internal/signals) and the daemon
// drains in-flight runs before exiting. On Windows there are no unix signals; a
// service is stopped when the SCM delivers SERVICE_CONTROL_STOP (or SHUTDOWN).
// This package translates that control message into cancellation of the same
// context, so `SERVICE_CONTROL_STOP ⇒ graceful drain ⇒ escalation` is identical
// to the unix `SIGTERM ⇒ graceful drain ⇒ escalation` path (issue #639 AC:
// "Stop semantics identical across platforms").
//
// The real implementation lives in winsvc_windows.go behind `//go:build
// windows` and imports golang.org/x/sys/windows/svc. This file is the
// non-Windows stub: every entry point compiles and links everywhere so callers
// (cmd/goobers) need no build tags, and IsWindowsService always reports false,
// leaving the unix signal path untouched.
//
// Note (#625 seam): today the "stop hook" the SCM handler drives is plain
// context cancellation, which is exactly what SIGTERM does now. When P3 (#625)
// introduces a dedicated service-stop hook / unified shutdown trigger, the
// Windows Execute handler swaps its cancel() for that hook without changing this
// package's public shape.
package winsvc

import (
	"context"
	"errors"
)

// ErrUnsupported is returned by Run on platforms without a Windows Service
// Control Manager. It is never encountered in practice because callers only
// reach Run after IsWindowsService reports true, which off Windows it never
// does.
var ErrUnsupported = errors.New("winsvc: Windows service control is not supported on this platform")

// IsWindowsService reports whether the current process was launched by the
// Windows Service Control Manager. Off Windows it is always (false, nil), so the
// daemon keeps its unix signal-driven shutdown path.
func IsWindowsService() (bool, error) {
	return false, nil
}

// Run executes fn under the Windows SCM, cancelling the context passed to fn
// when the service is asked to stop, and returns fn's exit code. Off Windows it
// is an unreachable stub returning ErrUnsupported without invoking fn.
func Run(name string, fn func(ctx context.Context) int) (int, error) {
	_ = name
	_ = fn
	return 1, ErrUnsupported
}
