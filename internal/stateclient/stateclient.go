// Package stateclient is the scheduler-state primitive set every stage CLI
// that touches gaggle-scoped scheduler state calls, behind one interface with
// two backends (decision 005 ruling 3; finding 002 "plane clients" §3, plan
// step C2).
//
// Scheduler state is the daemon-owned state that is NOT a claim: the
// learned-dependency block records (blocked.json), the per-scan backlog
// cursor (backlog-scan-<hash>.json, #2067 fairness), the backlog re-sweep
// generation, the reconcile-post-merge ledger, the gather-sibling-context
// cache, and the backlog-health ready-transition ledger
// (backlog-health/<gaggle>__<provider>__<repository>__<label>.json,
// Goobers#3948). Each lives as one JSON file in the instance's scheduler
// directory today, read and written in-process by the CLI when it shares a
// host with the daemon (type-1/type-2) and by the daemon's own scheduler. A
// stage POD has neither the file nor the lock.
//
// The two backends:
//
//   - File: the instance's own file under the key's own cross-process lock —
//     byte for byte the discipline cmd/goobers used before this package
//     existed. The lock is injected by the caller, so nothing about how the
//     daemon serializes changes: blocked.json and the scan cursor keep
//     claims.lock, the post-merge ledger keeps post-merge-reconcile.lock, the
//     sibling cache keeps sibling-context-cache.lock, and the ready-transition
//     ledger — which had no lock at all, because until the plane existed it
//     had a single in-process writer — gets its own, since a compare-and-swap
//     served over the plane is atomic only if the daemon serializes the
//     compare against the swap.
//   - HTTP: the daemon's scheduler-state plane
//     (GET/PUT /api/v1/gaggles/{gaggle}/state/{key}), selected when
//     GOOBERS_STATE_ENDPOINT and a state bearer are present in the stage's
//     environment — the path a stage pod takes, where the daemon is the
//     single writer (DS1) and serves each half under the SAME per-key lock
//     the in-process path takes.
//
// What this package moves is the transport, not the semantics. The one
// deliberate difference is where the atomicity comes from, and it is the whole
// point of the route's If-Match contract: on the file backend a read-modify-
// write runs inside ONE lock acquisition, so it cannot lose an update; on the
// plane the read and the write are separate round trips, so the write carries
// the read's ETag and a lost update becomes a refused write the caller retries.
// Update presents both as the same call, which is why a runner-driven 2.0 run
// (file backend, in the daemon's process) and an engine-driven 3.0 run (plane
// backend, in a pod) can advance the same scan cursor concurrently without
// either overwriting the other: they contend on one lock, in one atomicity
// domain, with the plane's CAS as the tie-break.
package stateclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Environment variables that select the HTTP backend in a stage process.
//
// Deliberately NOT the dispatcher's own GOOBERS_DAEMON_API/GOOBERS_POD_TOKEN
// pair: those are stripped from every stage environment
// (dispatcher.DispatcherPrivilegedEnv) because the pod token authorizes
// surrendering the run's result, and a CLI subprocess must not be able to
// author its own outcome. The state plane gets its own narrowly-scoped bearer
// for the same reason the claims plane does (claimsclient.EnvToken).
const (
	// EnvEndpoint is the daemon API base URL the state plane is reached at.
	EnvEndpoint = "GOOBERS_STATE_ENDPOINT"
	// EnvToken is the state-scoped bearer presented to the plane.
	EnvToken = "GOOBERS_STATE_TOKEN"
	// EnvGaggle is the gaggle the stage acts for — the route's scope. Shares
	// the dispatcher's spelling (dispatcher.EnvGaggle), which a goobers-CLI
	// stage already keeps.
	EnvGaggle = "GOOBERS_GAGGLE"
)

// ErrEndpointWithoutToken is the fail-closed refusal Select answers when the
// endpoint is configured but no bearer is: a stage in a pod must never fall
// back to a scheduler-state file it does not have.
var ErrEndpointWithoutToken = errors.New("stateclient: " + EnvEndpoint + " is set but " + EnvToken + " is empty; refusing to fall back to a local scheduler-state file")

// ErrEndpointWithoutGaggle is the sibling refusal for a missing gaggle: the
// route is gaggle-scoped and the plane contains every call to the caller's own
// gaggle, so a client with no gaggle has nothing it may legally address.
var ErrEndpointWithoutGaggle = errors.New("stateclient: " + EnvEndpoint + " is set but " + EnvGaggle + " is empty; the scheduler-state plane is gaggle-scoped")

// ErrPreconditionFailed reports a compare-and-swap that lost: the key's value
// changed between the read that produced the ETag and this write. It is a
// business outcome, not a fault — Update retries on it.
var ErrPreconditionFailed = errors.New("stateclient: scheduler-state precondition failed (the value changed since it was read)")

