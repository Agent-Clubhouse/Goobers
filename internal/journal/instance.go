package journal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/platform/safeopen"
	"github.com/goobers/goobers/internal/readprobe"
)

// InstanceLog is the instance-level journal — <instance-root>/scheduler/events.jsonl
// (ARCHITECTURE.md §4/§6): scheduler decisions (trigger fired, run started, tick
// skipped with reason) and claim-ledger transitions, under the same envelope and
// append-only rules as a run journal, so the portal, telemetry, and Tutor read
// scheduling history the same way they read runs.
//
// Unlike a Run, an InstanceLog has no run.yaml, state.json, or artifacts — it is
// a single long-lived append-only log for the daemon's lifetime, opened once
// when the instance starts (e.g. by `goobers up`) rather than once per run.
type InstanceLog struct {
	dir      string
	scrubber Scrubber
	now      func() time.Time

	mu     sync.Mutex
	file   *os.File
	seq    uint64
	closed bool
}

// OpenInstanceLog opens the instance journal at dir, creating the directory and
// log if absent. Exactly like Recover for a run journal, a torn tail left by a
// prior crash is discarded and a corrective EventRepaired is appended, so even
// instance-level durability leaves a trace.
func OpenInstanceLog(dir string, opts ...Option) (*InstanceLog, RecoverReport, error) {
	cfg := newConfig(opts...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, RecoverReport{}, fmt.Errorf("journal: create instance log dir: %w", err)
	}

	lock, err := acquireJournalLock(dir, "instance log")
	if err != nil {
		return nil, RecoverReport{}, err
	}
	defer releaseJournalLock(lock)

	path, _, err := resolveInstanceEventsPath(dir)
	if err != nil {
		return nil, RecoverReport{}, err
	}
	_, statErr := os.Stat(path)
	eventsExisted := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, RecoverReport{}, fmt.Errorf("journal: stat instance log: %w", statErr)
	}
	events, tornBytes, err := readEvents(path)
	if err != nil {
		return nil, RecoverReport{}, err
	}
	report := RecoverReport{TornBytes: tornBytes}
	report.LastSeq = highestEventSeq(events)
	if err := truncateTornTail(path, tornBytes); err != nil {
		return nil, RecoverReport{}, err
	}
	if _, err := ensureInstanceLogID(dir, eventsExisted); err != nil {
		return nil, RecoverReport{}, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, RecoverReport{}, fmt.Errorf("journal: open instance log: %w", err)
	}
	l := &InstanceLog{dir: dir, scrubber: cfg.scrubber, now: cfg.now, file: f, seq: report.LastSeq}

	if tornBytes > 0 {
		if _, err := appendEvent(l.file, &l.seq, l.scrubber, l.now, Event{
			Type:   EventRepaired,
			Runner: map[string]any{"discardedBytes": tornBytes},
		}); err != nil {
			_ = f.Close()
			return nil, RecoverReport{}, err
		}
		report.Repaired = true
	}
	return l, report, nil
}

// Dir returns the instance log's directory.
func (l *InstanceLog) Dir() string { return l.dir }

// Append scrubs, stamps, writes, and fsyncs one event, exactly like Run.Append.
func (l *InstanceLog) Append(ev Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}

	lock, err := acquireJournalLock(l.dir, "instance log")
	if err != nil {
		return err
	}
	defer releaseJournalLock(lock)

	path, _, err := resolveInstanceEventsPath(l.dir)
	if err != nil {
		return err
	}
	if err := l.ensureActiveFile(path); err != nil {
		return err
	}

	// Allocate the sequence from a BOUNDED read of the journal's tail rather than
	// a full re-read (#1914). The read stays under the same cross-process lock,
	// because it is the sequence allocator and not merely an optimization
	// (§2.2, §14.11).
	highest, tornBytes, bytesRead, err := l.allocateSeqFromTail(path)
	if err != nil {
		return err
	}
	readprobe.RecordInstanceLogAppend(bytesRead)

	// Never regress below what this handle has already written. The tail read
	// establishes what OTHER writers have committed; this floor covers the one
	// case a bounded read cannot see — a sequence this handle allocated whose
	// record has since been overtaken in the window by a journal carrying the
	// historical duplicate/regressed sequences #530 left behind.
	if l.seq > highest {
		highest = l.seq
	}
	l.seq = highest
	if err := truncateTornTail(path, tornBytes); err != nil {
		return err
	}
	if tornBytes > 0 {
		if _, err := appendEvent(l.file, &l.seq, l.scrubber, l.now, Event{
			Type:   EventRepaired,
			Runner: map[string]any{"discardedBytes": tornBytes},
		}); err != nil {
			return err
		}
	}
	_, err = appendEvent(l.file, &l.seq, l.scrubber, l.now, ev)
	return err
}

