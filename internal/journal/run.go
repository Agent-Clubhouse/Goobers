package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ErrClosed is returned by writer operations after Close.
var ErrClosed = errors.New("journal: run is closed")

// Run is a writer over a single run journal. It owns the append handle to
// events.jsonl and enforces the durability contract: every Append scrubs, writes
// a single line, and fsyncs before returning, so a completed event is never lost
// to a crash. All methods are safe for concurrent use.
type Run struct {
	dir      string
	id       RunIdentity
	scrubber Scrubber
	now      func() time.Time

	mu           sync.Mutex
	events       *os.File
	lock         *os.File
	seq          uint64
	phase        RunPhase
	machineState string
	reason       string
	appendErr    error
	closed       bool
}

// acquireRunLock takes a blocking exclusive flock on dir's lock file,
// serializing every writer that opens the same run directory (#243): the
// only flocks before this were the whole-instance up.lock and the claim
// ledger's — the run journal itself took none, so `goobers run abort`
// (which deliberately skips up.lock, see cmd/goobers/run.go) racing a live
// daemon's own Resume of the same crashed run could open two independent
// *Run writers on one events.jsonl, each with its own in-memory seq —
// interleaved appends, duplicate/rewound seq, racing state.json renames.
// Both Create and Recover hold this for the lifetime of the returned *Run,
// releasing it in Close; a second caller's acquireRunLock simply blocks
// until the first releases, rather than erroring — matching this
// package's existing bias (see cmd/goobers's withClaimLock) that a loser
// here should wait its turn and get a consistent view, not fail outright.
// It is not reentrant: acquiring the same run lock twice through separate
// descriptors in one process blocks too. Current flows avoid that deadlock:
// Create uses a fresh run id, and in-process resume closes its writer first.
func acquireRunLock(dir string) (*os.File, error) {
	return acquireJournalLock(dir, "run")
}

// releaseRunLock unlocks and closes a lock file acquireRunLock returned. Safe
// to call with nil (a Run that never acquired one, e.g. a construction path
// that failed before acquireRunLock ran).
func releaseRunLock(f *os.File) {
	releaseJournalLock(f)
}

// config holds constructor options.
type config struct {
	scrubber Scrubber
	now      func() time.Time
}

// Option configures a Run at creation/open.
type Option func(*config)

// WithScrubber sets the boundary scrubber applied to every event, snapshot, and
// artifact before write and before digesting. Defaults to the pattern net; pass
// a registry-backed chain (see DefaultScrubber) to redact resolver-issued
// credentials by exact value.
func WithScrubber(s Scrubber) Option {
	return func(c *config) { c.scrubber = s }
}

// WithClock overrides the time source (for deterministic tests).
func WithClock(now func() time.Time) Option {
	return func(c *config) { c.now = now }
}

