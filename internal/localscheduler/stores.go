package localscheduler

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/journal"
)

// ClaimStore is the scheduler-policy ownership ledger contract. ClaimLedger is
// the file implementation; a shared backend must preserve these semantics.
type ClaimStore interface {
	RecordClaimVerification(ClaimEntry, ClaimVerification) (bool, error)
	Claim(string, string, string, time.Duration) (bool, string, error)
	ClaimScoped(ClaimKey, string, string, time.Duration) (bool, string, error)
	Release(string, string, claimrelease.Reason) error
	ReleaseScoped(ClaimKey, string, claimrelease.Reason) error
	ReleaseEntry(ClaimEntry, string, claimrelease.Reason) error
	ForRunAllErr(context.Context, string) ([]ClaimEntry, error)
	SnapshotErr(context.Context) ([]ClaimEntry, error)
	HistorySnapshotErr(context.Context) ([]ClaimEntry, error)
}

// SchedulerState persists the scheduler's file-backed observational state.
type SchedulerState interface {
	ReadScheduleDemand(string) (map[WorkflowIdentity]bool, error)
	ReadTriggerEvaluations(string) (map[WorkflowIdentity]time.Time, error)
	WriteScheduleDemand(string, map[WorkflowIdentity]bool) error
	WriteTriggerEvaluations(string, map[WorkflowIdentity]time.Time) error
}

// errNoSchedulerDir refuses file-backed scheduler state for a log with no
// directory (a cloud InstanceStore). Joining "" would silently read and write
// relative to the process working directory if composition ever forgot
// WithSchedulerState.
var errNoSchedulerDir = errors.New("localscheduler: file scheduler state requires a scheduler directory; install WithSchedulerState for logs without one")

type fileSchedulerState struct{ owner *stateOwner }

// NewFileSchedulerState returns today's guarded JSON state implementation.
func NewFileSchedulerState() SchedulerState {
	return &fileSchedulerState{owner: newStateOwner()}
}

func (s *fileSchedulerState) ReadScheduleDemand(dir string) (map[WorkflowIdentity]bool, error) {
	if dir == "" {
		return nil, errNoSchedulerDir
	}
	return readScheduleDemandState(dir)
}

func (s *fileSchedulerState) ReadTriggerEvaluations(dir string) (map[WorkflowIdentity]time.Time, error) {
	if dir == "" {
		return nil, errNoSchedulerDir
	}
	return ReadTriggerEvaluations(dir)
}

func (s *fileSchedulerState) WriteScheduleDemand(dir string, state map[WorkflowIdentity]bool) error {
	if dir == "" {
		return errNoSchedulerDir
	}
	return writeScheduleDemandState(dir, s.owner, state)
}

func (s *fileSchedulerState) WriteTriggerEvaluations(dir string, state map[WorkflowIdentity]time.Time) error {
	if dir == "" {
		return errNoSchedulerDir
	}
	return writeTriggerEvaluations(dir, s.owner, state)
}

// ReconciledRun is one active run restored into scheduler policy.
type ReconciledRun struct {
	Identity  WorkflowIdentity
	Workflow  string
	Suspended bool
}

// RunInventory is the startup scan result consumed by scheduler reconciliation.
type RunInventory struct {
	Active map[WorkflowIdentity]int
	Runs   map[string]ReconciledRun
}

// RunDirReconciler inventories active run journals from complete roots or a
// caller-supplied recovery subset.
type RunDirReconciler interface {
	FromRoots(context.Context, []string) (RunInventory, error)
	FromRunDirs(context.Context, []string) (RunInventory, error)
}

type fileRunDirReconciler struct{}

// NewFileRunDirReconciler returns today's run-journal scanner.
func NewFileRunDirReconciler() RunDirReconciler { return fileRunDirReconciler{} }

func (fileRunDirReconciler) FromRoots(ctx context.Context, dirs []string) (RunInventory, error) {
	active, runs, err := activeRunsContext(ctx, dirs)
	return RunInventory{Active: active, Runs: runs}, err
}

func (fileRunDirReconciler) FromRunDirs(ctx context.Context, dirs []string) (RunInventory, error) {
	active, runs, err := activeRunsFromRunDirs(ctx, dirs)
	return RunInventory{Active: active, Runs: runs}, err
}

// SchedulerJournal is the instance-journal subset scheduler policy consumes.
type SchedulerJournal interface {
	journal.InstanceStore
}

var (
	_ ClaimStore       = (*ClaimLedger)(nil)
	_ SchedulerState   = (*fileSchedulerState)(nil)
	_ RunDirReconciler = fileRunDirReconciler{}
)

// WithSchedulerState installs a scheduler-state backend.
func WithSchedulerState(state SchedulerState) Option {
	return func(s *Scheduler) {
		if state != nil {
			s.state = state
		}
	}
}

// WithRunDirReconciler installs the startup run inventory backend.
func WithRunDirReconciler(reconciler RunDirReconciler) Option {
	return func(s *Scheduler) {
		if reconciler != nil {
			s.runDirs = reconciler
		}
	}
}
