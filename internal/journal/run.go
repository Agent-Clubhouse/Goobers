package journal

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ErrClosed is returned by writer operations after Close.
var ErrClosed = errors.New("journal: run is closed")

// ErrTerminalGenerationChanged means a continuation was checked against an
// older terminal event than the source currently records.
var ErrTerminalGenerationChanged = errors.New("terminal run generation changed")

// ErrImmutableSourceLockMissing means a continuation source cannot be locked
// without creating a file inside the source journal.
var ErrImmutableSourceLockMissing = errors.New("immutable source journal lock is missing")

// Run is a writer over a single run journal. It owns the append handle to
// events.jsonl and enforces the durability contract: every Append scrubs, writes
// a single line, and fsyncs before returning, so a completed event is never lost
// to a crash. All methods are safe for concurrent use.
type Run struct {
	dir      string
	id       RunIdentity
	scrubber Scrubber
	now      func() time.Time
	observer func(runID string, seq uint64)

	mu           sync.Mutex
	events       *os.File
	lock         *journalLock
	seq          uint64
	phase        RunPhase
	machineState string
	branch       int
	branches     []BranchCursor
	reason       string
	lastActivity time.Time
	appendErr    error
	closed       bool
}

// acquireRunLock takes a blocking exclusive lock on dir's lock file,
// serializing every writer that opens the same run directory (#243): the
// only file locks before this were the whole-instance up.lock and the claim
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
func acquireRunLock(dir string) (*journalLock, error) {
	return acquireJournalLock(dir, "run")
}

func acquireExistingRunLock(dir string) (*journalLock, error) {
	path := filepath.Join(dir, fileLock)
	lock, err := acquireExistingJournalLockPath(path, dir, "run")
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrImmutableSourceLockMissing, path)
	}
	return lock, err
}

// releaseRunLock unlocks and closes a lock file acquireRunLock returned. Safe
// to call with nil (a Run that never acquired one, e.g. a construction path
// that failed before acquireRunLock ran).
func releaseRunLock(held *journalLock) {
	releaseJournalLock(held)
}