func newConfig(opts ...Option) config {
	c := config{scrubber: NewPatternScrubber(), now: time.Now}
	for _, opt := range opts {
		opt(&c)
	}
	if c.scrubber == nil {
		// Fail closed: a nil scrubber must never disable redaction. Fall back to
		// the pattern net (the same default as an unset scrubber), not nopScrubber
		// — silently degrading to no scrubbing would let secrets land at rest
		// (SEC-041). A caller that genuinely wants no scrubbing opts in explicitly
		// via WithScrubber(Chain()).
		c.scrubber = NewPatternScrubber()
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// Dir returns the run directory.
func (r *Run) Dir() string { return r.dir }

// Create scaffolds a new run journal under runsDir/<run-id>, pins the identity
// to run.yaml, snapshots the given inputs by content digest, writes the initial
// state.json checkpoint, and appends the run.started event. inputs may be nil
// (e.g. a schedule-triggered run with no originating item).
func Create(runsDir string, id RunIdentity, inputs map[string][]byte, opts ...Option) (*Run, error) {
	if id.RunID == "" {
		return nil, errors.New("journal: RunID is required")
	}
	// A run id is joined onto runsDir below as a single path segment — it
	// must never itself be able to escape it (#244). Run ids are minted
	// internally as safe random hex today, but this is the ONE place every
	// run directory gets created, so it is the right fail-closed boundary
	// regardless of how a future caller sources the id.
	if !apiv1.ValidRunID(id.RunID) {
		return nil, fmt.Errorf("journal: invalid run id %q", id.RunID)
	}
	cfg := newConfig(opts...)
	dir := filepath.Join(runsDir, id.RunID)
	// Create the run directory atomically. os.Mkdir fails with EEXIST if the dir
	// already exists, so two processes racing to create the same run id can't both
	// proceed and interleave writers on one journal — the loser gets a clean
	// "already exists" error instead of the previous Stat-then-MkdirAll TOCTOU
	// window (goobers run takes no flock). The parent chain (shared instance state,
	// plus any nested segments if a run id contains a separator) is created
	// non-exclusively first; only the leaf run dir needs the atomic guarantee.
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, fmt.Errorf("journal: create runs dir: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("journal: run %q already exists at %s", id.RunID, dir)
		}
		return nil, fmt.Errorf("journal: create run dir: %w", err)
	}
	// fsync the parent (runs) directory so the new run dir's own directory
	// entry is durable across a crash (#243) — every OTHER durable write in
	// this package (writeStateAtomic, writeFileAtomic) already fsyncs its
	// parent after a rename into it; this Mkdir was the one directory-entry
	// creation that didn't, so a crash right after it could lose the whole
	// run dir despite its own contents being fsynced later.
	if err := fsyncDir(filepath.Dir(dir)); err != nil {
		return nil, fmt.Errorf("journal: fsync runs dir: %w", err)
	}
	for _, sub := range []string{dirInputs, dirArtifacts, dirSpans} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("journal: create run subdir %q: %w", sub, err)
		}
	}

	// Acquire the per-run-dir lock (#243) before any further writes. Create
	// itself is already race-free against a second Create of the SAME new
	// id (os.Mkdir's EEXIST atomicity above), but this run directory is
	// about to become resumable — the lock, held for the lifetime of the
	// returned *Run, is what stops a concurrent Recover (e.g. `goobers run
	// abort` racing this same run while it's still live) from opening a
	// second independent writer on this journal.
	lock, err := acquireRunLock(dir)
	if err != nil {
		return nil, err
	}

	id.Schema = RunSchema
	if id.StartedAt.IsZero() {
		id.StartedAt = cfg.now()
	}
	// Snapshot inputs immutably before pinning run.yaml, so run.yaml commits to
	// the scrubbed digests.
	id.Inputs = id.Inputs[:0:0]
	for _, name := range sortedKeys(inputs) {
		ref, err := writeContent(dir, filepath.Join(dirInputs, name), inputs[name], cfg.scrubber)
		if err != nil {
			releaseRunLock(lock)
			return nil, fmt.Errorf("journal: snapshot input %q: %w", name, err)
		}
		id.Inputs = append(id.Inputs, InputRef{Name: name, Ref: ref})
	}
	if err := writeRunYAML(dir, id); err != nil {
		releaseRunLock(lock)
		return nil, err
	}

	events, err := os.OpenFile(filepath.Join(dir, fileEvents), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		releaseRunLock(lock)
		return nil, fmt.Errorf("journal: open events log: %w", err)
	}
	r := &Run{
		dir:      dir,
		id:       id,
		scrubber: cfg.scrubber,
		now:      cfg.now,
		events:   events,
		lock:     lock,
		phase:    PhaseRunning,
	}
	if err := r.append(Event{Type: EventRunStarted, Status: string(PhaseRunning)}); err != nil {
		_ = events.Close()
		releaseRunLock(lock)
		return nil, err
	}
	if err := r.checkpoint(); err != nil {
		_ = events.Close()
		releaseRunLock(lock)
		return nil, err
	}
	return r, nil
}

// Append scrubs, stamps, writes, and fsyncs one event. seq, schema, and time are
// assigned by the journal — any values set by the caller are overwritten.
func (r *Run) Append(ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if err := r.append(ev); err != nil {
		return err
	}
	// Track terminal phase so Close/Checkpoint reflect the last run.finished.
	// Reason mirrors the terminal event's own Error.Message, if any (#520) —
	// empty for an ordinary business-outcome terminal that carries no error.
	if ev.Type == EventRunFinished {
		r.phase = phaseFromStatus(ev.Status)
		r.machineState = ""
		r.reason = ""
		if ev.Error != nil {
			r.reason = ev.Error.Message
		}
	}
	return r.checkpoint()
}

// append is the lock-held core: assign seq, scrub the serialized line, write, fsync.
func (r *Run) append(ev Event) error {
	if r.appendErr != nil {
		return fmt.Errorf("journal: append blocked after prior write failure: %w", r.appendErr)
	}
	_, err := appendEvent(r.events, &r.seq, r.scrubber, r.now, ev)
	if err != nil {
		r.appendErr = err
	}
	return err
}

// RepairAppendBoundary restores events.jsonl after an Append failure. A torn
// final record is discarded and recorded with a repaired event; a complete
// final record is retained. The sequence is reconstructed from the surviving
// log before appends are allowed again.
func (r *Run) RepairAppendBoundary() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}

	path := filepath.Join(r.dir, fileEvents)
	events, tornBytes, err := readEvents(path)
	if err != nil {
		return err
	}
	if err := truncateTornTail(path, tornBytes); err != nil {
		return err
	}

	r.seq = highestEventSeq(events)
	r.appendErr = nil
	if tornBytes == 0 {
		return nil
	}
	return r.append(Event{
		Type:   EventRepaired,
		Runner: map[string]any{"discardedBytes": tornBytes},
	})
}

