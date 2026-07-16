package journal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// InstanceLog is the instance-level journal — <instance-root>/scheduler/events.jsonl
// (ARCHITECTURE.md §4/§6): scheduler decisions (trigger fired, run started, tick
// skipped with reason) and claim-ledger transitions, under the same envelope and
// append-only rules as a run journal, so the portal, telemetry, and Tutor read
// scheduling history the same way they read runs.
//
// Unlike a Run, an InstanceLog has no run.yaml, state.json, or artifacts — it is
// a single long-lived append-only log for the instance's lifetime. The daemon
// keeps one handle open while stage subprocesses may open independent handles;
// appends are serialized across all of them.
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

	lock, err := acquireInstanceLogLock(dir)
	if err != nil {
		return nil, RecoverReport{}, err
	}

	path := filepath.Join(dir, fileEvents)
	events, tornBytes, err := readEvents(path)
	if err != nil {
		releaseInstanceLogLock(lock)
		return nil, RecoverReport{}, err
	}
	report := RecoverReport{TornBytes: tornBytes}
	report.LastSeq = highestEventSeq(events)
	if err := truncateTornTail(path, tornBytes); err != nil {
		releaseInstanceLogLock(lock)
		return nil, RecoverReport{}, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		releaseInstanceLogLock(lock)
		return nil, RecoverReport{}, fmt.Errorf("journal: open instance log: %w", err)
	}
	releaseInstanceLogLock(lock)
	l := &InstanceLog{dir: dir, scrubber: cfg.scrubber, now: cfg.now, file: f, seq: report.LastSeq}

	if tornBytes > 0 {
		if err := l.Append(Event{
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

	lock, err := acquireInstanceLogLock(l.dir)
	if err != nil {
		return err
	}
	defer releaseInstanceLogLock(lock)

	path := filepath.Join(l.dir, fileEvents)
	lastSeq, tornBytes, err := readInstanceLogTail(path)
	if err != nil {
		return err
	}
	l.seq = lastSeq
	if tornBytes > 0 {
		if err := truncateTornTail(path, tornBytes); err != nil {
			return err
		}
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
	events, _, err := readEvents(filepath.Join(dir, fileEvents))
	return events, err
}

func highestEventSeq(events []Event) uint64 {
	var highest uint64
	for _, event := range events {
		if event.Seq > highest {
			highest = event.Seq
		}
	}
	return highest
}

const (
	instanceTailInitialBytes = 64 * 1024
	instanceTailMaxBytes     = 3*maxEventBytes + 3
)

// readInstanceLogTail finds the newest usable sequence without rescanning the
// instance's lifetime journal. It expands a bounded suffix only when a large
// event, torn write, or corrupt completed record obscures the preceding event.
func readInstanceLogTail(path string) (uint64, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("journal: open instance log tail: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return 0, 0, fmt.Errorf("journal: stat instance log tail: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return 0, 0, nil
	}

	readSize := min(size, int64(instanceTailInitialBytes))
	maxRead := min(size, int64(instanceTailMaxBytes))
	for {
		offset := size - readSize
		data := make([]byte, int(readSize))
		if _, err := f.ReadAt(data, offset); err != nil {
			return 0, 0, fmt.Errorf("journal: read instance log tail: %w", err)
		}
		seq, tornBytes, needMore := parseInstanceLogTail(data, offset)
		if !needMore {
			return seq, tornBytes, nil
		}
		if readSize == maxRead {
			return 0, 0, fmt.Errorf("journal: no sequence found in last %d bytes", readSize)
		}
		readSize = min(readSize*2, maxRead)
	}
}

func parseInstanceLogTail(data []byte, offset int64) (uint64, int, bool) {
	completeEnd := len(data)
	tornBytes := 0
	if data[len(data)-1] != '\n' {
		newline := bytes.LastIndexByte(data, '\n')
		if newline < 0 {
			if offset > 0 {
				return 0, 0, true
			}
			return 0, len(data), false
		}
		completeEnd = newline + 1
		tornBytes = len(data) - completeEnd
	}

	for end := completeEnd; end > 0; {
		if data[end-1] == '\n' {
			end--
		}
		start := bytes.LastIndexByte(data[:end], '\n') + 1
		if start == 0 && offset > 0 {
			return 0, 0, true
		}

		line := bytes.TrimSpace(bytes.TrimLeft(data[start:end], "\x00"))
		var event struct {
			Seq uint64 `json:"seq"`
		}
		if len(line) > 0 && json.Unmarshal(line, &event) == nil && event.Seq > 0 {
			return event.Seq, tornBytes, false
		}
		end = start
	}
	return 0, tornBytes, false
}

func acquireInstanceLogLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, fileLock), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("journal: open instance log lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("journal: acquire instance log lock: %w", err)
	}
	return f, nil
}

func releaseInstanceLogLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
