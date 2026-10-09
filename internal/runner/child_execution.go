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
	// VerifyTerminalCustody refuses terminalization while a physical attempt
	// remains unresolved, including an ownerless watchdog recovery path.
	VerifyTerminalCustody func(*journal.Run) error
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
	cfg.childTerminalCustody = factories.VerifyTerminalCustody
	cfg.NewDeterministic, cfg.NewAgentic = factories.NewDeterministic, factories.NewAgentic
	cfg.Escalation, cfg.ClaimedItems = nil, nil
	cfg.Blocked, cfg.Failed, cfg.ExistingFix = nil, nil, nil
	cfg.PrepareTerminal, cfg.FinalizeTerminal, cfg.NotifyTerminal = nil, nil, nil
	cfg.BaselineHealth, cfg.LookPathFunc = nil, nil
	cfg.ChildHandoff, cfg.ChildParentCapacity = nil, nil
	cfg.BorrowParentJournal = nil
	cfg.AdditionalRepos = nil
	// The supplied factories exclusively dispatch remote attempts; their pinned
	// placement admission owns host capability and self-deny enforcement.
	cfg.SelfExecutionDenied, cfg.SelfExecutionObserved = false, nil
	return New(cfg)
}

func (r *Runner) borrowExecutionJournal(jr *journal.Run) (func(), error) {
	if r.cfg.childExecution == nil {
		return r.borrowParentJournal(jr)
	}
	if r.cfg.childBorrowJournal == nil {
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

// parkChildTaskDispatch leaves this exact attempt open for the original
// worker's final observations. Stop the local heartbeat and release local
// leases while retaining the workspace; no retry or terminal outcome is written.
func parkChildTaskDispatch(tf taskFrame, heartbeat stageHeartbeat, attempt int, class journal.AttemptClass, mutations []mutationFact, cleanup func(bool) error) error {
	heartbeatErr := finishTaskDispatch(tf.jr, heartbeat, tf.t.Name, attempt, class, mutations, nil)
	var cleanupErr error
	if cleanup != nil {
		cleanupErr = cleanup(true)
	}
	return errors.Join(heartbeatErr, cleanupErr)
}
