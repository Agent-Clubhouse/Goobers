package runner

import (
	"errors"

	"github.com/goobers/goobers/internal/journal"
)

// ChildExecutionFactories are supplied only by an isolated host dispatcher.
// Neither factory may fall back to execution on the daemon host. The dispatcher
// owns placement, credentials, workspace transport and writer-termination proof.
type ChildExecutionFactories struct {
	NewDeterministic NewDeterministicFunc
	NewAgentic       NewAgenticFunc
	// BorrowJournal lends the driver's existing handle to remote observation
	// owners for the complete run, including gaps between stage attempts.
	// Release must wait for in-flight emits before the driver closes its handle.
	BorrowJournal func(runID, gaggle string, jr *journal.Run) (func(), error)
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
	cfg.childBorrowJournal = factories.BorrowJournal
	cfg.NewDeterministic, cfg.NewAgentic = factories.NewDeterministic, factories.NewAgentic
	cfg.Escalation, cfg.ClaimedItems = nil, nil
	cfg.Blocked, cfg.Failed, cfg.ExistingFix = nil, nil, nil
	cfg.PrepareTerminal, cfg.FinalizeTerminal, cfg.NotifyTerminal = nil, nil, nil
	cfg.BaselineHealth, cfg.LookPathFunc = nil, nil
	cfg.ChildHandoff, cfg.ChildParentCapacity = nil, nil
	cfg.AdditionalRepos = nil
	// The supplied factories exclusively dispatch remote attempts; their pinned
	// placement admission owns host capability and self-deny enforcement.
	cfg.SelfExecutionDenied, cfg.SelfExecutionObserved = false, nil
	return New(cfg)
}

func (r *Runner) borrowChildJournal(jr *journal.Run) (func(), error) {
	if r.cfg.childExecution == nil || r.cfg.childBorrowJournal == nil {
		return func() {}, nil
	}
	id := r.cfg.childExecution
	release, err := r.cfg.childBorrowJournal(id.RunID, id.Gaggle, jr)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, errors.New("runner: child journal owner omitted release")
	}
	return release, nil
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
