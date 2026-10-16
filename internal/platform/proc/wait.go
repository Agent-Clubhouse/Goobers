package proc

import (
	"context"
	"errors"
	"time"
)

// WaitOptions controls process-tree cleanup after ctx ends.
type WaitOptions struct {
	KillWait time.Duration
	// StopTree replaces Kill when the owner must retain termination evidence
	// before descendants can be orphaned. Nil preserves ordinary cleanup.
	StopTree func() error
	// BeforeKill may request diagnostics before the tree is force-killed. If
	// it consumes wait, it reports waited so WaitOrKill does not wait twice.
	BeforeKill func(timedOut bool) (waited bool, err error)
}

// WaitOutcome describes how process waiting ended.
type WaitOutcome struct {
	Err      error
	TimedOut bool
	Canceled bool
	GaveUp   bool
}

// WaitOrKill waits for a process to exit or kills its tree when ctx ends.
func WaitOrKill(ctx context.Context, tree *Tree, wait <-chan error, opts WaitOptions) WaitOutcome {
	select {
	case err := <-wait:
		return WaitOutcome{Err: err}
	case <-ctx.Done():
	}

	outcome := WaitOutcome{}
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		outcome.TimedOut = true
	} else {
		outcome.Canceled = true
	}

	waited := false
	if opts.BeforeKill != nil {
		waited, outcome.Err = opts.BeforeKill(outcome.TimedOut)
	}
	stop := opts.StopTree
	if stop == nil {
		stop = tree.Kill
	}
	_ = stop()
	if waited {
		return outcome
	}

	timer := time.NewTimer(opts.KillWait)
	defer timer.Stop()
	select {
	case outcome.Err = <-wait:
	case <-timer.C:
		outcome.GaveUp = true
	}
	return outcome
}
