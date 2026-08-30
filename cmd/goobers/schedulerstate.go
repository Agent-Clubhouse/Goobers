package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
)

// schedulerstate.go is where every CLI stage that touches gaggle-scoped
// scheduler state gets its stateclient.Store, and where the daemon serves the
// other side of the same seam (decision 005 ruling R3, finding 002 plan step
// C2).
//
// Two constructions, mirroring claimledger.go's:
//
//   - stageStateStore: the STAGE seam. The plane backend when
//     GOOBERS_STATE_ENDPOINT (+ the state bearer and the gaggle) is in the
//     stage's environment — a stage pod — else the file backend below. Fail
//     closed in between: an endpoint without a bearer is an error, never a
//     silent fall through to a scheduler-state file the pod does not have.
//   - fileStateStore / heldStateStore: the instance's own scheduler-state
//     files under each key's EXISTING cross-process lock. The daemon's own
//     paths (its scheduler, and the plane service below) use these directly
//     and never select by environment — the daemon is the writer, not a
//     client.
//
// Nothing about the file path changes for a type-1/type-2 instance. The lock
// each key takes is the lock it already took: claims.lock for blocked.json and
// the backlog scan cursors (blockedrecords.go, backlogquery.go),
// post-merge-reconcile.lock for the reconcile ledger, and
// sibling-context-cache.lock for the gather memo. That is what makes a
// plane-served write and an in-process write ONE atomicity domain: the daemon
// serving a pod's compare-and-swap for blocked.json blocks on the very flock a
// runner-driven backlog-query holds, and vice versa.

// Scheduler-state lock operation labels, alongside providercmd.go's.
const (
	// stateLockOperationPostMergeUpdate and stateLockOperationSiblingUpdate
	// label the two keys that do NOT ride claims.lock, so a slow-lock event
	// names the section it was waiting on.
	stateLockOperationPostMergeUpdate = "post-merge-reconcile.update"
	stateLockOperationSiblingUpdate   = "sibling-context-cache.update"
	// backlogHealthCursorLockFile guards the backlog-health ready-transition
	// ledger. It is a NEW lock file, and deliberately not claims.lock.
	//
	// The ledger had no cross-process lock at all before the plane (#3392's
	// writer is one atomic write-then-rename by one in-process caller), so
	// there is no existing discipline to preserve — but there is now a reason
	// to have one: over the plane the compare and the swap are separate round
	// trips into the daemon, and only a lock the daemon holds across both
	// makes the CAS a compare-and-swap rather than two racing writes. Its own
	// file, because folding it into claims.lock would put a 400-page provider
	// walk's persist step into contention with every claim, release and
	// blocked-record update on the instance — the ledger is not in the
	// claiming path's atomicity domain and must not join it.
	backlogHealthCursorLockFile = "backlog-health-cursor.lock"
	// stateLockOperationBacklogHealthCursor labels that section.
	stateLockOperationBacklogHealthCursor = "backlog-health-cursor.update"
)

// schedulerStateLock maps a scheduler-state key onto the cross-process lock
// that key already used before the plane existed. This mapping is the single
// definition of "which lock guards which key" and is shared by the CLI's file
// backend and the daemon's plane service, so the two can never end up in
// different lock domains for the same file.
func schedulerStateLock(l instance.Layout) func(key, operation string, fn func() error) error {
	schedulerDir := l.SchedulerDir()
	return func(key, operation string, fn func() error) error {
		switch {
		case key == stateclient.KeyPostMergeReconcileLedger:
			return withFileLock(filepath.Join(schedulerDir, postMergeReconcileLockFile), fn)
		case key == stateclient.KeySiblingContextCache:
			return withFileLock(filepath.Join(schedulerDir, siblingCacheLockFileName), fn)
		case strings.HasPrefix(key, stateclient.BacklogHealthCursorKeyPrefix):
			return withBoundedFileLock(
				filepath.Join(schedulerDir, backlogHealthCursorLockFile), operation, fn)
		default:
			// blocked.json and every backlog-scan-<hash>.json cursor: the
			// claims lock, which is what the in-process readers and writers
			// have always taken (blockedrecords.go's updateBlockedRecords,
			// backlogquery.go's advanceBacklogScanCursor). Serving the plane
			// under any other lock would split the atomicity domain in two and
			// lose exactly the updates the CAS is there to protect.
			return withClaimLock(filepath.Join(schedulerDir, claimLockFileName), operation, fn)
		}
	}
}

