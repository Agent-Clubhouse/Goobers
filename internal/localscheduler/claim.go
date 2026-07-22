package localscheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

const forceReleaseActorCLI = "cli"

// ClaimKey identifies one provider item within a gaggle.
type ClaimKey struct {
	Gaggle     string
	Provider   string
	ExternalID string
}

// ClaimNamespace identifies the gaggle and provider that own a legacy claim.
type ClaimNamespace struct {
	Gaggle   string
	Provider string
}

// ErrLegacyClaimOwnershipUnresolved tells migration to retain a legacy claim
// unchanged until a later startup can resolve it or its lease expires.
var ErrLegacyClaimOwnershipUnresolved = errors.New("legacy claim ownership unresolved")

func (k ClaimKey) storageKey() (string, error) {
	if k.Gaggle == "" || k.Provider == "" || k.ExternalID == "" {
		return "", fmt.Errorf("localscheduler: claim key requires gaggle, provider, and external ID")
	}
	return "v2|" + url.QueryEscape(k.Gaggle) + "|" + url.QueryEscape(k.Provider) + "|" + url.QueryEscape(k.ExternalID), nil
}

// ClaimEntry is one lease in the claim ledger.
type ClaimEntry struct {
	ItemID     string    `json:"itemId"`
	Gaggle     string    `json:"gaggle,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	ExternalID string    `json:"externalId,omitempty"`
	RunID      string    `json:"runId"`
	Workflow   string    `json:"workflow"`
	ClaimedAt  time.Time `json:"claimedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// expired reports whether the lease is no longer live at now.
func (e ClaimEntry) expired(now time.Time) bool { return !e.ExpiresAt.After(now) }

// ClaimLedger is the authoritative, atomic, lease-based source of truth for
// exactly-once backlog-item processing (SCH-020/BL-005). A provider-visible
// marker (#12's providers.ClaimWorkItem) mirrors this ledger for human
// visibility once a local claim succeeds — the ledger never depends on the
// provider layer, and the marker is never the source of truth (§7, SCH-Q5).
//
// Durable state lives in a single JSON file under the instance root, rewritten
// atomically (journal.WriteFileAtomic) on every mutation — sized for V0's scale
// (concurrently-claimed backlog items, not a database's worth of rows). It is
// designed for one embedded scheduler per instance (SCH-040: no separate
// scheduler service), so an in-process mutex is the correct atomicity
// primitive — not cross-process file locking.
type ClaimLedger struct {
	mu      sync.Mutex
	path    string
	entries map[string]ClaimEntry
	now     func() time.Time
	log     *journal.InstanceLog // optional; nil-safe
}

// LedgerOption configures a ClaimLedger.
type LedgerOption func(*ClaimLedger)

// WithLedgerClock overrides the time source (for deterministic tests).
func WithLedgerClock(now func() time.Time) LedgerOption {
	return func(l *ClaimLedger) { l.now = now }
}

// WithInstanceLog journals claim.acquired/claim.released transitions to the
// instance journal (§4/§6). Optional — a ledger with no log still functions,
// it just isn't observable via `cat scheduler/events.jsonl`.
func WithInstanceLog(log *journal.InstanceLog) LedgerOption {
	return func(l *ClaimLedger) { l.log = log }
}

// OpenClaimLedger loads the ledger at path (a JSON file under the instance's
// scheduler dir), creating an empty one if absent.
func OpenClaimLedger(path string, opts ...LedgerOption) (*ClaimLedger, error) {
	l := &ClaimLedger{path: path, entries: map[string]ClaimEntry{}, now: time.Now}
	for _, opt := range opts {
		opt(l)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil // fresh ledger
		}
		return nil, fmt.Errorf("localscheduler: read claim ledger: %w", err)
	}
	if len(data) == 0 {
		return l, nil
	}

	if err := json.Unmarshal(data, &l.entries); err != nil {
		return nil, fmt.Errorf("localscheduler: parse claim ledger %q: %w", path, err)
	}
	for storageKey, entry := range l.entries {
		if entry.ItemID == "" {
			entry.ItemID = storageKey
		}
		if entry.ExternalID == "" {
			entry.ExternalID = entry.ItemID
		}
		l.entries[storageKey] = entry
	}
	return l, nil
}

