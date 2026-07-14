package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Reader is a read-only view over a run journal. `cat`/`jq`/`grep` remain the
// first-class debugging tools (§4); Reader is the typed path for the portal,
// telemetry rollup, and Tutor.
type Reader struct {
	dir string
}

// OpenRead opens an existing run directory for reading.
func OpenRead(dir string) (*Reader, error) {
	if _, err := os.Stat(filepath.Join(dir, fileRunYAML)); err != nil {
		return nil, fmt.Errorf("journal: not a run directory %q: %w", dir, err)
	}
	return &Reader{dir: dir}, nil
}

// Dir returns the run directory.
func (r *Reader) Dir() string { return r.dir }

// Identity parses run.yaml.
func (r *Reader) Identity() (RunIdentity, error) {
	b, err := os.ReadFile(filepath.Join(r.dir, fileRunYAML))
	if err != nil {
		return RunIdentity{}, fmt.Errorf("journal: read run.yaml: %w", err)
	}
	var id RunIdentity
	if err := yaml.Unmarshal(b, &id); err != nil {
		return RunIdentity{}, fmt.Errorf("journal: parse run.yaml: %w", err)
	}
	return id, nil
}

// State parses the state.json checkpoint. A missing or unparseable checkpoint is
// not fatal — it is derived and always reconstructable from the event log
// (Recover) — so callers that only need it as a hint can tolerate the error.
func (r *Reader) State() (State, error) {
	b, err := os.ReadFile(filepath.Join(r.dir, fileState))
	if err != nil {
		return State{}, fmt.Errorf("journal: read state.json: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("journal: parse state.json: %w", err)
	}
	return st, nil
}

// Phase reconstructs the run's phase from the event log — the source of
// truth this package documents (see reconstructPhase) — rather than the
// on-disk state.json checkpoint, which can lag it in the crash window
// between an event's fsync and the checkpoint rename that follows it in the
// same Append (#242). Every terminal-phase decision in this codebase
// (Resume, the daemon's resume scan, `run abort`) must use this, not
// State().Phase, which is only a checked hint.
func (r *Reader) Phase() (RunPhase, error) {
	events, err := r.Events()
	if err != nil {
		return "", err
	}
	return reconstructPhase(events), nil
}

// Events returns every durably-committed event in seq order. A torn final record
// from an interrupted append is skipped, not returned — the same rule Recover
// applies — so a reader never trips over a partial write. Use Recover to detect
// and repair the torn tail on the writer side.
func (r *Reader) Events() ([]Event, error) {
	events, _, err := readEvents(filepath.Join(r.dir, fileEvents))
	return events, err
}

// KnownSchema reports whether an event uses the schema version this build owns.
// Events written by an unknown future schema version still parse into the shared
// envelope (unknown fields are ignored by encoding/json); readers use this to
// decide whether to trust type-specific fields — the V0 forward-compat policy.
func (e Event) KnownSchema() bool { return e.Schema == EventSchema }

// ArtifactBytes reads and verifies a stored blob against its Ref.Digest,
// returning an error on any tamper/mismatch.
func (r *Reader) ArtifactBytes(ref Ref) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(r.dir, ref.Path))
	if err != nil {
		return nil, fmt.Errorf("journal: read blob %q: %w", ref.Path, err)
	}
	if got := Digest(b); got != ref.Digest {
		return nil, fmt.Errorf("journal: digest mismatch for %q: have %s want %s", ref.Path, got, ref.Digest)
	}
	return b, nil
}

// SpanBytes reads and verifies a stored span blob against its Ref.Digest —
// identical machinery to ArtifactBytes; a separate name keeps call sites at a
// harness-adapter/executor readable (spans/ vs artifacts/).
func (r *Reader) SpanBytes(ref Ref) ([]byte, error) {
	return r.ArtifactBytes(ref)
}

// RecoverReport describes what Recover found and did.
type RecoverReport struct {
	// LastSeq is the highest seq of a durably-committed event.
	LastSeq uint64
	// TornBytes is the size of a discarded partial final record (0 if clean).
	TornBytes int
	// Repaired is true when a torn tail was truncated and a corrective
	// repaired event was appended.
	Repaired bool
}

