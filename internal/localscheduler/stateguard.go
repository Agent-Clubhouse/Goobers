package localscheduler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// stateguard.go is the M5 guard (distributed-state-and-coordination.md §4/§9):
// trigger-evaluations.json and schedule-demand.json have historically had no
// lock and no generation check — their safety was entirely inherited from the
// up.lock single-daemon guarantee, and nothing in either file said so. The
// guard makes that dependency explicit and cheap to violate loudly: every
// scheduler stamps the files it writes with an owner token and a generation
// it claims on its first write, and a scheduler whose stamp has been
// superseded (a second daemon writing the same instance root) gets a named
// error instead of a silent lost update. This is deliberately a tripwire,
// not a coordinator — the real multi-daemon fix stays deferred with #2053.

// ErrStateSeized is the named M5 error: another daemon (a newer generation)
// has taken ownership of a scheduler state file this scheduler previously
// wrote. Match with errors.Is; the wrapping StateOwnershipError carries the
// file and both owners.
var ErrStateSeized = errors.New("localscheduler: scheduler state file is owned by another daemon (M5 guard; one daemon per instance root)")

// StateOwnershipError reports whose stamp a guarded state file carries when
// it no longer carries ours.
type StateOwnershipError struct {
	File               string
	Owner              string
	Generation         int64
	ExpectedOwner      string
	ExpectedGeneration int64
}

func (e *StateOwnershipError) Error() string {
	return fmt.Sprintf(
		"%v: %s is stamped owner %q generation %d, expected owner %q generation %d",
		ErrStateSeized, e.File, e.Owner, e.Generation, e.ExpectedOwner, e.ExpectedGeneration,
	)
}

func (e *StateOwnershipError) Unwrap() error { return ErrStateSeized }

// ownershipStamp is the owner/generation pair embedded in each guarded file.
// Legacy files (and files written by guard-less test helpers) carry the zero
// stamp, which any scheduler may claim.
type ownershipStamp struct {
	Owner      string `json:"owner,omitempty"`
	Generation int64  `json:"generation,omitempty"`
}

// stateOwner guards a scheduler's writes to the shared state files. One per
// Scheduler: the token identifies this scheduler process-instance, and the
// per-file generation is claimed lazily on the first write (construction
// order must not matter) and checked on every write after.
type stateOwner struct {
	token string

	mu      sync.Mutex
	claimed map[string]int64
}

func newStateOwner() *stateOwner {
	secret := make([]byte, 8)
	// rand.Read never fails on supported platforms (it panics internally
	// otherwise), and a guard token needs uniqueness, not secrecy.
	_, _ = rand.Read(secret)
	return &stateOwner{token: hex.EncodeToString(secret), claimed: make(map[string]int64)}
}

// stamp returns the stamp to embed in the next write of fileName under
// schedulerDir. It commits nothing: the caller must confirm the guarded write
// actually landed by calling commit with the returned stamp — claiming the
// generation before the write lands would let a transient first-write failure
// (ENOSPC, EIO) leave the in-memory claim ahead of the file and poison every
// later write with a false ErrStateSeized until restart.
//
// The first committed write claims ownership: whatever generation the file
// carries is superseded by generation+1 under this owner's token. Every later
// write verifies the on-disk stamp is still ours and fails with
// *StateOwnershipError when a second daemon has claimed the file since. A
// zero on-disk stamp is claimable by anyone — including the current owner,
// whose next write reclaims a file an operator deleted or truncated instead
// of wedging on the guard. A nil owner (guard-less test writers) writes the
// zero stamp and checks nothing.
func (o *stateOwner) stamp(schedulerDir, fileName string) (ownershipStamp, error) {
	if o == nil {
		return ownershipStamp{}, nil
	}
	current, err := readOwnershipStamp(filepath.Join(schedulerDir, fileName))
	if err != nil {
		return ownershipStamp{}, err
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	generation, held := o.claimed[fileName]
	if !held {
		return ownershipStamp{Owner: o.token, Generation: current.Generation + 1}, nil
	}
	if current == (ownershipStamp{}) {
		// The file lost its stamp under us (deleted or reset by an operator).
		// Nothing else owns it, so the current owner reclaims it rather than
		// tripping the tripwire until restart.
		return ownershipStamp{Owner: o.token, Generation: generation}, nil
	}
	if current.Owner != o.token || current.Generation != generation {
		return ownershipStamp{}, &StateOwnershipError{
			File:               fileName,
			Owner:              current.Owner,
			Generation:         current.Generation,
			ExpectedOwner:      o.token,
			ExpectedGeneration: generation,
		}
	}
	return ownershipStamp{Owner: o.token, Generation: generation}, nil
}

// commit records that the guarded write carrying stamp landed: only now does
// the claimed generation become the expectation later writes are verified
// against. A nil owner (or the nil-owner zero stamp) commits nothing.
func (o *stateOwner) commit(fileName string, stamp ownershipStamp) {
	if o == nil || stamp.Owner == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.claimed[fileName] = stamp.Generation
}

// readOwnershipStamp reads just the stamp fields from a guarded JSON state
// file. Missing and empty files carry the zero stamp; an unparsable file is
// reported rather than silently treated as claimable.
func readOwnershipStamp(path string) (ownershipStamp, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ownershipStamp{}, nil
	}
	if err != nil {
		return ownershipStamp{}, fmt.Errorf("localscheduler: read state ownership stamp: %w", err)
	}
	if len(data) == 0 {
		return ownershipStamp{}, nil
	}
	var stamp ownershipStamp
	if err := json.Unmarshal(data, &stamp); err != nil {
		return ownershipStamp{}, fmt.Errorf("localscheduler: decode state ownership stamp %q: %w", path, err)
	}
	return stamp, nil
}