// MigrateLegacyNamespace upgrades pre-GAG-011 item-only keys into the sole
// active gaggle/provider namespace. Empty namespace values are accepted only
// when there is nothing to migrate.
func (l *ClaimLedger) MigrateLegacyNamespace(gaggle, provider string) error {
	return l.MigrateLegacyClaims(func(ClaimEntry) (ClaimNamespace, error) {
		if gaggle == "" || provider == "" {
			return ClaimNamespace{}, fmt.Errorf("legacy claim requires a gaggle and provider")
		}
		return ClaimNamespace{Gaggle: gaggle, Provider: provider}, nil
	})
}

// MigrateLegacyClaims upgrades pre-GAG-011 item-only keys using authoritative
// ownership resolved independently for each live claim.
func (l *ClaimLedger) MigrateLegacyClaims(resolve func(ClaimEntry) (ClaimNamespace, error)) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	legacy := make(map[string]ClaimEntry)
	for storageKey, entry := range l.entries {
		if entry.Gaggle == "" && entry.Provider == "" {
			legacy[storageKey] = entry
		}
	}
	if len(legacy) == 0 {
		return nil
	}
	if resolve == nil {
		return fmt.Errorf("localscheduler: legacy claim migration requires an ownership resolver")
	}

	type migration struct {
		legacyKey string
		scopedKey string
		entry     ClaimEntry
	}
	migrations := make([]migration, 0, len(legacy))
	planned := make(map[string]struct{}, len(legacy))
	for storageKey, entry := range legacy {
		namespace, err := resolve(entry)
		if err != nil {
			if errors.Is(err, ErrLegacyClaimOwnershipUnresolved) {
				continue
			}
			return fmt.Errorf("localscheduler: resolve legacy claim %q ownership: %w", entry.ItemID, err)
		}
		key := ClaimKey{Gaggle: namespace.Gaggle, Provider: namespace.Provider, ExternalID: entry.ItemID}
		scopedKey, err := key.storageKey()
		if err != nil {
			return err
		}
		if _, exists := l.entries[scopedKey]; exists {
			return fmt.Errorf("localscheduler: cannot migrate legacy claim %q: scoped claim already exists", entry.ItemID)
		}
		if _, exists := planned[scopedKey]; exists {
			return fmt.Errorf("localscheduler: cannot migrate duplicate legacy claim %q", entry.ItemID)
		}
		planned[scopedKey] = struct{}{}
		entry.Gaggle = namespace.Gaggle
		entry.Provider = namespace.Provider
		entry.ExternalID = entry.ItemID
		migrations = append(migrations, migration{legacyKey: storageKey, scopedKey: scopedKey, entry: entry})
	}
	if len(migrations) == 0 {
		return nil
	}

	previous := make(map[string]ClaimEntry, len(l.entries))
	for storageKey, entry := range l.entries {
		previous[storageKey] = entry
	}
	for _, migration := range migrations {
		delete(l.entries, migration.legacyKey)
		l.entries[migration.scopedKey] = migration.entry
	}
	if err := l.persist(); err != nil {
		l.entries = previous
		return err
	}
	return nil
}

// Claim attempts to atomically acquire itemID for runID under workflow, for
// leaseDuration. It fails (ok=false, holder=the current owner's run id) if a
// live (non-expired) lease is already held by a DIFFERENT run. An idempotent
// re-claim by the same runID succeeds and renews the lease — a retried
// backlog-query stage attempt (same run, same item) must not be refused by its
// own earlier claim.
//
// leaseDuration must be positive (issue #235, edge 1): a non-positive
// duration computes ExpiresAt <= ClaimedAt, so the entry is expired() at the
// moment it's written — expired() is exactly what the exclusivity guard
// below checks, so a non-positive lease would admit it unconditionally and
// let a second run silently co-own the same item. Fails closed before any
// ledger mutation, independent of ledger state, so this can never be
// bypassed by a caller-supplied duration (e.g. a workflow's leaseDuration
// input) reaching a live-lease branch that skips validation.
func (l *ClaimLedger) Claim(itemID, runID, workflow string, leaseDuration time.Duration) (ok bool, holder string, err error) {
	return l.claim(itemID, "", ClaimKey{ExternalID: itemID}, runID, workflow, leaseDuration)
}