// Recover reopens a run directory for appending after a crash. It replays the
// event log, discards a torn final record if present, reconstructs seq and
// phase, and — when it repaired a torn tail — appends a corrective `repaired`
// event so even the repair leaves a trace (§4, append-only). The returned Run is
// ready to continue the run from where it left off.
func Recover(dir string, opts ...Option) (*Run, RecoverReport, error) {
	cfg := newConfig(opts...)
	rd, err := OpenRead(dir)
	if err != nil {
		return nil, RecoverReport{}, err
	}
	id, err := rd.Identity()
	if err != nil {
		return nil, RecoverReport{}, err
	}

	eventsPath := filepath.Join(dir, fileEvents)
	events, tornBytes, err := readEvents(eventsPath)
	if err != nil {
		return nil, RecoverReport{}, err
	}
	report := RecoverReport{TornBytes: tornBytes}
	if len(events) > 0 {
		report.LastSeq = events[len(events)-1].Seq
	}

	// Acquire the per-run-dir lock (#243) before any write below, including
	// the torn-tail truncation: a second Recover of the SAME run dir (e.g.
	// `goobers run abort` racing a live daemon's own resume of a crashed
	// run) must block here rather than open its own independent writer on
	// this events.jsonl. Held for the lifetime of the returned *Run,
	// released in Close.
	lock, err := acquireRunLock(dir)
	if err != nil {
		return nil, RecoverReport{}, err
	}

	// Truncate a torn partial final record so the next append starts on a clean
	// record boundary.
	if err := truncateTornTail(eventsPath, tornBytes); err != nil {
		releaseRunLock(lock)
		return nil, RecoverReport{}, err
	}

	f, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		releaseRunLock(lock)
		return nil, RecoverReport{}, fmt.Errorf("journal: reopen events log: %w", err)
	}
	r := &Run{
		dir:      dir,
		id:       id,
		scrubber: cfg.scrubber,
		now:      cfg.now,
		events:   f,
		lock:     lock,
		seq:      report.LastSeq,
		phase:    reconstructPhase(events),
	}
	diskSt, diskErr := rd.State()
	if diskErr == nil {
		r.machineState = diskSt.MachineState
	}

	// Heal state.json if it disagrees with what the log durably shows
	// happened. The crash window this closes is Append's own event-fsync-
	// then-checkpoint sequence (#242): a run.finished event can be fsynced
	// while the crash lands before the checkpoint rename that follows it in
	// the same Append call, leaving state.json still claiming
	// {running, <last stage/gate>}. The torn-tail repair below never
	// catches this case — a cleanly-fsynced run.finished leaves no torn
	// tail. A terminal reconstructed phase always implies MachineState
	// should be empty (State's own documented invariant), so healing here
	// clears it too. Only the terminal direction is healed: a non-terminal
	// reconstructed phase can't tell us the correct MachineState (that
	// requires the workflow Machine, which this package doesn't have), so
	// a missing/corrupt checkpoint for a still-running run is left for the
	// caller (Resume) to fall back on, not fabricated here.
	needsCheckpoint := tornBytes > 0
	if r.phase != PhaseRunning {
		if diskErr != nil || diskSt.Phase != r.phase || diskSt.MachineState != "" {
			r.machineState = ""
			needsCheckpoint = true
		}
	}

	if tornBytes > 0 {
		if err := r.append(Event{
			Type:   EventRepaired,
			Runner: map[string]any{"discardedBytes": tornBytes},
		}); err != nil {
			_ = f.Close()
			releaseRunLock(lock)
			return nil, RecoverReport{}, err
		}
		report.Repaired = true
	}
	if needsCheckpoint {
		if err := r.checkpoint(); err != nil {
			_ = f.Close()
			releaseRunLock(lock)
			return nil, RecoverReport{}, err
		}
	}
	return r, report, nil
}

// reconstructPhase derives the run phase from the event log — the source of
// truth — rather than trusting the derived state.json checkpoint.
func reconstructPhase(events []Event) RunPhase {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == EventRunFinished {
			return phaseFromStatus(events[i].Status)
		}
	}
	return PhaseRunning
}

// readEvents parses events.jsonl, returning the durably-committed events and the
// byte length of any torn partial final record. Every line up to the last
// newline is a completed, fsynced append and MUST parse; bytes after the last
// newline are an interrupted write and are reported as tornBytes, never returned
// as an event.
func readEvents(path string) ([]Event, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("journal: read events log: %w", err)
	}
	var complete, tail []byte
	if nl := bytes.LastIndexByte(data, '\n'); nl >= 0 {
		complete, tail = data[:nl+1], data[nl+1:]
	} else {
		tail = data // no complete record yet
	}

	var events []Event
	sc := bufio.NewScanner(bytes.NewReader(complete))
	sc.Buffer(make([]byte, 0, 64*1024), maxEventBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		// NUL bytes are never part of a scrubbed JSON event line — they are crash
		// zero-fill left by an interrupted append. Leading fill appears when a
		// prior NUL tail was not truncated and a later append ran past it (the
		// #116 cascade); strip it so a recoverable torn write is not mistaken for
		// fatal corruption. A line that is only fill collapses to empty and skips.
		if stripped := bytes.TrimLeft(line, "\x00"); len(stripped) != len(line) {
			line = bytes.TrimSpace(stripped)
		}
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// A completed (newline-terminated, fsynced) line that still fails to
			// parse after stripping crash fill is corruption beyond a torn tail —
			// surface it rather than hide it.
			return nil, 0, fmt.Errorf("journal: corrupt event at seq boundary: %w", err)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("journal: scan events log: %w", err)
	}
	// The torn tail is EVERY byte after the last complete record: a partial final
	// append and/or NUL zero-fill from a crash that extended the file without
	// flushing. All of it is torn and must be truncated. Discounting trailing
	// NULs from this length (as earlier code did) leaves zero-fill behind, which
	// the next append concatenates onto — fabricating a corrupt "complete" line
	// on the following recovery and bricking the journal (#116).
	return events, len(tail), nil
}

// maxEventBytes bounds a single event line during recovery scanning.
const maxEventBytes = 8 * 1024 * 1024
