package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	seq          uint64
	phase        RunPhase
	machineState string
	closed       bool
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
		c.scrubber = nopScrubber{}
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
	cfg := newConfig(opts...)
	dir := filepath.Join(runsDir, id.RunID)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("journal: run %q already exists at %s", id.RunID, dir)
	}
	for _, sub := range []string{"", dirInputs, dirArtifacts, dirSpans} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("journal: create run dir: %w", err)
		}
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
			return nil, fmt.Errorf("journal: snapshot input %q: %w", name, err)
		}
		id.Inputs = append(id.Inputs, InputRef{Name: name, Ref: ref})
	}
	if err := writeRunYAML(dir, id); err != nil {
		return nil, err
	}

	events, err := os.OpenFile(filepath.Join(dir, fileEvents), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("journal: open events log: %w", err)
	}
	r := &Run{
		dir:      dir,
		id:       id,
		scrubber: cfg.scrubber,
		now:      cfg.now,
		events:   events,
		phase:    PhaseRunning,
	}
	if err := r.append(Event{Type: EventRunStarted, Status: string(PhaseRunning)}); err != nil {
		_ = events.Close()
		return nil, err
	}
	if err := r.checkpoint(); err != nil {
		_ = events.Close()
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
	if ev.Type == EventRunFinished {
		r.phase = phaseFromStatus(ev.Status)
		r.machineState = ""
	}
	return r.checkpoint()
}

// append is the lock-held core: assign seq, scrub the serialized line, write, fsync.
func (r *Run) append(ev Event) error {
	_, err := appendEvent(r.events, &r.seq, r.scrubber, r.now, ev)
	return err
}

// SetMachineState records the current state-machine node used in the next
// checkpoint. The runner calls this as it advances; it does not itself write.
func (r *Run) SetMachineState(state string) {
	r.mu.Lock()
	r.machineState = state
	r.mu.Unlock()
}

// RecordArtifact scrubs data, stores it by content digest under artifacts/, and
// appends an artifact.recorded event. Identical content deduplicates to one
// blob. The returned Ref's Digest commits to the scrubbed bytes.
func (r *Run) RecordArtifact(name string, data []byte) (Ref, error) {
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
		return Ref{}, fmt.Errorf("journal: record artifact %q: %w", name, err)
	}
	if err := r.append(Event{Type: EventArtifactRecorded, Name: name, Ref: &ref}); err != nil {
		return Ref{}, err
	}
	if err := r.checkpoint(); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

// Close flushes and releases the events handle. It does not write a
// run.finished event — the caller appends that explicitly so the terminal status
// is part of the log.
func (r *Run) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.events.Close()
}

// checkpoint writes state.json atomically. Caller holds r.mu.
func (r *Run) checkpoint() error {
	st := State{
		Schema:       StateSchema,
		RunID:        r.id.RunID,
		Phase:        r.phase,
		MachineState: r.machineState,
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
