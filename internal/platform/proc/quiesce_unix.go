//go:build unix

package proc

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

// StopAndWait terminates the owned tree and verifies all captured identities
// have exited. The owner must call this before its normal Kill path can orphan
// descendants. Unknown process identity or an unsupported host fails closed.
func (t *Tree) StopAndWait(ctx context.Context) error {
	if !startTimeSupported {
		return fmt.Errorf("workspace quiescence is unsupported on this platform: %w", ErrQuiescenceUnobservable)
	}
	pids, err := quiescencePIDs(t.pid)
	if err != nil {
		return errors.Join(ErrQuiescenceUnobservable, err)
	}
	pids = append(pids, t.pid)
	identities := make([]processIdentity, 0, len(pids)+len(t.descendants))
	identities = append(identities, t.descendants...)
	for _, pid := range pids {
		started, ok := StartTime(pid)
		if !ok {
			if Alive(pid) {
				return fmt.Errorf("cannot verify workspace writer identity: %w", ErrQuiescenceUnobservable)
			}
			continue
		}
		identities = append(identities, processIdentity{pid: pid, startTime: started})
	}
	t.descendants = append(t.descendants, identities...)
	if err := t.kill(); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		waiting := false
		for _, process := range identities {
			if !Alive(process.pid) {
				continue
			}
			started, ok := StartTime(process.pid)
			if !ok {
				return fmt.Errorf("workspace writer termination could not be verified: %w", ErrQuiescenceUnobservable)
			}
			waiting = waiting || started.Equal(process.startTime)
		}
		if !waiting {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("workspace writers did not stop: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