func (l *InstanceLog) ensureActiveFile(path string) error {
	current, err := l.file.Stat()
	if err != nil {
		return fmt.Errorf("journal: stat open instance log: %w", err)
	}
	active, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("journal: stat active instance log: %w", err)
	}
	if os.SameFile(current, active) {
		return nil
	}
	return l.reopenFile(path)
}

func (l *InstanceLog) reopenFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("journal: reopen active instance log: %w", err)
	}
	old := l.file
	l.file = f
	if err := old.Close(); err != nil {
		return fmt.Errorf("journal: close rotated instance log: %w", err)
	}
	return nil
}

// allocateSeqFromTail reads the journal's tail to find the highest committed
// sequence, falling back to a full read when the bounded scan cannot establish
// it. Returns the sequence, the torn-tail size, and how many bytes were read.
//
// The fallback is what makes this safe: an exhausted budget means "I could not
// determine the highest sequence", and the only correct response to that is to
// read more — never to allocate from zero, which would duplicate every sequence
// in the journal (#530).
func (l *InstanceLog) allocateSeqFromTail(path string) (highest uint64, tornBytes, bytesRead int, err error) {
	// bytesRead is what was ACTUALLY read, not the budget. Reporting the budget
	// would make the §14.11 bound assert nothing: on any journal smaller than the
	// budget it would report the whole file and look unchanged, which is exactly
	// what the first version of this did.
	highest, tornBytes, bytesRead, err = tailSequence(path)
	switch {
	case err == nil:
		return highest, tornBytes, bytesRead, nil
	case errors.Is(err, errTailBudgetExhausted):
		// Fall through to the full recovery read below.
	default:
		return 0, 0, bytesRead, err
	}

	size := 0
	if info, statErr := os.Stat(path); statErr == nil {
		size = int(info.Size())
	}
	events, tornBytes, err := readEvents(path)
	if err != nil {
		return 0, 0, size, err
	}
	return highestEventSeq(events), tornBytes, size, nil
}

// Close flushes and releases the log's file handle.
func (l *InstanceLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	return l.file.Close()
}

// ReadInstanceLog returns every durably-committed event in the instance journal
// at dir, in seq order — the same read semantics as Reader.Events for a run.
func ReadInstanceLog(dir string) ([]Event, error) {
	path, _, err := resolveInstanceEventsPath(dir)
	if err != nil {
		return nil, err
	}
	events, _, err := readEvents(path)
	return events, err
}

// ReadInstanceLogAfterSeq returns the durably-committed events in the instance
// journal at dir whose sequence is greater than seq, scanning back only far
// enough to find that sequence instead of reading the whole journal.
//
// A caller that folds the result into state it already holds pays for what was
// appended since its previous read rather than for the journal's history, which
// is what keeps a recurring read (a status request, a metric) from growing with
// the instance's age (#3050). seq 0 returns every event, matching
// ReadInstanceLog.
func ReadInstanceLogAfterSeq(dir string, seq uint64) ([]Event, error) {
	path, _, err := resolveInstanceEventsPath(dir)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("journal: open instance log: %w", err)
	}
	defer func() { _ = file.Close() }()
	events, _, bytesRead, err := readEventsAfterSeq(file, seq)
	readprobe.RecordInstanceTailRead(bytesRead)
	if err != nil {
		return nil, err
	}
	if err := validateEventSchemas(events); err != nil {
		return nil, err
	}
	return events, nil
}