// fileStateStore is the instance's scheduler state under each key's own lock.
func fileStateStore(l instance.Layout) (stateclient.Store, error) {
	return stateclient.NewFile(stateclient.FileConfig{
		Dir:  l.SchedulerDir(),
		Lock: schedulerStateLock(l),
	})
}

// heldStateStore is the instance's scheduler state for a caller ALREADY inside
// the key's own lock: no lock is taken (a second flock on the held path would
// wait on itself until the timeout), the file is read and written directly.
func heldStateStore(l instance.Layout) (stateclient.Store, error) {
	return stateclient.NewFile(stateclient.FileConfig{Dir: l.SchedulerDir()})
}

// openStageStateStore is the stage seam, swappable in tests.
var openStageStateStore = stageStateStore

// stageStateStore selects a scheduler-state store for a stage CLI process from
// its environment.
func stageStateStore(l instance.Layout) (stateclient.Store, error) {
	return stateclient.Select(os.Getenv, func() (stateclient.Store, error) {
		return fileStateStore(l)
	})
}

// statePlaneSelected reports whether the stage's environment names the
// scheduler-state plane — the same predicate Select applies, read here so a
// seam can skip instance-root side work (a scheduler-directory create) the
// plane owns.
func statePlaneSelected() bool { return stateclient.Selected(os.Getenv) }

// stateContext is the context a CLI seam hands the store when it has none of
// its own: the plane backend bounds each round trip itself, and the file
// backend ignores it.
func stateContext() context.Context { return context.Background() }

// daemonStateService is the scheduler-state plane over the daemon's own files.
// It is not a second store: every operation goes through the SAME
// stateclient.File the local CLI seam builds, with the SAME per-key lock, so
// an API caller and a subprocess caller share one atomicity domain rather than
// two (DS1/DS3 — API contract first, the file stays the store).
type daemonStateService struct {
	layout instance.Layout
	// store is the file backend; nil is never valid (newDaemonStateService
	// fails instead), so a request can never silently reach an unlocked path.
	store stateclient.Store
}

func newDaemonStateService(layout instance.Layout) (*daemonStateService, error) {
	store, err := fileStateStore(layout)
	if err != nil {
		return nil, err
	}
	return &daemonStateService{layout: layout, store: store}, nil
}

// authorize is the plane's containment. A pod principal may address ONLY the
// gaggle its own run belongs to: the run's own run.yaml on this instance is
// the authority on which gaggle that is, the same lookup the claims plane's
// listing makes. Fail closed — an unverifiable gaggle is not a gaggle the run
// belongs to, and a gaggle that is not one plain path element is refused
// outright rather than resolved.
//
// The key is checked against the gaggle TOO, not only against the namespace.
// For every key whose name is gaggle-agnostic the route's own scope is the
// whole of the containment, but the backlog-health ready-transition ledger
// carries its gaggle IN the key (Goobers#3948) — so without this a pod
// contained to gaggle A could name gaggle B's cursor and the route would serve
// it under A's scope. Applied to every principal, not only pod ones: the check
// costs nothing and a rule that holds only for the principals we remembered to
// enumerate is not containment.
func (s *daemonStateService) authorize(request httpapi.StateGetRequest) error {
	if !stateclient.ValidKey(request.Key) {
		return httpapi.NewInterventionError(http.StatusBadRequest, "invalid_state_key",
			"key is not a scheduler-state key", nil)
	}
	if !stateclient.BacklogHealthCursorKeyContained(
		request.Key, instance.SchedulerNameSegment(request.Gaggle)) {
		return httpapi.NewInterventionError(http.StatusForbidden, "gaggle_mismatch",
			"scheduler-state key names a different gaggle than the route's", nil)
	}
	if !request.PodScoped {
		return nil
	}
	if !runBelongsToGaggle(s.layout, request.Gaggle, request.RunID) {
		return httpapi.NewInterventionError(http.StatusForbidden, "gaggle_mismatch",
			"pod principal may only read and write the scheduler state of the gaggle its own run belongs to", nil)
	}
	return nil
}

