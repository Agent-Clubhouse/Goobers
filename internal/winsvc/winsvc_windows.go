//go:build windows

package winsvc

import (
	"context"
	"time"

	"golang.org/x/sys/windows/svc"
)

// stopWaitHintMS is the WaitHint (in milliseconds) the handler reports to the
// SCM while draining after a stop request. It must comfortably exceed the
// daemon's own drain budget (drainGrace 30s + HTTP shutdown grace 5s in
// cmd/goobers/up.go) so the SCM does not kill the process mid-drain. The handler
// also advances CheckPoint on a ticker while draining, which keeps the SCM
// waiting even if a drain legitimately runs long.
const stopWaitHintMS = 45_000

// checkpointInterval is how often the handler bumps CheckPoint (and re-asserts
// the WaitHint) while draining, so the SCM sees continuous progress.
const checkpointInterval = 2 * time.Second

// handler implements svc.Handler by running the daemon function under a
// cancellable context and cancelling it on SERVICE_CONTROL_STOP/SHUTDOWN — the
// Windows equivalent of the unix SIGTERM path.
type handler struct {
	fn   func(ctx context.Context) int
	code int
}

// Execute is invoked by the svc package when the SCM starts the service. It
// starts fn in a goroutine, reports Running, and then either exits when fn
// returns on its own or, on a stop/shutdown control, cancels fn's context and
// drains until fn returns — reporting StopPending with an advancing checkpoint
// throughout so the SCM waits for a graceful drain rather than force-killing.
func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() { done <- h.fn(ctx) }()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case code := <-done:
			// The daemon exited on its own (crash or self-initiated stop).
			h.code = code
			changes <- svc.Status{State: svc.StopPending}
			return false, uint32(code)
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				h.code = h.drain(cancel, done, changes)
				return false, uint32(h.code)
			default:
				// Ignore controls we did not advertise via Accepts.
			}
		}
	}
}

// drain cancels the daemon context and waits for fn to return, advancing the
// SCM checkpoint so a long-but-legitimate drain is not force-killed. It answers
// Interrogate during the wait so `sc query` stays responsive.
func (h *handler) drain(cancel context.CancelFunc, done <-chan int, changes chan<- svc.Status) int {
	cancel()

	checkPoint := uint32(1)
	status := svc.Status{State: svc.StopPending, CheckPoint: checkPoint, WaitHint: stopWaitHintMS}
	changes <- status

	ticker := time.NewTicker(checkpointInterval)
	defer ticker.Stop()

	for {
		select {
		case code := <-done:
			return code
		case <-ticker.C:
			checkPoint++
			status = svc.Status{State: svc.StopPending, CheckPoint: checkPoint, WaitHint: stopWaitHintMS}
			changes <- status
		}
	}
}

// IsWindowsService reports whether the current process was launched by the SCM.
func IsWindowsService() (bool, error) {
	return svc.IsWindowsService()
}

// Run executes fn under the SCM as service `name`, returning fn's exit code once
// the service stops. A non-nil error means the service dispatcher itself failed
// to start (e.g. the process was not launched as a service).
func Run(name string, fn func(ctx context.Context) int) (int, error) {
	h := &handler{fn: fn}
	if err := svc.Run(name, h); err != nil {
		return 1, err
	}
	return h.code, nil
}
