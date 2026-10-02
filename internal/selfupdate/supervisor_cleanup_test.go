package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

type cleanupProcess struct {
	done chan error
	kill func() error
}

func (p *cleanupProcess) Done() <-chan error { return p.done }
func (p *cleanupProcess) Kill() error        { return p.kill() }

func TestSupervisorDrainBeforeDoneClose(t *testing.T) {
	for name, killErr := range map[string]error{
		"already-waited": os.ErrProcessDone,
		"missing-group":  fmt.Errorf("kill group: %w", syscall.ESRCH),
	} {
		t.Run(name, func(t *testing.T) {
			p := &cleanupProcess{done: make(chan error, 1)}
			// execLauncher publishes Wait's result before closing Done. Force
			// the drain to consume that result while the channel is still open.
			p.done <- nil
			p.kill = func() error { close(p.done); return killErr }
			if err := waitOrKill(p, time.Second); err != nil {
				t.Fatal(err)
			}
			if err := terminateProcess(p, time.Second); err != nil {
				t.Fatalf("cleanup after successful drain: %v", err)
			}
		})
	}
}

func TestTerminateProcessAlreadyDoneDoesNotKill(t *testing.T) {
	if err := terminateProcess(nil, time.Second); err != nil {
		t.Fatal(err)
	}
	p := &cleanupProcess{done: make(chan error), kill: func() error {
		t.Fatal("cleanup killed a completed process")
		return nil
	}}
	close(p.done)
	if err := terminateProcess(p, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestTerminateProcessWaitsForDelayedDone(t *testing.T) {
	for name, killErr := range map[string]error{
		"killed":         nil,
		"already-waited": os.ErrProcessDone,
		"missing-group":  syscall.ESRCH,
	} {
		t.Run(name, func(t *testing.T) {
			killed := make(chan struct{})
			p := &cleanupProcess{done: make(chan error), kill: func() error { close(killed); return killErr }}
			result := make(chan error, 1)
			go func() { result <- terminateProcess(p, time.Second) }()
			<-killed
			select {
			case err := <-result:
				t.Fatalf("cleanup returned before Done: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			close(p.done)
			if err := <-result; err != nil {
				t.Fatalf("cleanup after delayed completion: %v", err)
			}
		})
	}
}

func TestSupervisorCleanupRequiresDoneAfterKill(t *testing.T) {
	for name, cleanup := range map[string]func(process, time.Duration) error{
		"terminate": terminateProcess,
		"drain":     waitOrKill,
	} {
		t.Run(name, func(t *testing.T) {
			for name, killErr := range map[string]error{
				"killed":         nil,
				"already-waited": os.ErrProcessDone,
				"missing-group":  syscall.ESRCH,
			} {
				t.Run(name, func(t *testing.T) {
					p := &cleanupProcess{done: make(chan error), kill: func() error { return killErr }}
					if err := cleanup(p, 5*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out waiting") {
						t.Fatalf("cleanup without Done = %v, want termination timeout", err)
					}
				})
			}
		})
	}
}

func TestSupervisorCleanupPreservesKillErrors(t *testing.T) {
	want := errors.New("permission denied killing child")
	for name, cleanup := range map[string]func(process, time.Duration) error{
		"terminate": terminateProcess,
		"drain":     waitOrKill,
	} {
		t.Run(name, func(t *testing.T) {
			p := &cleanupProcess{done: make(chan error)}
			// Even a concurrent completion must not mask an unrelated Kill error.
			p.kill = func() error { close(p.done); return want }
			if err := cleanup(p, 5*time.Millisecond); !errors.Is(err, want) {
				t.Fatalf("cleanup error = %v, want %v", err, want)
			}
		})
	}
}

func TestWaitOrKillPreservesDrainExitError(t *testing.T) {
	want := errors.New("daemon exited unsuccessfully")
	p := &cleanupProcess{done: make(chan error, 1), kill: func() error {
		t.Fatal("drain killed an already completed process")
		return nil
	}}
	p.done <- want
	close(p.done)
	if err := waitOrKill(p, time.Second); !errors.Is(err, want) {
		t.Fatalf("drain error = %v, want %v", err, want)
	}
}
