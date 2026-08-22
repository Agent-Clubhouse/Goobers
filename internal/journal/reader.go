package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/readprobe"
)

// Reader is a read-only view over a run journal. `cat`/`jq`/`grep` remain the
// first-class debugging tools (§4); Reader is the typed path for the portal,
// telemetry rollup, and Tutor.
type Reader struct {
	dir    string
	schema SchemaInfo
}

// EventRecord retains both the parsed shared envelope and its original JSON,
// including unknown fields from the supported schema.
type EventRecord struct {
	Event Event
	Raw   json.RawMessage
}

// ErrNotRunDirectory identifies a directory with neither schema.json nor the
// legacy run.yaml marker. Callers scanning a runs root may skip only this error.
var ErrNotRunDirectory = errors.New("journal: not a run directory")

// ErrJournalMigrationRequired identifies a journal that cannot be inspected
// without first migrating its on-disk schema.
var ErrJournalMigrationRequired = errors.New("journal: schema migration required")

// OpenRead opens an existing run directory for reading, migrating an older
// journal schema when necessary.
func OpenRead(dir string) (*Reader, error) {
	_, hasManifest, err := readSchemaInfo(dir)
	if err != nil {
		return nil, err
	}
	if !hasManifest {
		if _, err := os.Stat(filepath.Join(dir, fileRunYAML)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w %q: %w", ErrNotRunDirectory, dir, err)
			}
			return nil, fmt.Errorf("journal: inspect run.yaml in %q: %w", dir, err)
		}
	}
	schema, err := ensureJournalSchema(dir)
	if err != nil {
		return nil, err
	}
	return &Reader{dir: dir, schema: schema}, nil
}

// OpenReadOnly opens an existing run directory without writing or migrating it.
// Journals that are not already at the current schema are refused with
// ErrJournalMigrationRequired.
func OpenReadOnly(dir string) (*Reader, error) {
	info, hasManifest, err := readSchemaInfo(dir)
	if err != nil {
		return nil, err
	}
	if !hasManifest {
		if _, err := os.Stat(filepath.Join(dir, fileRunYAML)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w %q: %w", ErrNotRunDirectory, dir, err)
			}
			return nil, fmt.Errorf("journal: inspect run.yaml in %q: %w", dir, err)
		}
	}
	done, err := admitJournalSchema(dir, info, hasManifest)
	if err != nil {
		return nil, err
	}
	if !done {
		return nil, fmt.Errorf(
			"%w: schema version %d must be migrated to version %d",
			ErrJournalMigrationRequired, info.Version, CurrentSchemaVersion,
		)
	}
	return &Reader{dir: dir, schema: info}, nil
}

// Dir returns the run directory.
func (r *Reader) Dir() string { return r.dir }

// Schema returns the persisted run-directory schema metadata.
func (r *Reader) Schema() SchemaInfo { return r.schema }

// openCurrentJournal revalidates a journal while its writer lock is already
// held. It must not attempt migration because migration acquires that same lock.
func openCurrentJournal(dir string) (*Reader, error) {
	return OpenReadOnly(dir)
}

// Identity parses run.yaml and rejects payload schemas this build does not own.
func (r *Reader) Identity() (RunIdentity, error) {
	b, err := os.ReadFile(filepath.Join(r.dir, fileRunYAML))
	if err != nil {
		return RunIdentity{}, fmt.Errorf("journal: read run.yaml: %w", err)
	}
	var id RunIdentity
	if err := yaml.Unmarshal(b, &id); err != nil {
		return RunIdentity{}, fmt.Errorf("journal: parse run.yaml: %w", err)
	}
	if id.Schema != RunSchema {
		return RunIdentity{}, unsupportedPayloadSchema("run", id.Schema, RunSchema)
	}
	return id, nil
}