// config holds constructor options.
type config struct {
	scrubber       Scrubber
	now            func() time.Time
	inputIntegrity map[string]apiv1.Integrity
	inputSource    map[string]string
	appendObserver func(runID string, seq uint64)
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

// WithAppendObserver reports each event after its checkpoint is durable.
// Observers maintain derived state and must handle their own failures.
func WithAppendObserver(observer func(runID string, seq uint64)) Option {
	return func(c *config) { c.appendObserver = observer }
}

// WithInputIntegrity labels immutable snapshots by logical input name. Inputs
// not present in grades default to trusted operator/config provenance.
func WithInputIntegrity(grades map[string]apiv1.Integrity) Option {
	return func(c *config) {
		c.inputIntegrity = make(map[string]apiv1.Integrity, len(grades))
		for name, grade := range grades {
			c.inputIntegrity[name] = grade
		}
	}
}

// WithInputSource records the caller-provided provenance for immutable input
// snapshots.
func WithInputSource(sources map[string]string) Option {
	return func(c *config) {
		c.inputSource = make(map[string]string, len(sources))
		for name, source := range sources {
			c.inputSource[name] = source
		}
	}
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

// Seq returns the highest event sequence durably appended to the run.
func (r *Run) Seq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// RunCreationStagingDir returns the hidden sibling directory where Create
// assembles unpublished runs before their atomic rename into runsDir.
func RunCreationStagingDir(runsDir string) string {
	return filepath.Join(filepath.Dir(runsDir), "."+filepath.Base(runsDir)+".creating")
}

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
	finalDir := filepath.Join(runsDir, id.RunID)
	runsDir = filepath.Dir(finalDir)
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return nil, fmt.Errorf("journal: create runs dir: %w", err)
	}
	publicationLock, err := acquireRunPublicationLock(finalDir)
	if err != nil {
		return nil, err
	}
	defer releaseJournalLock(publicationLock)
	stagingRoot := RunCreationStagingDir(runsDir)
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return nil, fmt.Errorf("journal: create run staging root: %w", err)
	}
	dir, err := os.MkdirTemp(stagingRoot, id.RunID+"-")
	if err != nil {
		return nil, fmt.Errorf("journal: create run staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, fmt.Errorf("journal: set run staging permissions: %w", err)
	}
	for _, sub := range []string{dirInputs, dirArtifacts, dirSpans} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("journal: create run subdir %q: %w", sub, err)
		}
	}

	// Serialize initialization inside the staging directory. The staged writer is
	// closed before rename so publication also works on Windows; Recover below
	// reacquires the same lock at its final path and refreshes state under it.
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
		if name == "" || name == "." || name == ".." || path.Base(name) != name || strings.ContainsAny(name, `/\`) {
			releaseRunLock(lock)
			return nil, fmt.Errorf("journal: invalid input name %q", name)
		}
		ref, err := writeContent(dir, path.Join(dirInputs, name), inputs[name], cfg.scrubber)
		if err != nil {
			releaseRunLock(lock)
			return nil, fmt.Errorf("journal: snapshot input %q: %w", name, err)
		}
		integrity := apiv1.IntegrityTrusted
		if configured, ok := cfg.inputIntegrity[name]; ok {
			integrity = configured
		}
		if !integrity.Valid() {
			releaseRunLock(lock)
			return nil, fmt.Errorf("journal: snapshot input %q has unknown integrity %q", name, integrity)
		}
		ref.Integrity = integrity
		id.Inputs = append(id.Inputs, InputRef{
			Name: name, Ref: ref, Source: cfg.inputSource[name], Integrity: integrity,
		})
	}
	if err := writeRunYAML(dir, id); err != nil {
		releaseRunLock(lock)
		return nil, err
	}
	if err := writeSchemaInfo(dir, SchemaInfo{
		Version:       CurrentSchemaVersion,
		MinimumBinary: minimumBinaryForJournalSchema(CurrentSchemaVersion),
	}); err != nil {
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
		observer: cfg.appendObserver,
		events:   events,
		lock:     lock,
		phase:    PhaseRunning,
	}
	if err := r.append(Event{
		Type: EventRunStarted, Status: string(PhaseRunning),
		SourceRunID: id.ContinuedFromRunID, SourceTerminalSeq: id.SourceTerminalSeq,
		Actor: id.Operator, Target: id.RequestedTarget,
	}); err != nil {
		_ = events.Close()
		releaseRunLock(lock)
		return nil, err
	}

	if err := r.checkpoint(); err != nil {
		_ = events.Close()
		releaseRunLock(lock)
		return nil, err
	}
	if err := r.Close(); err != nil {
		return nil, fmt.Errorf("journal: close staged run: %w", err)
	}
	if err := renameNoReplace(dir, finalDir); err != nil {
		if _, statErr := os.Stat(finalDir); statErr == nil {
			return nil, fmt.Errorf("journal: run %q already exists at %s", id.RunID, finalDir)
		}
		return nil, fmt.Errorf("journal: publish run directory: %w", err)
	}
	if err := fsyncDir(runsDir); err != nil {
		return nil, fmt.Errorf("journal: fsync runs dir: %w", err)
	}
	if err := fsyncDir(stagingRoot); err != nil {
		return nil, fmt.Errorf("journal: fsync run staging root: %w", err)
	}
	published, _, err := recover(finalDir, true, opts...)
	if err != nil {
		return nil, fmt.Errorf("journal: open published run: %w", err)
	}
	if published.observer != nil {
		published.observer(published.id.RunID, published.seq)
	}
	return published, nil
}

// ContinuationRequest is the journal-side creation contract for a continuation.
// Inputs are copied into the new journal and never read from the source journal.
type ContinuationRequest struct {
	RunID               string
	SourceRunID         string
	ExpectedTerminalSeq uint64
	Operator            string
	Target              string
	Inputs              map[string][]byte
	InputIntegrity      map[string]apiv1.Integrity
	InputSource         map[string]string
}

// CreateContinuation creates a distinct journal from a terminal source
// generation. The source is opened read-only and is never rewritten.
func CreateContinuation(runsDir string, req ContinuationRequest, opts ...Option) (*Run, error) {
	if !apiv1.ValidRunID(req.SourceRunID) {
		return nil, fmt.Errorf("journal: invalid source run id %q", req.SourceRunID)
	}
	if !apiv1.ValidRunID(req.RunID) {
		return nil, fmt.Errorf("journal: invalid continuation run id %q", req.RunID)
	}
	if req.ExpectedTerminalSeq == 0 {
		return nil, errors.New("journal: expected terminal sequence is required")
	}
	if strings.TrimSpace(req.Operator) == "" {
		return nil, errors.New("journal: operator is required")
	}
	if strings.TrimSpace(req.Target) == "" {
		return nil, errors.New("journal: continuation target is required")
	}
	sourceDir := filepath.Join(runsDir, req.SourceRunID)
	// Refuse legacy journals before taking their run lock. Lock acquisition can
	// create .lock, and OpenRead may migrate, so either operation would mutate a
	// source whose bytes must remain immutable.
	if _, err := OpenReadOnly(sourceDir); err != nil {
		return nil, fmt.Errorf("journal: inspect continuation source: %w", err)
	}
	sourceLock, err := acquireExistingRunLock(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("journal: lock continuation source: %w", err)
	}
	defer releaseRunLock(sourceLock)
	reader, err := OpenReadOnly(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("journal: open continuation source: %w", err)
	}
	phase, err := reader.Phase()
	if err != nil {
		return nil, fmt.Errorf("journal: read continuation source phase: %w", err)
	}
	if phase != PhaseCompleted && phase != PhaseFailed && phase != PhaseAborted && phase != PhaseEscalated {
		return nil, fmt.Errorf("journal: continuation source %q is not terminal (phase=%s)", req.SourceRunID, phase)
	}
	events, err := reader.Events()
	if err != nil {
		return nil, fmt.Errorf("journal: read continuation source events: %w", err)
	}
	var terminalSeq uint64
	for _, event := range events {
		if event.Type == EventRunFinished {
			terminalSeq = event.Seq
		}
	}
	if terminalSeq != req.ExpectedTerminalSeq {
		return nil, fmt.Errorf("%w: source %q is at sequence %d, expected %d", ErrTerminalGenerationChanged, req.SourceRunID, terminalSeq, req.ExpectedTerminalSeq)
	}
	id, err := reader.Identity()
	if err != nil {
		return nil, fmt.Errorf("journal: read continuation source identity: %w", err)
	}
	id.RunID = req.RunID
	id.ContinuedFromRunID = req.SourceRunID
	id.SourceTerminalSeq = req.ExpectedTerminalSeq
	id.Operator = strings.TrimSpace(req.Operator)
	id.RequestedTarget = strings.TrimSpace(req.Target)
	id.Trigger = Trigger{Kind: TriggerManual, Ref: req.SourceRunID}
	return Create(runsDir, id, req.Inputs, append(opts,
		WithInputIntegrity(req.InputIntegrity), WithInputSource(req.InputSource),
	)...)
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
	// Track lifecycle transitions so Close/Checkpoint reflect the last durable
	// run.finished or intervention event. Reason mirrors the terminal event's own
	// Error.Message, if any (#520) — empty for an ordinary business-outcome
	// terminal that carries no error, and cleared when a run resumes.
	switch ev.Type {
	case EventRunResumed, EventGateOverridden:
		r.phase = PhaseRunning
		r.machineState = ev.Target
		r.reason = ""
	case EventRunFinished:
		r.phase = phaseFromStatus(ev.Status)
		r.machineState = ""
		r.reason = ""
		if ev.Error != nil {
			r.reason = ev.Error.Message
		}
	case EventStageRerunRequested:
		r.phase = PhaseRunning
		r.machineState = ev.Stage
		r.reason = ""
	}
	if err := r.checkpoint(); err != nil {
		return err
	}
	if r.observer != nil {
		r.observer(r.id.RunID, r.seq)
	}
	return nil
}

// ClaimNotificationDelivery appends pending unless the journal already contains
// a completed or unresolved claim for the same idempotency key and sink.
func (r *Run) ClaimNotificationDelivery(pending apiv1.NotificationReceipt) (*apiv1.NotificationReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	events, _, err := readEvents(filepath.Join(r.dir, fileEvents))
	if err != nil {
		return nil, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		receipt := events[i].NotificationReceipt
		if receipt == nil || receipt.IdempotencyDigest != pending.IdempotencyDigest ||
			receipt.Sink.Kind != pending.Sink.Kind || receipt.Attempt == 0 {
			continue
		}
		if receipt.Status == apiv1.NotificationDelivered ||
			receipt.Status == apiv1.NotificationPending || receipt.Unresolved {
			existing := *receipt
			return &existing, nil
		}
		break
	}
	if err := r.append(Event{
		Type:                EventNotificationReceipt,
		NotificationReceipt: &pending,
	}); err != nil {
		return nil, err
	}
	if err := r.checkpoint(); err != nil {
		return nil, err
	}
	if r.observer != nil {
		r.observer(r.id.RunID, r.seq)
	}
	return nil, nil
}

// append is the lock-held core: assign seq, scrub the serialized line, write, fsync.
func (r *Run) append(ev Event) error {
	if r.appendErr != nil {
		return fmt.Errorf("journal: append blocked after prior write failure: %w", r.appendErr)
	}
	// Attribute the event to the active branch unless the caller set one.
	if ev.Branch == 0 {
		ev.Branch = r.branch
	}
	stamped, err := appendEvent(r.events, &r.seq, r.scrubber, r.now, ev)
	if err != nil {
		r.appendErr = err
	} else {
		r.lastActivity = stamped.Time
	}
	return err
}

// IfLastActivityBefore runs claim while holding the writer mutex only when no
// event has been appended at or after cutoff. It lets a live owner atomically
// claim a stale journal without racing a concurrent heartbeat append.
func (r *Run) IfLastActivityBefore(cutoff time.Time, claim func(time.Time)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lastActivity.Before(cutoff) {
		return false
	}
	claim(r.lastActivity)
	return true
}

// ObserveActivity makes live executor progress immediately visible to the
// watchdog without adding another durable event. Heartbeat events still
// coalesce that progress for crash recovery and inspection.
func (r *Run) ObserveActivity() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.lastActivity.Before(now) {
		r.lastActivity = now
	}
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

// SetBranch stamps every subsequent append with a parallel branch id, so an
// event is attributable to the branch that produced it without every call site
// having to thread the id. 0 restores the run's root branch, which is what
// every run that never forks stays on.
//
// An event that sets Branch explicitly keeps its own value.
func (r *Run) SetBranch(branch int) {
	r.mu.Lock()
	r.branch = branch
	r.mu.Unlock()
}

// SetBranchCursors records the per-branch resume positions used in the next
// checkpoint. Passing nil clears them, which is what the runner does once a
// parallel has joined and the run is single-cursor again.
//
// Cursors are stored in the caller's order, which is declaration order — the
// same order that assigns branch ids — so a checkpoint is deterministic.
func (r *Run) SetBranchCursors(cursors []BranchCursor) {
	r.mu.Lock()
	if len(cursors) == 0 {
		r.branches = nil
	} else {
		r.branches = append(r.branches[:0:0], cursors...)
	}
	r.mu.Unlock()
}

// BranchCursors returns the current per-branch resume positions, if any.
func (r *Run) BranchCursors() []BranchCursor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]BranchCursor(nil), r.branches...)
}

// Checkpoint writes state.json immediately, reflecting the current
// MachineState. Most transitions checkpoint implicitly as part of Append or
// RecordArtifact; call Checkpoint directly when a transition pauses without
// either.
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
	return r.RecordArtifactWithIntegrity(name, data, apiv1.IntegrityDerived)
}

// RecordArtifactWithIntegrity records an artifact with explicit provenance.
func (r *Run) RecordArtifactWithIntegrity(name string, data []byte, integrity apiv1.Integrity) (Ref, error) {
	return r.recordArtifact(Event{Type: EventArtifactRecorded, Name: name, Integrity: integrity}, data, 0)
}

// RecordArtifactBounded is RecordArtifact with a byte limit applied after
// journal redaction, at the same boundary that writes and digests the artifact.
func (r *Run) RecordArtifactBounded(name string, data []byte, maxBytes int) (Ref, error) {
	return r.RecordArtifactBoundedWithIntegrity(name, data, apiv1.IntegrityDerived, maxBytes)
}

// RecordArtifactBoundedWithIntegrity records a size-bounded artifact with
// explicit provenance.
func (r *Run) RecordArtifactBoundedWithIntegrity(name string, data []byte, integrity apiv1.Integrity, maxBytes int) (Ref, error) {
	if maxBytes <= 0 {
		return Ref{}, fmt.Errorf("journal: artifact %q byte limit must be positive", name)
	}
	return r.recordArtifact(Event{Type: EventArtifactRecorded, Name: name, Integrity: integrity}, data, maxBytes)
}

// RecordBranchArtifact records an artifact with explicit parallel-branch
// attribution instead of using the run's sequential branch default.
func (r *Run) RecordBranchArtifact(branch int, name string, data []byte) (Ref, error) {
	return r.RecordBranchArtifactWithIntegrity(branch, name, data, apiv1.IntegrityDerived)
}

// RecordBranchArtifactWithIntegrity records a branch-attributed artifact with
// explicit provenance.
func (r *Run) RecordBranchArtifactWithIntegrity(branch int, name string, data []byte, integrity apiv1.Integrity) (Ref, error) {
	return r.recordArtifact(Event{Type: EventArtifactRecorded, Branch: branch, Name: name, Integrity: integrity}, data, 0)
}

// RecordBranchArtifactBounded is RecordBranchArtifact with a post-redaction
// byte limit.
func (r *Run) RecordBranchArtifactBounded(branch int, name string, data []byte, maxBytes int) (Ref, error) {
	return r.RecordBranchArtifactBoundedWithIntegrity(branch, name, data, apiv1.IntegrityDerived, maxBytes)
}

// RecordBranchArtifactBoundedWithIntegrity records a size-bounded,
// branch-attributed artifact with explicit provenance.
func (r *Run) RecordBranchArtifactBoundedWithIntegrity(branch int, name string, data []byte, integrity apiv1.Integrity, maxBytes int) (Ref, error) {
	if maxBytes <= 0 {
		return Ref{}, fmt.Errorf("journal: artifact %q byte limit must be positive", name)
	}
	return r.recordArtifact(Event{Type: EventArtifactRecorded, Branch: branch, Name: name, Integrity: integrity}, data, maxBytes)
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
	return r.RecordStageArtifactWithIntegrity(stage, attempt, class, name, data, apiv1.IntegrityDerived)
}

// RecordStageArtifactWithIntegrity records a stage-scoped artifact with
// explicit provenance.
func (r *Run) RecordStageArtifactWithIntegrity(stage string, attempt int, class AttemptClass, name string, data []byte, integrity apiv1.Integrity) (Ref, error) {
	return r.recordArtifact(Event{
		Type: EventArtifactRecorded, Stage: stage, Attempt: attempt, AttemptClass: class, Name: name, Integrity: integrity,
	}, data, 0)
}

// RecordStageArtifactAnnotated is RecordStageArtifactWithIntegrity with
// caller-supplied runner.* annotations stamped on the artifact.recorded
// event. The Runner map is the sanctioned non-normative namespace, so the
// annotations never enter the conformance view — the live journal writer's
// idempotency key (livejournal.EmitKeyRunnerField) rides here.
func (r *Run) RecordStageArtifactAnnotated(stage string, attempt int, class AttemptClass, name string, data []byte, integrity apiv1.Integrity, runnerMeta map[string]any) (Ref, error) {
	return r.recordArtifact(Event{
		Type: EventArtifactRecorded, Stage: stage, Attempt: attempt, AttemptClass: class, Name: name, Integrity: integrity,
		Runner: copyRunnerMeta(runnerMeta),
	}, data, 0)
}

// RecordArtifactAnnotated is RecordArtifactWithIntegrity with caller-supplied
// runner.* annotations on the artifact.recorded event.
func (r *Run) RecordArtifactAnnotated(name string, data []byte, integrity apiv1.Integrity, runnerMeta map[string]any) (Ref, error) {
	return r.recordArtifact(Event{
		Type: EventArtifactRecorded, Name: name, Integrity: integrity, Runner: copyRunnerMeta(runnerMeta),
	}, data, 0)
}

// copyRunnerMeta isolates the recorded event from later caller mutation of
// the annotation map. nil in, nil out.
func copyRunnerMeta(meta map[string]any) map[string]any {
	if len(meta) == 0 {
		return nil
	}
	copied := make(map[string]any, len(meta))
	for k, v := range meta {
		copied[k] = v
	}
	return copied
}

// RecordBranchStageArtifact is RecordStageArtifact with explicit
// parallel-branch attribution.
func (r *Run) RecordBranchStageArtifact(branch int, stage string, attempt int, class AttemptClass, name string, data []byte) (Ref, error) {
	return r.RecordBranchStageArtifactWithIntegrity(branch, stage, attempt, class, name, data, apiv1.IntegrityDerived)
}

// RecordBranchStageArtifactWithIntegrity records a branch-attributed stage
// artifact with explicit provenance.
func (r *Run) RecordBranchStageArtifactWithIntegrity(branch int, stage string, attempt int, class AttemptClass, name string, data []byte, integrity apiv1.Integrity) (Ref, error) {
	return r.recordArtifact(Event{
		Type: EventArtifactRecorded, Branch: branch, Stage: stage, Attempt: attempt, AttemptClass: class, Name: name, Integrity: integrity,
	}, data, 0)
}

func (r *Run) recordArtifact(ev Event, data []byte, maxBytes int) (Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return Ref{}, ErrClosed
	}
	if !ev.Integrity.Valid() {
		return Ref{}, fmt.Errorf("journal: record artifact %q with unknown integrity %q", ev.Name, ev.Integrity)
	}
	scrubbed := r.scrubber.Scrub(data)
	if maxBytes > 0 && len(scrubbed) > maxBytes {
		return Ref{}, fmt.Errorf(
			"journal: artifact %q is %d bytes after redaction, exceeds %d-byte limit",
			ev.Name,
			len(scrubbed),
			maxBytes,
		)
	}
	digest := Digest(scrubbed)
	relPath, err := artifactPath(digest)
	if err != nil {
		return Ref{}, err
	}
	ref, err := writeContentScrubbed(r.dir, relPath, scrubbed, digest)
	if err != nil {
		return Ref{}, fmt.Errorf("journal: record artifact %q: %w", ev.Name, err)
	}
	ref.Integrity = ev.Integrity
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
	return r.recordSpan(0, stage, name, "", data)
}

// RecordSpanWithSchema records a span and identifies the schema of its
// content. RecordSpan remains the compatibility path for legacy unversioned
// transcript rows.
func (r *Run) RecordSpanWithSchema(stage, name, dataSchema string, data []byte) (Ref, error) {
	return r.recordSpan(0, stage, name, dataSchema, data)
}

// RecordSpanAnnotated is RecordSpanWithSchema with caller-supplied runner.*
// annotations on the span.recorded event (the live journal writer's
// idempotency key).
func (r *Run) RecordSpanAnnotated(stage, name, dataSchema string, data []byte, runnerMeta map[string]any) (Ref, error) {
	return r.recordSpanEvent(Event{
		Type: EventSpanRecorded, Stage: stage, Name: name, DataSchema: dataSchema, Runner: copyRunnerMeta(runnerMeta),
	}, data)
}

// RecordBranchSpanWithSchema is RecordSpanWithSchema for a stage running
// inside a parallel branch, attributing the resulting span.recorded event to
// that branch — the same branch-attribution seam RecordBranchArtifact et al.
// already provide for artifacts.
func (r *Run) RecordBranchSpanWithSchema(branch int, stage, name, dataSchema string, data []byte) (Ref, error) {
	return r.recordSpan(branch, stage, name, dataSchema, data)
}

func (r *Run) recordSpan(branch int, stage, name, dataSchema string, data []byte) (Ref, error) {
	return r.recordSpanEvent(Event{Type: EventSpanRecorded, Branch: branch, Stage: stage, Name: name, DataSchema: dataSchema}, data)
}

func (r *Run) recordSpanEvent(ev Event, data []byte) (Ref, error) {
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
		return Ref{}, fmt.Errorf("journal: record span %q: %w", ev.Name, err)
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
		Branches:     append([]BranchCursor(nil), r.branches...),
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
