//go:build windows

package winsvc

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

const serviceTestTimeout = 5 * time.Second

type executeResult struct {
	serviceSpecific bool
	exitCode        uint32
}

type fakeServiceControl struct {
	requests chan svc.ChangeRequest
	changes  chan svc.Status
}

func newFakeServiceControl() *fakeServiceControl {
	return &fakeServiceControl{
		requests: make(chan svc.ChangeRequest, 4),
		changes:  make(chan svc.Status, 4),
	}
}

func (f *fakeServiceControl) run(h svc.Handler) <-chan executeResult {
	result := make(chan executeResult, 1)
	go func() {
		serviceSpecific, exitCode := h.Execute(nil, f.requests, f.changes)
		result <- executeResult{serviceSpecific: serviceSpecific, exitCode: exitCode}
	}()
	return result
}

func (f *fakeServiceControl) receiveStatus(t *testing.T) svc.Status {
	t.Helper()
	select {
	case status := <-f.changes:
		return status
	case <-time.After(serviceTestTimeout):
		t.Fatal("timed out waiting for service status")
		return svc.Status{}
	}
}

func receiveExecuteResult(t *testing.T, result <-chan executeResult) executeResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(serviceTestTimeout):
		t.Fatal("timed out waiting for service handler to exit")
		return executeResult{}
	}
}

func requireStatus(t *testing.T, got, want svc.Status) {
	t.Helper()
	if got != want {
		t.Fatalf("service status = %+v, want %+v", got, want)
	}
}

func requireStarted(t *testing.T, control *fakeServiceControl) {
	t.Helper()
	requireStatus(t, control.receiveStatus(t), svc.Status{State: svc.StartPending})
	requireStatus(t, control.receiveStatus(t), svc.Status{
		State:   svc.Running,
		Accepts: svc.AcceptStop | svc.AcceptShutdown,
	})
}

func TestHandlerExecuteStartAndNaturalExit(t *testing.T) {
	control := newFakeServiceControl()
	h := &handler{fn: func(context.Context) int { return 17 }}

	result := control.run(h)

	requireStarted(t, control)
	requireStatus(t, control.receiveStatus(t), svc.Status{State: svc.StopPending})
	if got := receiveExecuteResult(t, result); got != (executeResult{exitCode: 17}) {
		t.Fatalf("Execute() = %+v, want exit code 17", got)
	}
	if h.code != 17 {
		t.Fatalf("handler code = %d, want 17", h.code)
	}
}

func TestHandlerExecuteStopRequests(t *testing.T) {
	for _, command := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		t.Run(commandName(command), func(t *testing.T) {
			control := newFakeServiceControl()
			cancelled := make(chan struct{})
			h := &handler{fn: func(ctx context.Context) int {
				<-ctx.Done()
				close(cancelled)
				return 23
			}}

			result := control.run(h)
			requireStarted(t, control)
			control.requests <- svc.ChangeRequest{Cmd: command}

			requireStatus(t, control.receiveStatus(t), svc.Status{
				State:      svc.StopPending,
				CheckPoint: 1,
				WaitHint:   stopWaitHintMS,
			})
			select {
			case <-cancelled:
			case <-time.After(serviceTestTimeout):
				t.Fatal("service function context was not cancelled")
			}
			if got := receiveExecuteResult(t, result); got != (executeResult{exitCode: 23}) {
				t.Fatalf("Execute() = %+v, want exit code 23", got)
			}
		})
	}
}

func TestHandlerExecuteIgnoresUnexpectedControls(t *testing.T) {
	for _, command := range []svc.Cmd{svc.Pause, svc.Continue, svc.ParamChange} {
		t.Run(commandName(command), func(t *testing.T) {
			control := newFakeServiceControl()
			h := &handler{fn: func(ctx context.Context) int {
				<-ctx.Done()
				return 0
			}}

			result := control.run(h)
			requireStarted(t, control)
			control.requests <- svc.ChangeRequest{Cmd: command}

			marker := svc.Status{State: svc.Paused, CheckPoint: uint32(command)}
			control.requests <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: marker}
			requireStatus(t, control.receiveStatus(t), marker)

			control.requests <- svc.ChangeRequest{Cmd: svc.Stop}
			requireStatus(t, control.receiveStatus(t), svc.Status{
				State:      svc.StopPending,
				CheckPoint: 1,
				WaitHint:   stopWaitHintMS,
			})
			if got := receiveExecuteResult(t, result); got != (executeResult{}) {
				t.Fatalf("Execute() = %+v, want zero exit code", got)
			}
		})
	}
}

func commandName(command svc.Cmd) string {
	switch command {
	case svc.Stop:
		return "stop"
	case svc.Shutdown:
		return "shutdown"
	case svc.Pause:
		return "pause"
	case svc.Continue:
		return "continue"
	case svc.ParamChange:
		return "parameter_change"
	default:
		return "unknown"
	}
}