// State parses the state.json checkpoint. A missing or unparseable checkpoint is
// not fatal — it is derived and always reconstructable from the event log
// (Recover) — so callers that only need it as a hint can tolerate the error.
// An unknown schema version is refused the same way Identity refuses one
// (#2054): a checkpoint reshaped by a newer build must not be silently
// misread as zero-valued fields of the current shape.
func (r *Reader) State() (State, error) {
	b, err := os.ReadFile(filepath.Join(r.dir, fileState))
	if err != nil {
		return State{}, fmt.Errorf("journal: read state.json: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("journal: parse state.json: %w", err)
	}
	if st.Schema != StateSchema {
		return State{}, unsupportedPayloadSchema("state", st.Schema, StateSchema)
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
	if readprobe.Enabled() {
		// Gated on the probe: this route reads the whole journal, so its cost is
		// the file size, and a stat to learn that must not be charged to the
		// production path. See readprobe.RunPhaseBytes (#2755).
		if info, statErr := os.Stat(filepath.Join(r.dir, fileEvents)); statErr == nil {
			readprobe.RecordRunPhaseBytes(int(info.Size()))
		}
	}
	events, err := r.Events()
	if err != nil {
		return "", err
	}
	return reconstructPhase(events), nil
}

// PhaseFromEvents reconstructs the phase from events the caller has already
// read, using the same rules as Phase.
//
// A caller that both renders events and tests for terminality must use this
// rather than calling Events and then Phase: those are two separate reads of a
// file the writer is still appending to, so the second can observe the terminal
// event that the first missed by microseconds. A poll loop written that way
// stops as soon as the second read reports terminal, having never rendered the
// final records — silently dropping the last stage's output (#1557).
func PhaseFromEvents(events []Event) RunPhase { return reconstructPhase(events) }

// Events returns every durably-committed event in seq order. A torn final record
// from an interrupted append is skipped, not returned — the same rule Recover
// applies — so a reader never trips over a partial write. Use Recover to detect
// and repair the torn tail on the writer side.
func (r *Reader) Events() ([]Event, error) {
	events, _, err := readEvents(filepath.Join(r.dir, fileEvents))
	if err == nil {
		err = validateEventSchemas(events)
	}
	return events, err
}

// EventRecords is Events with each complete record's original JSON retained.
func (r *Reader) EventRecords() ([]EventRecord, error) {
	records, _, err := readEventRecords(filepath.Join(r.dir, fileEvents))
	if err == nil {
		events := make([]Event, len(records))
		for i := range records {
			events[i] = records[i].Event
		}
		err = validateEventSchemas(events)
	}
	return records, err
}

// KnownSchema reports whether an event uses the schema version this build owns.
// Event consumers reject unsupported versions before interpreting type-specific
// fields.
func (e Event) KnownSchema() bool { return e.Schema == EventSchema }

// ArtifactBytes reads and verifies a stored blob against its Ref.Digest,
// returning an error on any tamper/mismatch. ref.Path is untrusted at
// resume time (#244): it round-trips through run.yaml, so a tampered
// InputRef.Path (e.g. "../../…") must be refused before it steers a read
// outside the run directory, not just joined blindly the way this used to
// — the same containment guard Redact already applies to the identical
// operation via containedBlobPath.
func (r *Reader) ArtifactBytes(ref Ref) ([]byte, error) {
	full, err := containedExistingBlobPath(r.dir, ref.Path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("journal: read blob %q: %w", ref.Path, err)
	}
	if got := Digest(b); got != ref.Digest {
		return nil, fmt.Errorf("journal: digest mismatch for %q: have %s want %s", ref.Path, got, ref.Digest)
	}
	return b, nil
}

// ArtifactByDigest reads an artifact from its canonical content-addressed path.
// Callers cannot steer this read with a journal-provided filesystem path.
func (r *Reader) ArtifactByDigest(digest string) ([]byte, error) {
	path, err := artifactPath(digest)
	if err != nil {
		return nil, err
	}
	return r.ArtifactBytes(Ref{Path: path, Digest: digest})
}

func containedExistingBlobPath(dir, relPath string) (string, error) {
	full, err := containedBlobPath(dir, relPath)
	if err != nil {
		return "", err
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("journal: resolve run directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("journal: resolve blob %q: %w", relPath, err)
	}
	relative, err := filepath.Rel(resolvedDir, resolved)
	if err != nil {
		return "", fmt.Errorf("journal: resolve blob containment %q: %w", relPath, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("journal: blob path %q resolves outside the run directory", relPath)
	}
	return resolved, nil
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
	// Events is the full ordered event log as it stood when the writer lock
	// was acquired — the exact state the returned Run continues from. A
	// caller deriving state from the journal (the live writer's rehydration)
	// MUST derive it from this view, never from a separate unlocked pre-read:
	// the log can change while acquireRunLock blocks behind another owner (a
	// stalled-sweep or `goobers run abort` terminalizer appending
	// run.finished), and state derived before the lock silently misses that.
	// The corrective repaired event appended for a torn tail (Repaired) is
	// not included; it postdates this read.
	Events []Event
}

// Recover reopens a run directory for appending after a crash. It replays the
// event log, discards a torn final record if present, reconstructs seq and
// phase, and — when it repaired a torn tail — appends a corrective `repaired`
// event so even the repair leaves a trace (§4, append-only). The returned Run is
// ready to continue the run from where it left off.
func Recover(dir string, opts ...Option) (*Run, RecoverReport, error) {
	return recover(dir, false, opts...)
}

func recover(dir string, publicationLocked bool, opts ...Option) (*Run, RecoverReport, error) {
	cfg := newConfig(opts...)
	rd, err := OpenRead(dir)
	if err != nil {
		return nil, RecoverReport{}, err
	}
	id, err := rd.Identity()
	if err != nil {
		return nil, RecoverReport{}, err
	}

	// The event log is read exactly once, under the locks below. An earlier
	// revision also pre-validated it here, before acquiring anything — a
	// second full O(N) parse whose result was discarded; corruption now
	// surfaces from the single under-lock read instead.
	eventsPath := filepath.Join(dir, fileEvents)

	var publicationLock *journalLock
	if !publicationLocked {
		publicationLock, err = acquireRunPublicationLock(dir)
		if err != nil {
			return nil, RecoverReport{}, err
		}
		defer releaseJournalLock(publicationLock)
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
	rd, err = openCurrentJournal(dir)
	if err != nil {
		releaseRunLock(lock)
		return nil, RecoverReport{}, err
	}
	id, err = rd.Identity()
	if err != nil {
		releaseRunLock(lock)
		return nil, RecoverReport{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, filePruning)); err == nil {
		releaseRunLock(lock)
		return nil, RecoverReport{}, fmt.Errorf("journal: run %q is reserved for telemetry pruning", id.RunID)
	} else if !errors.Is(err, os.ErrNotExist) {
		releaseRunLock(lock)
		return nil, RecoverReport{}, fmt.Errorf("journal: inspect telemetry pruning reservation: %w", err)
	}

	// The log may have changed while acquireRunLock blocked behind a live
	// writer (another Recover's repair, a terminalizer's run.finished). This
	// under-lock read is the ONLY event parse Recover performs and the one
	// every derived value below — seq, phase, reason, cursors, and the
	// report's Events — comes from, so truncation cannot apply a stale byte
	// count to a newer tail and callers cannot rehydrate from a pre-lock view.
	events, tornBytes, err := readEvents(eventsPath)
	if err != nil {
		releaseRunLock(lock)
		return nil, RecoverReport{}, err
	}

	// Truncate a torn partial final record so the next append starts on a clean
	// record boundary.
	if err := truncateTornTail(eventsPath, tornBytes); err != nil {
		releaseRunLock(lock)
		return nil, RecoverReport{}, err
	}

	report := RecoverReport{TornBytes: tornBytes, Events: events}
	if len(events) > 0 {
		report.LastSeq = events[len(events)-1].Seq
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
		observer: cfg.appendObserver,
		events:   f,
		lock:     lock,
		seq:      report.LastSeq,
		phase:    reconstructPhase(events),
		reason:   reconstructReason(events),
	}
	if len(events) > 0 {
		r.lastActivity = events[len(events)-1].Time
	}
	needsBranchCheckpoint := false
	diskSt, diskErr := rd.State()
	if diskErr == nil {
		r.machineState = diskSt.MachineState
		r.branches = diskSt.Branches
	}

	// Branch cursors are reconstructed from the log rather than trusted from
	// the checkpoint. state.json is a derived convenience and the crash window
	// it can be stranded in is exactly the one that matters here: a
	// branch.finished can be fsynced while the crash lands before the
	// checkpoint rename. The log is the source of truth (§4), so where the two
	// disagree the log wins.
	if logCursors, ok := reconstructBranchCursors(events); ok {
		if !equalBranchCursors(r.branches, logCursors) {
			r.branches = logCursors
			needsBranchCheckpoint = true
		}
	} else if len(r.branches) > 0 {
		// The log shows no live parallel (it never started, or it finished and
		// the run is single-cursor again) but the checkpoint still carries
		// cursors — a stranded checkpoint. Clear them.
		r.branches = nil
		needsBranchCheckpoint = true
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
	// clears it too, and Reason is compared too (#520) so a crash-stranded
	// refusal's WF-016 text still reaches state.json on the next open. The
	// terminal direction is always healed. A run.resumed event is the one
	// non-terminal case the journal can heal exactly because it durably names
	// the chosen MachineState; other running checkpoints still require the
	// workflow Machine and remain the caller's responsibility.
	needsCheckpoint := tornBytes > 0 || needsBranchCheckpoint
	if r.phase != PhaseRunning {
		if diskErr != nil || diskSt.Phase != r.phase || diskSt.MachineState != "" || diskSt.Reason != r.reason {
			r.machineState = ""
			needsCheckpoint = true
		}
	} else if resumed, ok := latestActiveResume(events); ok {
		// The resume target is exact only while the checkpoint has not advanced
		// beyond run.resumed. Once later stage/gate events exist, reconstructing
		// the current node requires the workflow Machine and stays Runner work.
		if diskErr == nil && diskSt.LastSeq <= resumed.Seq &&
			(diskSt.Phase != PhaseRunning || diskSt.MachineState != resumed.Target) {
			r.machineState = resumed.Target
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
		switch events[i].Type {
		case EventStageRerunRequested, EventRunResumed, EventGateOverridden:
			return PhaseRunning
		case EventRunFinished:
			return phaseFromStatus(events[i].Status)
		}
	}
	return PhaseRunning
}

// reconstructReason derives the terminal run's durable reason from the event
// log (#520) — the last run.finished event's own Error.Message, if it
// carried one (e.g. a WF-016 resume-refusal's digest-mismatch text). Empty
// for a non-terminal run or an ordinary terminal with no attached error,
// mirroring Run.Append's own reason tracking.
func reconstructReason(events []Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case EventStageRerunRequested, EventRunResumed, EventGateOverridden:
			return ""
		case EventRunFinished:
			if events[i].Error != nil {
				return events[i].Error.Message
			}
			return ""
		}
	}
	return ""
}

func latestActiveResume(events []Event) (Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case EventRunResumed, EventGateOverridden:
			return events[i], true
		case EventRunFinished:
			return Event{}, false
		}
	}
	return Event{}, false
}

// readEvents parses events.jsonl, returning the durably-committed events and the
// byte length of any torn partial final record. Every line up to the last
// newline is a completed, fsynced append and MUST parse; bytes after the last
// newline are an interrupted write and are reported as tornBytes, never returned
// as an event.
func readEvents(path string) ([]Event, int, error) {
	records, tornBytes, err := readEventRecords(path)
	if err != nil {
		return nil, 0, err
	}
	events := make([]Event, len(records))
	for i := range records {
		events[i] = records[i].Event
	}
	if err := validateEventSchemas(events); err != nil {
		return nil, 0, err
	}
	return events, tornBytes, nil
}

func readEventRecords(path string) ([]EventRecord, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("journal: read events log: %w", err)
	}
	return parseEventRecords(data)
}

func parseEventRecords(data []byte) ([]EventRecord, int, error) {
	var complete, tail []byte
	if nl := bytes.LastIndexByte(data, '\n'); nl >= 0 {
		complete, tail = data[:nl+1], data[nl+1:]
	} else {
		tail = data // no complete record yet
	}

	var records []EventRecord
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
		records = append(records, EventRecord{
			Event: ev,
			Raw:   append(json.RawMessage(nil), line...),
		})
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
	return records, len(tail), nil
}

// maxEventBytes bounds a single event line during recovery scanning.
const maxEventBytes = 8 * 1024 * 1024