// ErrInvalidKey reports a key outside the closed scheduler-state namespace.
var ErrInvalidKey = errors.New("stateclient: key is not a scheduler-state key")

// MaxValueBytes bounds one scheduler-state value on the wire and on disk. The
// largest of these in production is the sibling cache, which holds one entry
// per currently-open PR; 8 MiB is orders of magnitude above any observed size
// and still far below a bound that would let a pod exhaust the daemon's
// memory through the plane.
const MaxValueBytes = 8 << 20

// Well-known scheduler-state keys. The namespace is CLOSED: ValidKey admits
// exactly these shapes and nothing else, so a state-plane bearer can never be
// turned into a read or a write of claims.json, the instance config, or any
// other file in the scheduler directory. Fail closed — an unrecognised key is
// refused, not resolved.
const (
	// KeyBlockedRecords is blocked.json, the learned-dependency block records
	// (#552). Guarded by claims.lock in-process; the plane serves it under the
	// same lock.
	KeyBlockedRecords = "blocked.json"
	// KeyPostMergeReconcileLedger is the reconcile-post-merge ledger.
	KeyPostMergeReconcileLedger = "post-merge-reconcile.json"
	// KeySiblingContextCache is gather-sibling-context's per-sibling memo
	// (#523).
	KeySiblingContextCache = "sibling-context-cache.json"
)

// scanCursorKeyPattern matches the per-scan backlog cursor,
// backlog-scan-<sha256 of the scan's cache key>.json — the only key shape in
// the namespace that is not a fixed name. The digest is pinned to exactly 64
// lowercase hex characters, so no other spelling (a traversal, a wildcard, a
// differently-cased digest) is admitted.
var scanCursorKeyPattern = regexp.MustCompile(`^backlog-scan-[0-9a-f]{64}\.json$`)

// resweepStateKeyPattern matches the per-scan-shape backlog RE-SWEEP state,
// backlog-resweep-<sha256 of the re-sweep's shape key>.json, pinned to the
// same 64-lowercase-hex digest for the same reason.
//
// It joins the namespace because it is the LAST scheduler-directory file the
// claiming path read and wrote directly (Goobers#3898): with the annotations
// on the journal plane and the claims on the claims plane, this one file was
// all that still held `backlog-query --claim` to StageRequiresInstanceRoot.
// Its generation counter also makes it a natural compare-and-swap value —
// exactly what the plane's Update serves under claims.lock — so a pod-executed
// re-sweep and a daemon-driven one serialize instead of each advancing a
// private copy.
var resweepStateKeyPattern = regexp.MustCompile(`^backlog-resweep-[0-9a-f]{64}\.json$`)

// The backlog-health READY-TRANSITION cursor (Goobers#3948), the one key in
// this namespace that does not live directly in the scheduler directory and
// the one whose name is not a digest.
//
// It is scheduler state by every test this package's doc comment applies:
// daemon-owned, not a claim, one JSON file under the instance's scheduler
// directory, read and written in-process by the CLI when it shares a host with
// the daemon. It was simply left behind when C2 landed, and it was the last
// thing holding `backlog-health` — BOTH modes — to
// executor.StageRequiresInstanceRoot.
//
// TWO shape differences from the rest of the namespace, both deliberate:
//
//   - It resolves into a SUBDIRECTORY. The file is
//     <schedulerDir>/backlog-health/<gaggle>__<provider>__<repository>__<label>.json
//     and it must STAY there — a pod-executed cycle and a daemon-driven one
//     advancing two different paths is precisely the split this plane exists
//     to prevent. The key therefore carries the subdirectory as a "." -
//     separated prefix rather than a "/": a slash in the key would be
//     percent-escaped onto the wire, decoded back into a second path segment,
//     and refused by the router's structural pod-scope match (which requires
//     the key to be ONE segment) — a 403 that reads like a containment failure
//     rather than the encoding accident it is. KeyRelativePath is what turns
//     the prefix back into the directory, and it is the ONLY place a key
//     becomes more than one path element.
//   - It carries the GAGGLE. Every other key in this namespace is
//     gaggle-agnostic, so the route's gaggle segment is the whole of its
//     containment. Here it is not: a pod contained to gaggle A could otherwise
//     name gaggle B's cursor in the key and the route would serve it under A's
//     scope. So the key keeps the gaggle in its own "."-delimited position
//     instead of leaving it as the first of the file name's four "__"-joined
//     coordinates — "__" cannot decide where the gaggle ends (a repository or
//     a label may legitimately contain one, and "goobers__x__github__..."
//     would otherwise be readable by a pod scoped to plain "goobers") whereas
//     "." can, because instance.SchedulerNameSegment maps "." to "_" and no
//     sanitized coordinate can contain one. See
//     BacklogHealthCursorKeyContained, which the daemon applies alongside the
//     run-belongs-to-gaggle check.
const (
	// BacklogHealthCursorKeyPrefix opens every ready-transition cursor key.
	BacklogHealthCursorKeyPrefix = "backlog-health."
	// BacklogHealthCursorDirName is the scheduler-directory subdirectory the
	// prefix resolves to. Pinned against instance.BacklogHealthDirName by
	// cmd/goobers's parity test rather than imported, so this package stays
	// free of the instance layout.
	BacklogHealthCursorDirName = "backlog-health"
	// backlogHealthCursorGaggleSep delimits the gaggle from the rest of the
	// scan's coordinates. Unambiguous by construction: see above.
	backlogHealthCursorGaggleSep = "."
)

