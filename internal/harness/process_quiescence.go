package harness

import (
	"context"
	"errors"
	"sync"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/platform/proc"
)

// Only child-enabled invocations pay for strict writer acknowledgement. The
// callback completes after the owner's process wait, never at cancellation.
func workspaceWriterStop(ctx context.Context, tree *proc.Tree) (stop func() error, joined func()) {
	acknowledge := invoke.RegisterWorkspaceWriter(ctx)
	if acknowledge == nil {
		return tree.Kill, func() {}
	}
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), groupKillWaitDelay)
			defer cancel()
			err = tree.StopAndWait(cleanup)
			if err != nil {
				err = errors.Join(err, tree.Kill())
			}
		})
		return err
	}
	joined = func() { acknowledge(stop()) }
	return stop, joined
}