// GetState implements httpapi.StateService.
func (s *daemonStateService) GetState(ctx context.Context, request httpapi.StateGetRequest) (httpapi.StateValue, error) {
	if err := s.authorize(request); err != nil {
		return httpapi.StateValue{}, err
	}
	value, err := s.store.Get(ctx, request.Key)
	if err != nil {
		return httpapi.StateValue{}, statePlaneServiceError(err)
	}
	if !value.Exists() {
		return httpapi.StateValue{Found: false}, nil
	}
	return httpapi.StateValue{Data: value.Data, ETag: value.ETag, Found: true}, nil
}

// PutState implements httpapi.StateService: the compare-and-swap, served under
// the key's own lock so the compare and the swap are one operation against
// every other writer — including the daemon's own scheduler.
func (s *daemonStateService) PutState(ctx context.Context, request httpapi.StatePutRequest) (httpapi.StateValue, error) {
	if err := s.authorize(request.StateGetRequest); err != nil {
		return httpapi.StateValue{}, err
	}
	value, err := s.store.Put(ctx, request.Key, request.Data, request.IfMatch)
	if err != nil {
		return httpapi.StateValue{}, statePlaneServiceError(err)
	}
	return httpapi.StateValue{Data: value.Data, ETag: value.ETag, Found: true}, nil
}

// statePlaneServiceError maps the store's failures onto the plane's
// vocabulary: a lost compare-and-swap is the route's 412, an invalid key a
// 400, everything else a server fault.
func statePlaneServiceError(err error) error {
	switch {
	case errors.Is(err, stateclient.ErrPreconditionFailed):
		return httpapi.ErrStatePrecondition
	case errors.Is(err, stateclient.ErrInvalidKey):
		return httpapi.NewInterventionError(http.StatusBadRequest, "invalid_state_key", err.Error(), err)
	default:
		return err
	}
}

// runBelongsToGaggle reports whether runID's journal lives under gaggle's runs
// directory on this instance — the run.yaml the daemon's live writer creates
// at the run's first emit, which every pod plane call follows.
//
// Both segments are validated before they are joined into a path: this is a
// containment check whose only inputs are a pod's request, so a gaggle that is
// not a single plain path element (".", "..", anything carrying a separator)
// is refused outright rather than resolved. Fail closed — an unverifiable
// gaggle is not a gaggle the run belongs to.
func runBelongsToGaggle(layout instance.Layout, gaggle, runID string) bool {
	if !apiv1.ValidRunID(runID) || !plainPathElement(gaggle) {
		return false
	}
	_, err := os.Stat(filepath.Join(layout.ForGaggle(gaggle).RunsDir(), runID, "run.yaml"))
	return err == nil
}

// heldStageStateStore is the stage seam for a caller ALREADY inside the key's
// cross-process lock — the blocked-record reconcile that runs inside the claim
// ledger's locked section. On the plane it is the same HTTP store as
// stageStateStore (the daemon takes the lock server-side, so the caller holds
// nothing locally); locally it is the no-lock file store, because taking
// claims.lock a second time from inside claims.lock waits on itself until the
// timeout.
func heldStageStateStore(l instance.Layout) (stateclient.Store, error) {
	return stateclient.Select(os.Getenv, func() (stateclient.Store, error) {
		return heldStateStore(l)
	})
}

// openHeldStageStateStore is the held-stage seam, swappable in tests.
var openHeldStageStateStore = heldStageStateStore