// backlogHealthCursorKeyPattern pins a cursor key to the prefix, the gaggle,
// and exactly the shape instance.BacklogHealthCursorScope produces: three
// "__"-joined coordinates over a closed character class that contains no path
// separator, no "." and no percent — so the key cannot spell a traversal,
// cannot acquire a second path segment on the wire, and cannot name a file
// outside BacklogHealthCursorDirName. Fail closed: a coordinate the sanitizer
// would have reduced to something outside this class is refused, never
// resolved.
var backlogHealthCursorKeyPattern = regexp.MustCompile(
	`^backlog-health\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+(?:__[A-Za-z0-9_-]+){2}\.json$`)

// BacklogHealthCursorKey names the ready-transition cursor for one scan.
// gaggleSegment is the gaggle reduced by instance.SchedulerNameSegment and
// scope is instance.BacklogHealthCursorScope's output for the scan's other
// three coordinates; joined, they are exactly the file name
// instance.BacklogHealthCursorName produces. Deriving the key from the SAME
// sanitizer the path builder uses, rather than reducing the coordinates a
// second time here, is what makes it impossible for the key and the path to
// disagree about which file they mean.
func BacklogHealthCursorKey(gaggleSegment, scope string) string {
	return BacklogHealthCursorKeyPrefix + gaggleSegment + backlogHealthCursorGaggleSep + scope
}

// BacklogHealthCursorKeyContained reports whether key is addressable by a
// caller scoped to gaggleSegment (the caller's gaggle reduced by
// instance.SchedulerNameSegment). Keys that carry no gaggle are contained by
// the route's own scope and are admitted here unchanged; a cursor key is
// admitted only when its gaggle is exactly the caller's own.
func BacklogHealthCursorKeyContained(key, gaggleSegment string) bool {
	if !strings.HasPrefix(key, BacklogHealthCursorKeyPrefix) {
		return true
	}
	if gaggleSegment == "" || strings.Contains(gaggleSegment, backlogHealthCursorGaggleSep) {
		return false
	}
	return strings.HasPrefix(key, BacklogHealthCursorKey(gaggleSegment, ""))
}

// KeyRelativePath resolves a scheduler-state key to its path RELATIVE to the
// scheduler directory. Every key but the backlog-health cursor is its own file
// name; the cursor's prefix becomes its subdirectory and its gaggle rejoins
// the rest of the coordinates. checkKey runs first, so nothing outside the
// closed namespace ever reaches a path join.
func KeyRelativePath(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	rest, ok := strings.CutPrefix(key, BacklogHealthCursorKeyPrefix)
	if !ok {
		return key, nil
	}
	// checkKey admitted it, so the separator is present and both sides are
	// single, traversal-free path components.
	gaggleSegment, scope, _ := strings.Cut(rest, backlogHealthCursorGaggleSep)
	return filepath.Join(BacklogHealthCursorDirName, gaggleSegment+"__"+scope), nil
}

// ScanCursorKey names the scan cursor for a scan-key digest.
func ScanCursorKey(digest string) string {
	return "backlog-scan-" + digest + ".json"
}

// ResweepStateKey names the backlog re-sweep state for a shape digest.
func ResweepStateKey(digest string) string {
	return "backlog-resweep-" + digest + ".json"
}

// ValidKey reports whether key is one of the closed scheduler-state keys.
func ValidKey(key string) bool {
	switch key {
	case KeyBlockedRecords, KeyPostMergeReconcileLedger, KeySiblingContextCache:
		return true
	}
	return scanCursorKeyPattern.MatchString(key) ||
		resweepStateKeyPattern.MatchString(key) ||
		backlogHealthCursorKeyPattern.MatchString(key)
}

// Value is one scheduler-state read: the bytes and the ETag that addresses
// exactly this version of them. An ABSENT key reads as the zero Value — nil
// Data and an empty ETag — and an empty ETag is itself a precondition ("the
// key must still be absent"), so a read-modify-write that creates a key is the
// same code as one that replaces it.
type Value struct {
	Data []byte
	ETag string
}

