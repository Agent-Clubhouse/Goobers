package main

import (
	"context"
	"errors"
	"sync"
)

// acquirePRRepairAdmission installs a short admission barrier before inventory
// inspection. Existing owners remain cancellable; no registry mutex spans I/O.
// Lock order is claims.lock -> this barrier -> sorted repository manager locks.
func (r *daemonRunnerRegistry) acquirePRRepairAdmission(ctx context.Context) (func(), []trackedRun, error) {
	if r == nil {
		return nil, nil, errors.New("PR repair run registry unavailable")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		r.mu.Lock()
		if r.hardStopping {
			r.mu.Unlock()
			return nil, nil, errors.New("PR repair refused during shutdown")
		}
		if len(r.childCustody) != 0 {
			r.mu.Unlock()
			return nil, nil, errors.New("PR repair refused while retained execution custody is in use")
		}
		pending := r.prRepairAdmission
		if pending == nil {
			done := make(chan struct{})
			r.prRepairAdmission = done
			owners := make([]trackedRun, 0, len(r.owners))
			for _, owner := range r.owners {
				owners = append(owners, owner)
			}
			r.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { r.mu.Lock(); r.prRepairAdmission = nil; close(done); r.mu.Unlock() }) }, owners, nil
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-pending:
		}
	}
}
