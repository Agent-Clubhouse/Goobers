// Package shutdown provides the single cross-platform graceful-shutdown trigger
// for the Goobers daemon and CLI. Every long-running process derives its root
// context here so that a platform shutdown trigger — SIGINT/SIGTERM on unix,
// os.Interrupt (Ctrl+C / Ctrl+Break) on windows, or a Windows Service Control
// Manager STOP/SHUTDOWN via RequestStop — funnels into one context cancellation
// and one shared downstream sequence.
//
// It exists so call sites never branch on runtime.GOOS or register OS signals
// directly. Following the convention of internal/platform/lock and
// internal/platform/proc, the surface here is small and the platform-specific
// trigger lives in build-tagged files:
//
//   - Notify wires the platform OS shutdown signals to a Notifier.
//   - Notifier.Context is cancelled on the first trigger.
//   - Notifier.RequestStop is the exported hook a non-signal source (the Windows
//     SCM handler, P8) drives so a service stop shares the same cancellation.
//   - DrainWithEscalation is the shared downstream: drain, then hard-kill on
//     timeout — the platform-agnostic tail every trigger funnels into.
//
// On unix the observed signals are SIGINT and SIGTERM (today's behavior,
// unchanged); on windows only os.Interrupt is observable, because windows cannot
// deliver SIGTERM — an external supervisor stops the daemon-as-a-service through
// the SCM, which the winsvc handler routes in via RequestStop.
package shutdown

import (
	"context"
	"os"
	"os/signal"
	"sync"
)

// Reason records what triggered graceful shutdown, so downstream code (and
// tests) can distinguish an OS signal from a service-manager stop.
type Reason string

const (
	// ReasonSignal is a platform OS shutdown signal (unix SIGINT/SIGTERM,
	// windows Ctrl+C / Ctrl+Break).
	ReasonSignal Reason = "signal"
	// ReasonService is a Windows Service Control Manager STOP/SHUTDOWN control,
	// delivered through RequestStop by the winsvc handler (P8). It drives the
	// exact same cancellation as ReasonSignal so stop semantics are identical
	// across platforms.
	ReasonService Reason = "service"
)

// Notifier funnels every shutdown trigger into a single context cancellation.
// Both the platform OS-signal watcher and external stop requests (RequestStop,
// used by the Windows SCM handler) resolve to the same Context() being
// cancelled, so all downstream shutdown is one platform-agnostic path.
//
// The zero value is not usable; construct with Notify or NewExternal.
type Notifier struct {
	ctx     context.Context
	cancel  context.CancelFunc
	ch      chan os.Signal // nil for an external-only notifier (no OS signals)
	stopped chan struct{}  // closed by Stop to release the watcher goroutine
	stopOne sync.Once      // guards stopped close + signal.Stop + cancel

	mu     sync.Mutex
	fired  bool
	reason Reason

	// onHard is invoked when a second OS signal arrives while a first-signal
	// shutdown is already draining, force-killing a wedged shutdown. Overridable
	// in tests; defaults to a process exit with a non-zero code.
	onHard func()
}

// Notify returns a Notifier wired to the platform's OS shutdown signals. The
// first signal cancels Context(); a second OS signal invokes the hard-exit so a
// wedged shutdown can still be force-killed from the same terminal. Callers
// should defer Stop.
//
// On unix the observed signals are SIGINT and SIGTERM; on windows, os.Interrupt
// (Ctrl+C / Ctrl+Break) — see the platform notify functions.
func Notify() *Notifier {
	n := newNotifier()
	ch := make(chan os.Signal, 2)
	n.ch = ch
	notify(ch)

	go n.watch()
	return n
}

// NewExternal returns a Notifier with no OS-signal registration — only
// RequestStop drives it. The Windows SCM handler uses this so a service STOP,
// not a console signal, triggers shutdown while still sharing this package's
// single downstream path.
func NewExternal() *Notifier {
	return newNotifier()
}

func newNotifier() *Notifier {
	ctx, cancel := context.WithCancel(context.Background())
	return &Notifier{
		ctx:     ctx,
		cancel:  cancel,
		stopped: make(chan struct{}),
		onHard:  func() { os.Exit(1) },
	}
}

// watch is the OS-signal loop: the first signal begins graceful shutdown; a
// second signal (before Stop) hard-exits. It exits promptly when Stop closes the
// stopped channel, so it never leaks after the process releases the notifier.
func (n *Notifier) watch() {
	select {
	case <-n.ch:
		n.RequestStop(ReasonSignal)
	case <-n.stopped:
		return
	}
	// Graceful shutdown has begun; arm the force-kill on a second signal.
	select {
	case <-n.ch:
		n.onHard()
	case <-n.stopped:
	}
}

// Context returns the context cancelled on the first shutdown trigger.
func (n *Notifier) Context() context.Context { return n.ctx }

// Reason returns the trigger that began shutdown, or "" if none has fired yet.
func (n *Notifier) Reason() Reason {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.reason
}

// RequestStop begins graceful shutdown from a non-signal source — the Windows
// SCM stop/shutdown handler (P8) calls it so SERVICE_CONTROL_STOP drives the
// exact same cancellation as a unix SIGTERM. It is the exported service-stop
// hook. Safe to call repeatedly and concurrently; only the first call records a
// reason and cancels the context.
func (n *Notifier) RequestStop(reason Reason) {
	n.mu.Lock()
	if !n.fired {
		n.fired = true
		n.reason = reason
	}
	n.mu.Unlock()
	n.cancel()
}

// Stop releases the signal handler (if any) and cancels the context. It is
// idempotent and safe to defer.
func (n *Notifier) Stop() {
	n.stopOne.Do(func() {
		if n.ch != nil {
			signal.Stop(n.ch)
		}
		close(n.stopped)
	})
	n.cancel()
}
