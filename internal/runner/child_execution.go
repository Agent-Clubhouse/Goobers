package runner

import (
	"errors"

	"github.com/goobers/goobers/internal/journal"
)

// ChildExecutionFactories are supplied only by an isolated host dispatcher.
// Authored code cannot fall back to execution on the daemon host. Canonical
// typed host publication may use its separately validated ceiling. The dispatcher
// owns placement, credentials, workspace transport and writer-termination proof.
type ChildExecutionFactories struct {
	NewDeterministic NewDeterministicFunc
	NewAgentic       NewAgenticFunc
}

// ForChildExecution constructs an independent driver for exactly one admitted
// child. The original runner and its ordinary automation hooks are unchanged.
// Generated output cannot invoke host provider/claim hooks outside the child's
// credential ceiling: durable child results are observed by its parent instead.
func (r *Runner) ForChildExecution(id journal.RunIdentity, factories ChildExecutionFactories) (*Runner, error) {
	if r == nil || id.Child == nil || factories.NewDeterministic == nil || factories.NewAgentic == nil {
		return nil, errors.New("runner: isolated child execution factories unavailable")
	}
	if err := id.ValidateChildLineage(); err != nil {
		return nil, err
	}
	child := *id.Child
	id.Child = &child
	cfg := r.cfg
	cfg.childExecution = &id
	cfg.NewDeterministic, cfg.NewAgentic = factories.NewDeterministic, factories.NewAgentic
	cfg.Escalation, cfg.ClaimedItems = nil, nil
	cfg.Blocked, cfg.Failed, cfg.ExistingFix = nil, nil, nil
	cfg.PrepareTerminal, cfg.FinalizeTerminal, cfg.NotifyTerminal = nil, nil, nil
	cfg.BaselineHealth, cfg.LookPathFunc = nil, nil
	cfg.ChildHandoff, cfg.ChildParentCapacity = nil, nil
	cfg.AdditionalRepos = nil
	// The supplied factories dispatch authored code remotely and separately admit
	// typed host publication; they own placement and capability enforcement.
	cfg.SelfExecutionDenied, cfg.SelfExecutionObserved = false, nil
	return New(cfg)
}

func (r *Runner) validateChildExecution(in StartInput) error {
	expected := r.cfg.childExecution
	if expected == nil {
		return nil
	}
	if in.Child == nil || *in.Child != *expected.Child || in.RunID != expected.RunID || in.Gaggle != expected.Gaggle || in.Machine == nil || in.Machine.Digest() != expected.WorkflowDigest || in.GooberDigest != expected.GooberDigest || r.cfg.ConfigGeneration != expected.ConfigGeneration {
		return errors.New("runner: isolated driver cannot execute another child or source")
	}
	if len(in.RequiredCapabilities) != 0 {
		return errors.New("runner: isolated child capabilities must be admitted by the stage dispatcher")
	}
	return nil
}
