package invoke

import (
	"context"
	"errors"
	"sync"
)

// ErrWorkspaceNotQuiescent prevents snapshot capture without acknowledged
// termination of every process writer registered by the actual runtime.
var ErrWorkspaceNotQuiescent = errors.New("invoke: workspace writers have not acknowledged termination")

type workspaceQuiescenceKey struct{}

// WorkspaceQuiescence is host-owned evidence, never an envelope field or model
// assertion. A runtime that never registers its writers cannot satisfy it.
type WorkspaceQuiescence struct {
	mu       sync.Mutex
	started  int
	active   int
	failures error
	parent   *WorkspaceQuiescence
}

// WithWorkspaceQuiescence tracks writers for one child-enabled invocation.
func WithWorkspaceQuiescence(ctx context.Context) (context.Context, *WorkspaceQuiescence) {
	parent, _ := ctx.Value(workspaceQuiescenceKey{}).(*WorkspaceQuiescence)
	state := &WorkspaceQuiescence{parent: parent}
	return context.WithValue(ctx, workspaceQuiescenceKey{}, state), state
}

// RegisterWorkspaceWriter is called by an actual process owner after launch.
// Its completion callback requires stop/join evidence, not context cancellation.
// Nil means no handoff was requested and preserves ordinary runtime behavior.
func RegisterWorkspaceWriter(ctx context.Context) func(error) {
	state, _ := ctx.Value(workspaceQuiescenceKey{}).(*WorkspaceQuiescence)
	if state == nil {
		return nil
	}
	// A stage may require its own proof inside a run-wide revocable credential
	// lease. Replacing the local tracker must never hide that stage's writers
	// or failed acknowledgements from the enclosing lease owner.
	for current := state; current != nil; current = current.parent {
		current.mu.Lock()
		current.started++
		current.active++
		current.mu.Unlock()
	}
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			for current := state; current != nil; current = current.parent {
				current.mu.Lock()
				current.active--
				current.failures = errors.Join(current.failures, err)
				current.mu.Unlock()
			}
		})
	}
}

// Verify refuses absent, active or failed writer acknowledgments.
func (state *WorkspaceQuiescence) Verify() error {
	if state == nil {
		return ErrWorkspaceNotQuiescent
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.started == 0 || state.active != 0 || state.failures != nil {
		return errors.Join(ErrWorkspaceNotQuiescent, state.failures)
	}
	return nil
}

// VerifyIdle checks all registered processes without requiring a process to have
// started. A human execution can fail during preparation before launching any.
func (state *WorkspaceQuiescence) VerifyIdle() error {
	if state == nil {
		return ErrWorkspaceNotQuiescent
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.active != 0 || state.failures != nil {
		return errors.Join(ErrWorkspaceNotQuiescent, state.failures)
	}
	return nil
}