// ClaimScoped acquires a claim namespaced by gaggle, provider, and external ID.
func (l *ClaimLedger) ClaimScoped(key ClaimKey, runID, workflow string, leaseDuration time.Duration) (ok bool, holder string, err error) {
	storageKey, err := key.storageKey()
	if err != nil {
		return false, "", err
	}
	return l.claim(storageKey, key.ExternalID, key, runID, workflow, leaseDuration)
}

func (l *ClaimLedger) claim(storageKey, legacyStorageKey string, key ClaimKey, runID, workflow string, leaseDuration time.Duration) (ok bool, holder string, err error) {
	if leaseDuration <= 0 {
		return false, "", fmt.Errorf("localscheduler: lease duration must be positive, got %s", leaseDuration)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	// An unresolved item-only claim could belong to any namespace, so it
	// remains exclusive against every scoped claimant until its lease expires.
	if legacyStorageKey != "" {
		if existing, held := l.entries[legacyStorageKey]; held && !existing.expired(now) {
			return false, existing.RunID, nil
		}
	}
	if existing, held := l.entries[storageKey]; held && !existing.expired(now) && existing.RunID != runID {
		return false, existing.RunID, nil
	}

	prev, hadPrev := l.entries[storageKey]
	entry := ClaimEntry{
		ItemID:     key.ExternalID,
		Gaggle:     key.Gaggle,
		Provider:   key.Provider,
		ExternalID: key.ExternalID,
		RunID:      runID,
		Workflow:   workflow,
		ClaimedAt:  now,
		ExpiresAt:  now.Add(leaseDuration),
	}
	l.entries[storageKey] = entry
	if err := l.persist(); err != nil {
		// Roll back the in-memory mutation so a failed persist leaves the item
		// exactly as it was — claimable if it was unheld, or still held by its
		// prior owner on an idempotent renewal. The ledger's in-memory and durable
		// state must never diverge: without this, a persist blip would strand the
		// item as un-claimable in memory while nothing durably holds it.
		if hadPrev {
			l.entries[storageKey] = prev
		} else {
			delete(l.entries, storageKey)
		}
		return false, "", err
	}
	l.journal(journal.EventClaimAcquired, entry)
	return true, runID, nil
}

// Release explicitly releases a claim (run finished, failed, or crash-recovery
// determined it orphaned). Releasing a claim not held (already released, held
// by a different run, or never claimed) is a no-op, not an error — normal
// completion and crash-recovery can race to release the same item, and both
// outcomes are fine as long as exactly one claimant ever wins.
func (l *ClaimLedger) Release(itemID, runID string) error {
	return l.release(itemID, runID)
}

// ReleaseScoped releases a claim identified by its scoped key.
func (l *ClaimLedger) ReleaseScoped(key ClaimKey, runID string) error {
	storageKey, err := key.storageKey()
	if err != nil {
		return err
	}
	return l.release(storageKey, runID)
}

// ReleaseEntry releases entry without reconstructing whether it came from a
// scoped or legacy ledger key.
func (l *ClaimLedger) ReleaseEntry(entry ClaimEntry, runID string) error {
	if entry.Gaggle == "" || entry.Provider == "" {
		return l.Release(entry.ItemID, runID)
	}
	return l.ReleaseScoped(ClaimKey{
		Gaggle:     entry.Gaggle,
		Provider:   entry.Provider,
		ExternalID: entry.ExternalID,
	}, runID)
}

func (l *ClaimLedger) release(storageKey, runID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, held := l.entries[storageKey]
	if !held || entry.RunID != runID {
		return nil
	}
	delete(l.entries, storageKey)
	if err := l.persist(); err != nil {
		// Same rollback discipline as Claim: a failed persist must not leave
		// memory believing the item is free while the durable ledger (if the
		// write partially landed) or a crash-recovery reread still sees it
		// held — otherwise the item could be double-claimed while this run
		// still holds it, or the caller believes the release succeeded and
		// finalizes the run while the ledger still lists it as claimed.
		l.entries[storageKey] = entry
		return err
	}
	l.journal(journal.EventClaimReleased, entry)
	return nil
}

// ForceRelease releases itemID without requiring the holding run ID. It is
// reserved for operator recovery of stuck claims and journals a distinct event
// so the override cannot be mistaken for normal run cleanup.
func (l *ClaimLedger) ForceRelease(itemID string) error {
	return l.forceRelease(itemID, forceReleaseActorCLI)
}

// ForceReleaseEntry force-releases entry without losing its namespace and
// records actor in the distinct administrative journal event.
func (l *ClaimLedger) ForceReleaseEntry(entry ClaimEntry, actor string) error {
	if entry.Gaggle == "" || entry.Provider == "" {
		return l.forceRelease(entry.ItemID, actor)
	}
	storageKey, err := (ClaimKey{
		Gaggle:     entry.Gaggle,
		Provider:   entry.Provider,
		ExternalID: entry.ExternalID,
	}).storageKey()
	if err != nil {
		return err
	}
	return l.forceRelease(storageKey, actor)
}

func (l *ClaimLedger) forceRelease(storageKey, actor string) error {
	if actor == "" {
		return errors.New("localscheduler: force-release actor is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, held := l.entries[storageKey]
	if !held {
		return nil
	}
	delete(l.entries, storageKey)
	if err := l.persist(); err != nil {
		l.entries[storageKey] = entry
		return err
	}
	l.journalWithRunner(journal.EventClaimForceReleased, entry, map[string]any{"actor": actor})
	return nil
}

// RecoverExpired releases every lease whose expiry has passed as of now and
// returns the released entries — the crash-recovery pass (SCH-021): a lease
// survives its owning run's crash only until it expires, at which point the
// item is claimable again exactly once. Call once at daemon startup (recovers
// leases orphaned by a prior crash) and periodically thereafter (catches a live
// run that overran its lease without crashing).
//
// Safety (WF-031): auto-releasing a lease whose owning run is still live but
// simply ran long invites double-processing — the freed item can be claimed
// by a second run while the first is still working it. This ledger's only
// heartbeat mechanism is Claim's own idempotent re-claim-by-same-runID path
// (see Claim's doc comment), which renews ExpiresAt; nothing currently drives
// it periodically. Until a caller wires that renewal, leaseDuration passed to
// Claim MUST be set well above the workflow's realistic max run duration, not
// tuned tightly — RecoverExpired trusts the lease at face value and has no
// way to distinguish "orphaned by a crash" from "still running, just slow."
//
// Issue #235 (edge 2): a ci-poll-bearing implementation run can exceed the
// OLD 2h DefaultClaimLease (cmd/goobers/backlogquery.go), which made this
// hazard reachable in the shipped config, not just theoretical. The chosen
// V0.2 mitigation is raising DefaultClaimLease comfortably above a realistic
// run's duration — not the liveness-aware renewal this comment already
// describes as the durable fix, which remains deferred to V1 hardening.
func (l *ClaimLedger) RecoverExpired(now time.Time) ([]ClaimEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	type releasedClaim struct {
		storageKey string
		entry      ClaimEntry
	}
	var released []releasedClaim
	for storageKey, entry := range l.entries {
		if entry.expired(now) {
			delete(l.entries, storageKey)
			released = append(released, releasedClaim{storageKey: storageKey, entry: entry})
		}
	}
	if len(released) == 0 {
		return nil, nil
	}
	if err := l.persist(); err != nil {
		// Roll back every deletion this pass made: a partial view (some
		// entries released in memory, none durably, none journaled) would
		// both strand those items as claimable-in-memory-only and discard,
		// via the (nil, err) return, the exact set the caller would need to
		// retry or reconcile — restoring them makes a failed pass a clean
		// no-op the caller can safely retry on its next periodic call.
		for _, claim := range released {
			l.entries[claim.storageKey] = claim.entry
		}
		return nil, err
	}
	entries := make([]ClaimEntry, 0, len(released))
	for _, claim := range released {
		l.journal(journal.EventClaimReleased, claim.entry)
		entries = append(entries, claim.entry)
	}
	return entries, nil
}

// Lookup returns the current entry for itemID, if any live or expired claim
// exists (for inspection/testing; does not distinguish expired from live —
// callers wanting only live claims should check ExpiresAt themselves or use
// RecoverExpired first).
func (l *ClaimLedger) Lookup(itemID string) (ClaimEntry, bool) {
	return l.lookup(itemID)
}

// LookupScoped returns the entry for a gaggle/provider/external-ID key.
func (l *ClaimLedger) LookupScoped(key ClaimKey) (ClaimEntry, bool) {
	storageKey, err := key.storageKey()
	if err != nil {
		return ClaimEntry{}, false
	}
	return l.lookup(storageKey)
}

func (l *ClaimLedger) lookup(storageKey string) (ClaimEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[storageKey]
	return e, ok
}

// Snapshot returns every ledger entry ordered by item ID and namespace.
func (l *ClaimLedger) Snapshot() []ClaimEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries := make([]ClaimEntry, 0, len(l.entries))
	for _, entry := range l.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ItemID != entries[j].ItemID {
			return entries[i].ItemID < entries[j].ItemID
		}
		if entries[i].Gaggle != entries[j].Gaggle {
			return entries[i].Gaggle < entries[j].Gaggle
		}
		return entries[i].Provider < entries[j].Provider
	})
	return entries
}