// SetMachineState records the current state-machine node used in the next
// checkpoint. The runner calls this as it advances; it does not itself write.
func (r *Run) SetMachineState(state string) {
	r.mu.Lock()
	r.machineState = state
	r.mu.Unlock()
}

// Checkpoint writes state.json immediately, reflecting the current
// MachineState. Most transitions checkpoint implicitly as part of Append or
// RecordArtifact; call Checkpoint directly when a transition pauses without
// either — e.g. a human gate (ARCHITECTURE.md §5: "a human gate executes
// nothing"), which appends no event but still must leave state.json pointing
// at the correct resume state.
func (r *Run) Checkpoint() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	return r.checkpoint()
}

// RecordArtifact scrubs data, stores it by content digest under artifacts/, and
// appends an artifact.recorded event. Identical content deduplicates to one
// blob. The returned Ref's Digest commits to the scrubbed bytes.
func (r *Run) RecordArtifact(name string, data []byte) (Ref, error) {
	return r.recordArtifact(Event{Type: EventArtifactRecorded, Name: name}, data)
}

// ContextManifestArtifactName is the stable journal name for the context
// manifest supplied to one stage attempt.
func ContextManifestArtifactName(stage string, attempt int) string {
	return fmt.Sprintf("context/%s-attempt-%d.json", stage, attempt)
}

// RecordStageArtifact is RecordArtifact for runner-authored artifacts tied to
// one stage attempt. The stage metadata keeps infra-retry artifacts out of the
// conformance set alongside the attempt that produced them.
func (r *Run) RecordStageArtifact(stage string, attempt int, class AttemptClass, name string, data []byte) (Ref, error) {
	return r.recordArtifact(Event{
		Type: EventArtifactRecorded, Stage: stage, Attempt: attempt, AttemptClass: class, Name: name,
	}, data)
}

func (r *Run) recordArtifact(ev Event, data []byte) (Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return Ref{}, ErrClosed
	}
	scrubbed := r.scrubber.Scrub(data)
	digest := Digest(scrubbed)
	relPath, err := artifactPath(digest)
	if err != nil {
		return Ref{}, err
	}
	ref, err := writeContentScrubbed(r.dir, relPath, scrubbed, digest)
	if err != nil {
		return Ref{}, fmt.Errorf("journal: record artifact %q: %w", ev.Name, err)
	}
	ev.Ref = &ref
	if err := r.append(ev); err != nil {
		return Ref{}, err
	}
	if err := r.checkpoint(); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

// RecordSpan scrubs data, stores it by content digest under spans/, and
// appends a span.recorded event — the within-stage trace/transcript capture
// GBO-020 requires (e.g. a harness adapter's transcript, issue #19). Mirrors
// RecordArtifact's content-addressed, scrub-then-write pattern; spans are
// excluded from conformance (§3.3) since harness/LLM output is not
// content-comparable across runners.
func (r *Run) RecordSpan(stage, name string, data []byte) (Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return Ref{}, ErrClosed
	}
	scrubbed := r.scrubber.Scrub(data)
	digest := Digest(scrubbed)
	relPath, err := spanPath(digest)
	if err != nil {
		return Ref{}, err
	}
	ref, err := writeContentScrubbed(r.dir, relPath, scrubbed, digest)
	if err != nil {
		return Ref{}, fmt.Errorf("journal: record span %q: %w", name, err)
	}
	if err := r.append(Event{Type: EventSpanRecorded, Stage: stage, Name: name, Ref: &ref}); err != nil {
		return Ref{}, err
	}
	if err := r.checkpoint(); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

// Close flushes and releases the events handle, and releases the per-run-dir
// lock (#243) so a waiting Create/Recover in another process can proceed. It
// does not write a run.finished event — the caller appends that explicitly
// so the terminal status is part of the log.
func (r *Run) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.events.Close()
	releaseRunLock(r.lock)
	return err
}

// checkpoint writes state.json atomically. Caller holds r.mu.
func (r *Run) checkpoint() error {
	st := State{
		Schema:       StateSchema,
		RunID:        r.id.RunID,
		Phase:        r.phase,
		MachineState: r.machineState,
		Reason:       r.reason,
		LastSeq:      r.seq,
		UpdatedAt:    r.now(),
	}
	return writeStateAtomic(r.dir, st)
}

// phaseFromStatus maps a run.finished status string to a RunPhase.
func phaseFromStatus(status string) RunPhase {
	switch RunPhase(status) {
	case PhaseCompleted, PhaseFailed, PhaseAborted, PhaseEscalated:
		return RunPhase(status)
	default:
		return PhaseCompleted
	}
}
