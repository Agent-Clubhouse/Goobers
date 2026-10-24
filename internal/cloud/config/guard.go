package config

import (
	"fmt"
	"sync/atomic"
)

// Guard is the scheduler-side defence in depth. It is bound to one immutable
// Generation and denies anything that generation did not assess as eligible,
// including workflows it has never seen. Replace swaps the whole generation
// atomically, so a reader sees the old generation or the new one, never a mix.
type Guard struct {
	current atomic.Pointer[Generation]
}

// NewGuard returns a guard that denies everything until a generation is bound.
func NewGuard() *Guard { return &Guard{} }

// Replace binds the guard to gen. A nil generation unbinds it (deny all).
func (g *Guard) Replace(gen *Generation) { g.current.Store(gen) }

// Admit returns nil only when the guard is bound to the generation named by
// digest and that generation assessed gaggle/name as eligible. A stale or
// unknown digest, an unassessed workflow and an ineligible workflow all deny.
func (g *Guard) Admit(digest, gaggle, name string) error {
	gen := g.current.Load()
	switch {
	case gen == nil:
		return fmt.Errorf("%w: no configuration generation is bound", ErrIneligible)
	case gen.digest != digest:
		return fmt.Errorf("%w: generation %q is not the bound generation %q", ErrIneligible, digest, gen.digest)
	}
	return gen.admits(gaggle, name)
}

// admits is the one eligibility check: nil only when this generation assessed
// gaggle/name eligible. Guard.Admit and Configuration.AdmitApplied both end here,
// so the scheduler guard and the trigger plane cannot disagree.
func (g *Generation) admits(gaggle, name string) error {
	if _, ok := g.eligible[workflowKey(gaggle, name)]; ok {
		return nil
	}
	return fmt.Errorf("%w: workflow %s was not assessed eligible in generation %q",
		ErrIneligible, workflowKey(gaggle, name), g.digest)
}