// ForRun returns the entry runID currently holds, if any (for inspection;
// same expired/live caveat as Lookup). A workflow whose backlog-query stage
// claims at most one item per run (#131's implementation.yaml: maxItems=1)
// can use this to recover which item its own run is processing from a
// later stage — a downstream stage such as issue-close-out runs as its own
// OS process in its own worktree, several stages after backlog-query, with
// no other way to learn the claimed item's id (Task.InputsFrom only threads
// from the immediately preceding stage, not an arbitrary earlier one, and
// backlog-query's own worktree — where it wrote the claimed item as a result
// file — no longer exists by the time a later stage runs). If a run somehow
// holds more than one claim, the entry returned is unspecified — the ledger
// does not track per-run claim order.
func (l *ClaimLedger) ForRun(runID string) (ClaimEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.RunID == runID {
			return e, true
		}
	}
	return ClaimEntry{}, false
}

// ForRunAll returns every entry runID currently holds, ordered by item ID
// (for inspection; same expired/live caveat as Lookup).
func (l *ClaimLedger) ForRunAll(runID string) []ClaimEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	var entries []ClaimEntry
	for _, e := range l.entries {
		if e.RunID == runID {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ItemID < entries[j].ItemID
	})
	return entries
}

// persist rewrites the ledger file atomically. Caller holds l.mu.
func (l *ClaimLedger) persist() error {
	data, err := json.MarshalIndent(l.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("localscheduler: marshal claim ledger: %w", err)
	}
	if err := journal.WriteFileAtomic(l.path, data, 0o644); err != nil {
		return fmt.Errorf("localscheduler: persist claim ledger: %w", err)
	}
	return nil
}

// journal appends a claim transition to the instance log, if one is wired.
// Best-effort observability, not the durability mechanism (persist() above is)
// — a journal write failure here is deliberately swallowed rather than failing
// the claim/release operation the ledger already committed.
func (l *ClaimLedger) journal(eventType journal.EventType, entry ClaimEntry) {
	l.journalWithRunner(eventType, entry, nil)
}

func (l *ClaimLedger) journalWithRunner(eventType journal.EventType, entry ClaimEntry, runner map[string]any) {
	if l.log == nil {
		return
	}
	_ = l.log.Append(journal.Event{
		Type:     eventType,
		Name:     entry.ItemID,
		Gaggle:   entry.Gaggle,
		RunID:    entry.RunID,
		Workflow: entry.Workflow,
		Runner:   runner,
	})
}