// InstanceLogState identifies the current instance-journal file. The file
// identity distinguishes removal and recreation at the same generation.
type InstanceLogState struct {
	Generation int
	Exists     bool
	identity   string
	info       os.FileInfo
}

// SameJournal reports whether two states describe the same generation file.
func (s InstanceLogState) SameJournal(other InstanceLogState) bool {
	if !s.Exists || !other.Exists || s.Generation != other.Generation {
		return false
	}
	if s.identity != "" || other.identity != "" {
		if s.identity == "" || s.identity != other.identity {
			return false
		}
	}
	return s.info != nil && other.info != nil && os.SameFile(s.info, other.info)
}

// ReadInstanceLogState returns the current instance journal's identity.
func ReadInstanceLogState(dir string) (InstanceLogState, error) {
	identity, err := readInstanceLogID(dir)
	if err != nil {
		return InstanceLogState{}, err
	}
	path, generation, err := resolveInstanceEventsPath(dir)
	if err != nil {
		return InstanceLogState{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return InstanceLogState{Generation: generation, identity: identity}, nil
		}
		return InstanceLogState{}, fmt.Errorf("journal: stat instance log generation: %w", err)
	}
	return InstanceLogState{Generation: generation, Exists: true, identity: identity, info: info}, nil
}

func ensureInstanceLogID(dir string, eventsExisted bool) (string, error) {
	if eventsExisted {
		identity, err := readInstanceLogID(dir)
		if err != nil || identity != "" {
			return identity, err
		}
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("journal: generate instance log identity: %w", err)
	}
	identity := hex.EncodeToString(random[:])
	if err := writeFileAtomic(filepath.Join(dir, fileInstanceLogID), []byte(identity+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("journal: persist instance log identity: %w", err)
	}
	return identity, nil
}

func readInstanceLogID(dir string) (string, error) {
	return readInstanceLogIDWith(dir, safeopen.OpenAt)
}

func readInstanceLogIDWith(dir string, openAt func(*os.File, string) (*os.File, error)) (identity string, err error) {
	path := filepath.Join(dir, fileInstanceLogID)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("journal: stat instance log identity: %w", err)
	}
	if !before.Mode().IsRegular() || before.Size() != 33 {
		return "", fmt.Errorf("journal: invalid instance log identity file %s", path)
	}
	// On Windows, FileInfo defers loading its stable volume/file identity until
	// os.SameFile is called. Resolve it before opening the file: otherwise a
	// pathname swap between Lstat and OpenAt can make both FileInfos resolve the
	// replacement and falsely appear identical. Other platforms perform this
	// comparison entirely from the metadata Lstat already captured.
	if !os.SameFile(before, before) {
		return "", fmt.Errorf("journal: instance log identity changed while opening %s", path)
	}
	directory, err := safeopen.Open(dir)
	if err != nil {
		return "", fmt.Errorf("journal: open instance log directory: %w", err)
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	f, err := openAt(directory, fileInstanceLogID)
	if err != nil {
		return "", fmt.Errorf("journal: open instance log identity: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	after, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("journal: stat opened instance log identity: %w", err)
	}
	if !after.Mode().IsRegular() || after.Size() != 33 || !os.SameFile(before, after) {
		return "", fmt.Errorf("journal: instance log identity changed while opening %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, 34))
	if err != nil {
		return "", fmt.Errorf("journal: read instance log identity: %w", err)
	}
	identity = strings.TrimSuffix(string(data), "\n")
	decoded, decodeErr := hex.DecodeString(identity)
	if len(data) != 33 || decodeErr != nil || len(decoded) != 16 || identity != strings.ToLower(identity) || identity == strings.Repeat("0", 32) {
		return "", fmt.Errorf("journal: invalid instance log identity in %s", path)
	}
	return identity, nil
}

func highestEventSeq(events []Event) uint64 {
	var highest uint64
	for _, ev := range events {
		if ev.Seq > highest {
			highest = ev.Seq
		}
	}
	return highest
}
