// Package claimsclient is the claim-ledger primitive set every ledger-touching
// goobers CLI stage calls, behind one interface with two backends
// (decision 005 ruling 2; finding 002 "plane clients" C1):
//
//   - File: the instance's own claims.json under the cross-process claims
//     lock — the type-1/type-2 same-host path and the daemon's own scheduler,
//     byte-for-byte the discipline cmd/goobers used before this package
//     existed (the lock and the ledger options are injected by the caller,
//     so nothing about how the daemon serializes or journals changes).
//   - HTTP: the daemon's claims plane (/api/v1/claims/*), selected when
//     GOOBERS_CLAIMS_ENDPOINT and a claims bearer are present in the stage's
//     environment — the path a stage POD takes, where the pod's filesystem
//     has no claims.json and the daemon is the single writer (DS1).
//
// The split the design doc names (DS1 wording, decision 005 R2): item
// SELECTION — which candidates to try, in what order — belongs to the stage
// and stays in cmd/goobers; ADMISSION — whether a lease is granted — belongs
// to the ledger, and acquire's refusal is the only arbiter of contention.
// This package moves the primitive's transport, not its semantics: a stage
// that did select-then-acquire under one flock does select-then-acquire over
// N round trips, and two concurrent pods selecting the same item are settled
// by acquire exactly as two subprocesses were.
package claimsclient

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
)

// Key identifies one provider item within a gaggle — the ledger's own
// vocabulary. A Key with empty Gaggle and Provider addresses a legacy,
// unscoped (pre-GAG-011) ledger entry by item ID; only the file backend can
// address those (the plane requires a namespace), which is the one place
// the two backends deliberately differ.
type Key = localscheduler.ClaimKey

// Entry is one lease in the ledger, live or released.
type Entry = localscheduler.ClaimEntry

// Environment variables that select the HTTP backend in a stage process.
const (
	// EnvEndpoint is the daemon API base URL the claims plane is reached at.
	// Set by the dispatcher into goobers-CLI stage pods only.
	EnvEndpoint = "GOOBERS_CLAIMS_ENDPOINT"
	// EnvToken is the claims-scoped bearer presented to the plane. Never the
	// pod's surrender token: a CLI subprocess must not be able to author its
	// own outcome (decision 005 R2).
	EnvToken = "GOOBERS_CLAIMS_TOKEN"
	// EnvRunID is the run the stage acts for — the plane's containment key.
	EnvRunID = "GOOBERS_RUN_ID"
)

// ErrEndpointWithoutToken is the fail-closed refusal Select answers when the
// endpoint is configured but no bearer is: a stage in a pod must never fall
// back to a filesystem ledger it does not have.
var ErrEndpointWithoutToken = errors.New("claimsclient: " + EnvEndpoint + " is set but " + EnvToken + " is empty; refusing to fall back to a file ledger")

// ErrEndpointWithoutRun is the sibling refusal for a missing run identity.
var ErrEndpointWithoutRun = errors.New("claimsclient: " + EnvEndpoint + " is set but " + EnvRunID + " is empty; the claims plane contains every call to the stage's own run")

// ErrLegacyKeyOverPlane reports a legacy (unscoped) key reaching the HTTP
// backend, which cannot address it.
var ErrLegacyKeyOverPlane = errors.New("claimsclient: legacy unscoped claim keys are not addressable over the claims plane")

// Listing is a namespace read of the ledger: its current holders and the
// retained released history, in the ledger's own orders (entries by item ID;
// history newest first).
type Listing struct {
	Entries []Entry
	History []Entry
}

// Lookup returns the current entry for key, if any live or expired claim
// exists — ClaimLedger.Lookup/LookupScoped's contract, so callers wanting
// only live claims check ExpiresAt themselves, exactly as they did against
// the ledger. A legacy key (empty namespace) matches a legacy entry by item
// ID; a scoped key matches on the full namespace.
func (l Listing) Lookup(key Key) (Entry, bool) {
	for _, entry := range l.Entries {
		if key.Gaggle == "" && key.Provider == "" {
			if entry.Gaggle == "" && entry.Provider == "" && entry.ItemID == key.ExternalID {
				return entry, true
			}
			continue
		}
		if entry.Gaggle == key.Gaggle && entry.Provider == key.Provider && entry.ExternalID == key.ExternalID {
			return entry, true
		}
	}
	return Entry{}, false
}

// HistoryForItem returns the retained claim attempts for itemID, released
// attempts included, newest first — ClaimLedger.HistoryForItem's contract
// over the listing's namespace.
func (l Listing) HistoryForItem(itemID string) []Entry {
	var entries []Entry
	for _, entry := range l.History {
		if entry.ItemID == itemID || entry.ExternalID == itemID {
			entries = append(entries, entry)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return historyTime(entries[i]).After(historyTime(entries[j]))
	})
	return entries
}

func historyTime(entry Entry) time.Time {
	if entry.ReleasedAt != nil {
		return *entry.ReleasedAt
	}
	return entry.ClaimedAt
}

// KeyForEntry is the key that addresses entry: scoped when the entry carries
// a namespace, legacy otherwise — ClaimLedger.ReleaseEntry's dispatch, so a
// caller iterating ForRunAll's results does not reconstruct which path an
// entry came from.
func KeyForEntry(entry Entry) Key {
	if entry.Gaggle == "" || entry.Provider == "" {
		return Key{ExternalID: entry.ItemID}
	}
	return Key{Gaggle: entry.Gaggle, Provider: entry.Provider, ExternalID: entry.ExternalID}
}

