package invoke

import (
	"context"
	"errors"
	"sync"
	"time"
)

// WorkspaceProcess is supplied by the actual process owner after successful
// launch. StopAndWait must observe termination of the entire owned tree.
type WorkspaceProcess interface {
	StopAndWait(context.Context) error
	Kill() error
}

// TrackWorkspaceProcess gives both cancellation and normal return the same
// bounded stop/join operation. Ordinary invocations preserve their existing
// kill behavior; strict custody scopes cannot acknowledge an unobserved tree.
func TrackWorkspaceProcess(ctx context.Context, process WorkspaceProcess, timeout time.Duration) (stop func() error, joined func()) {
	acknowledge := RegisterWorkspaceWriter(ctx)
	if acknowledge == nil {
		return process.Kill, func() {}
	}
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			defer cancel()
			err = process.StopAndWait(cleanup)
			if err != nil {
				err = errors.Join(err, process.Kill())
			}
		})
		return err
	}
	joined = func() { acknowledge(stop()) }
	return stop, joined
}