// Exists reports whether the key had a value when it was read.
func (v Value) Exists() bool { return v.ETag != "" }

// ETagFor is the ETag a value's bytes hash to — the content digest, so two
// writers that independently produce identical bytes produce identical ETags
// and neither spuriously refuses the other.
func ETagFor(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Store is the scheduler-state primitive set. Get and Put are the route's two
// halves; Update is the read-modify-write both backends implement as one
// atomic operation by their own means.
type Store interface {
	// Get reads key. An absent key is the zero Value, never an error — the
	// overwhelmingly common first-run state for every one of these files.
	Get(ctx context.Context, key string) (Value, error)
	// Put writes data at key if the key's current ETag is ifMatch. An empty
	// ifMatch requires the key to be ABSENT. A precondition that does not hold
	// answers ErrPreconditionFailed, which is a business outcome, not a fault.
	// The returned Value is what was written, with its new ETag.
	Put(ctx context.Context, key string, data []byte, ifMatch string) (Value, error)
	// Update runs fn against the key's current value and writes what fn
	// returns, atomically with respect to every other writer of the same key.
	// fn returns write=false to leave the key untouched (nothing changed — the
	// common case for a reconcile that found no drift). operation labels the
	// critical section for the file backend's lock journaling, exactly as the
	// claims lock's operation labels do.
	//
	// On the file backend fn runs INSIDE the key's lock, so it observes and
	// replaces the value with no window in between — today's behaviour, byte
	// for byte. On the plane fn runs between two round trips and the write
	// carries the read's ETag: a lost update is refused and fn is re-run
	// against the new value, up to MaxUpdateAttempts. fn must therefore be
	// safe to run more than once.
	Update(ctx context.Context, key, operation string, fn func(Value) (data []byte, write bool, err error)) error
	// Section runs fn as one critical section over key, for the callers whose
	// read-modify-write cannot be expressed as a single Update: the post-merge
	// reconcile, which reads the ledger once and then persists partial
	// progress repeatedly as it polls providers, so that a crash mid-scan does
	// not repeat the actions it already took.
	//
	// On the file backend this is the key's lock held across the whole of fn —
	// exactly the flock those callers held before the plane existed, so local
	// behaviour is unchanged. On the plane there is no local lock to hold:
	// mutual exclusion comes from each write inside fn carrying the ETag it
	// read, so an interleaved writer is REFUSED (ErrPreconditionFailed) rather
	// than silently clobbered. Callers must therefore treat a precondition
	// failure inside a Section as a real outcome and retry the work, never as
	// a fault to ignore.
	Section(ctx context.Context, key, operation string, fn func() error) error
}

// MaxUpdateAttempts bounds the plane backend's CAS retry loop. High enough
// that genuine contention (a handful of concurrent runs) always resolves, low
// enough that a pathological hot key fails loudly instead of spinning.
const MaxUpdateAttempts = 8

// ErrUpdateContention reports an Update that exhausted MaxUpdateAttempts.
var ErrUpdateContention = fmt.Errorf("stateclient: scheduler-state update lost %d compare-and-swap attempts", MaxUpdateAttempts)

// Select chooses the backend for a stage process from its environment: the
// plane when EnvEndpoint is set (fail closed on a missing bearer or gaggle —
// never a silent fall-through to a file the pod does not have), else the file
// backend the caller constructs.
func Select(getenv func(string) string, file func() (Store, error)) (Store, error) {
	endpoint := strings.TrimSpace(getenv(EnvEndpoint))
	if endpoint == "" {
		return file()
	}
	token := strings.TrimSpace(getenv(EnvToken))
	if token == "" {
		return nil, ErrEndpointWithoutToken
	}
	gaggle := strings.TrimSpace(getenv(EnvGaggle))
	if gaggle == "" {
		return nil, ErrEndpointWithoutGaggle
	}
	return NewHTTP(HTTPConfig{BaseURL: endpoint, Token: token, Gaggle: gaggle})
}

// Selected reports whether the environment names the state plane — the same
// predicate Select applies, read by a seam that needs to skip instance-root
// side work the plane owns.
func Selected(getenv func(string) string) bool {
	return strings.TrimSpace(getenv(EnvEndpoint)) != ""
}

// checkKey is the fail-closed key guard both backends apply before they
// resolve a key into a path or a URL.
func checkKey(key string) error {
	if !ValidKey(key) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return nil
}

// checkValue bounds a value before it is written.
func checkValue(data []byte) error {
	if len(data) > MaxValueBytes {
		return fmt.Errorf("stateclient: scheduler-state value is %d bytes, over the %d-byte limit", len(data), MaxValueBytes)
	}
	return nil
}