// InNamespace reports whether entry belongs to the gaggle/provider namespace.
// A legacy unscoped entry belongs to every namespace: the ledger holds it
// exclusive against every scoped claimant (ClaimLedger.claim), so a lister
// that hid it would select an item acquire is going to refuse.
func InNamespace(entry Entry, gaggle, provider string) bool {
	if entry.Gaggle == "" && entry.Provider == "" {
		return true
	}
	return entry.Gaggle == gaggle && entry.Provider == provider
}

// MergeLockItemPrefix namespaces the synthetic ledger item a merge lease is
// taken on, so it can never collide with a work item or a PR claim.
const MergeLockItemPrefix = "merge-lock/"

// MergeLockKey is the synthetic claim key merge-pr's exclusive window is
// leased on over the plane: one lease per repository, held by the run for
// the poll->decide->merge window (#719), expiring on its own if the holder
// crashes instead of leaking a flock.
func MergeLockKey(gaggle, provider, owner, repo string) Key {
	return Key{Gaggle: gaggle, Provider: provider, ExternalID: MergeLockItemPrefix + owner + "/" + repo}
}

// MergeLock names one merge-pr exclusive window.
type MergeLock struct {
	// Key is MergeLockKey's synthetic item (HTTP backend; the file backend
	// holds the instance-wide merge.lock flock regardless of key).
	Key Key
	// RunID and Workflow identify the holder to the ledger.
	RunID, Workflow string
}

// Ledger is the claim-ledger primitive set (finding 002 C1). Every method
// is one ledger operation; Locked scopes a critical section.
type Ledger interface {
	// ClaimScoped acquires key for runID under workflow for lease. ok=false
	// with the holder's run id is a refusal, not an error; an idempotent
	// re-claim by the same run renews — ClaimLedger.ClaimScoped's contract.
	ClaimScoped(ctx context.Context, key Key, runID, workflow string, lease time.Duration) (ok bool, holder string, err error)
	// ReleaseScoped surrenders key if runID holds it; releasing a claim not
	// held is a no-op — the ledger's idempotency contract.
	ReleaseScoped(ctx context.Context, key Key, runID string) error
	// ReleaseAllForRun surrenders every claim runID holds and reports them.
	ReleaseAllForRun(ctx context.Context, runID string) ([]Entry, error)
	// ForRunAll returns every entry runID currently holds, ordered by item ID.
	ForRunAll(ctx context.Context, runID string) ([]Entry, error)
	// ListNamespace reads the gaggle/provider namespace: current holders
	// (legacy unscoped entries included, see InNamespace) and released
	// history.
	ListNamespace(ctx context.Context, gaggle, provider string) (Listing, error)
	// MergeLock runs fn inside merge-pr's exclusive window: the instance-wide
	// merge flock on the file backend, a polled lease on the plane.
	MergeLock(ctx context.Context, lock MergeLock, fn func() error) error
	// Locked runs fn as one critical section labelled operation. On the file
	// backend that is one cross-process claims-lock acquisition and one
	// fresh ledger open for every primitive fn calls — today's
	// withClaimLock(...){ OpenClaimLedger(...) ... } shape, byte for byte. On
	// the plane there is no client-side lock: each primitive is its own
	// round trip and the daemon serializes (DS1), so fn simply runs.
	Locked(ctx context.Context, operation string, fn func(Ledger) error) error
}

// Contained is implemented by a backend that can act as exactly one run —
// the plane, whose every call is contained to the bearer's run
// (principal.Subject == run:<id>). A seam that synthesizes a claimant
// identity today (backlog-reconcile's per-reservation run id) asks for this
// and, when present, reserves under the contained run instead.
type Contained interface {
	ContainedRunID() string
}

// Select chooses the backend for a stage process from its environment: the
// plane when EnvEndpoint is set (fail closed on a missing bearer or run
// identity — never a silent fall-through to a ledger file the pod does not
// have), else the file backend the caller constructs.
func Select(getenv func(string) string, file func() (Ledger, error)) (Ledger, error) {
	endpoint := strings.TrimSpace(getenv(EnvEndpoint))
	if endpoint == "" {
		return file()
	}
	token := strings.TrimSpace(getenv(EnvToken))
	if token == "" {
		return nil, ErrEndpointWithoutToken
	}
	runID := strings.TrimSpace(getenv(EnvRunID))
	if runID == "" {
		return nil, ErrEndpointWithoutRun
	}
	return NewHTTP(HTTPConfig{BaseURL: endpoint, Token: token, RunID: runID})
}

// leaseSeconds converts a lease to the plane's integer seconds, rounded up
// so a sub-second lease never becomes the zero the server reads as "use the
// default", and clamped to the plane's ceiling: a longer lease is refused by
// the server outright, while a clamped one is kept alive by the daemon's
// renewal ticker (DS6) for as long as the holding run is live.
func leaseSeconds(lease time.Duration) (int, error) {
	if lease <= 0 {
		return 0, fmt.Errorf("claimsclient: lease duration must be positive, got %s", lease)
	}
	seconds := int((lease + time.Second - 1) / time.Second)
	if seconds > MaxLeaseSeconds {
		seconds = MaxLeaseSeconds
	}
	return seconds, nil
}

// MaxLeaseSeconds restates the claims plane's lease ceiling
// (httpapi.MaxClaimLeaseSeconds); pinned against it by the server's tests.
const MaxLeaseSeconds = 4 * 30 * 60
