package journal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// InstanceEventsCompaction reports what CompactInstanceEvents changed.
type InstanceEventsCompaction struct {
	BeforeBytes int64
	AfterBytes  int64
	Kept        int
	Dropped     int
}

// CompactInstanceEvents rewrites the instance journal at <dir>/events.jsonl in
// place, keeping only complete records whose event time is at or after
// keepAfter. A zero keepAfter keeps every record (a no-op on the journal — used
// when the caller only wants the surrounding db-vacuum maintenance). Records
// are preserved as their ORIGINAL raw line bytes, never re-marshaled, so any
// forward-compatible unknown fields survive compaction unchanged; only seq and
// time are parsed, and only to decide what to keep. Kept records retain their
// original seq, so the journal stays seq-monotonic and a consumer resuming by
// seq or byte offset (the rollup's incremental scheduler ingest, #1411) simply
// re-reads the now-shorter file.
//
// The caller MUST ensure no daemon is appending: a live InstanceLog holds an
// O_APPEND handle, and replacing the file out from under it would strand its
// writes on the unlinked inode. CompactInstanceEvents takes the journal lock
// defensively, but the lock cannot close another process's open handle. A
// missing journal is not an error. A torn final record (crash mid-append) is
// preserved verbatim so the next OpenInstanceLog repairs it as usual.
//
// dryRun computes what would be dropped (Kept/Dropped and the projected
// AfterBytes) without rewriting the file.
func CompactInstanceEvents(dir string, keepAfter time.Time, dryRun bool) (InstanceEventsCompaction, error) {
	path := filepath.Join(dir, fileEvents)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return InstanceEventsCompaction{}, nil
	}
	if err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: stat instance log: %w", err)
	}

	lock, err := acquireJournalLock(dir, "instance log")
	if err != nil {
		return InstanceEventsCompaction{}, err
	}
	defer releaseJournalLock(lock)

	data, err := os.ReadFile(path)
	if err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: read instance log: %w", err)
	}
	result, compacted, err := compactInstanceEventsData(data, info.Size(), keepAfter)
	if err != nil {
		return InstanceEventsCompaction{}, err
	}
	if result.Dropped == 0 {
		return result, nil // nothing aged out — leave the file untouched
	}
	if dryRun {
		return result, nil
	}
	if err := WriteFileAtomic(path, compacted, 0o644); err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: rewrite compacted instance log: %w", err)
	}
	if newInfo, statErr := os.Stat(path); statErr == nil {
		result.AfterBytes = newInfo.Size()
	}
	return result, nil
}

func compactInstanceEventsData(data []byte, beforeBytes int64, keepAfter time.Time) (InstanceEventsCompaction, []byte, error) {
	result := InstanceEventsCompaction{BeforeBytes: beforeBytes, AfterBytes: beforeBytes}

	// Only complete (newline-terminated) records are eligible; anything after
	// the last newline is a torn in-flight write, kept verbatim as the tail.
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return result, data, nil
	}
	complete := data[:end+1]
	tail := data[end+1:]

	var kept bytes.Buffer
	for _, line := range bytes.SplitAfter(complete, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var meta struct {
			Seq  uint64    `json:"seq"`
			Time time.Time `json:"time"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(line), &meta); err != nil {
			return InstanceEventsCompaction{}, nil, fmt.Errorf("journal: compact decode record: %w", err)
		}
		if !keepAfter.IsZero() && meta.Time.Before(keepAfter) {
			result.Dropped++
			continue
		}
		kept.Write(line)
		result.Kept++
	}
	if result.Dropped == 0 {
		return result, data, nil
	}
	kept.Write(tail)
	result.AfterBytes = int64(kept.Len())
	return result, kept.Bytes(), nil
}

// Compact drops records older than keepAfter while the instance log remains
// open. It rewrites the existing inode under both writer locks so concurrent
// append handles continue writing to the active journal.
func (l *InstanceLog) Compact(keepAfter time.Time) (InstanceEventsCompaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return InstanceEventsCompaction{}, ErrClosed
	}

	lock, err := acquireJournalLock(l.dir, "instance log")
	if err != nil {
		return InstanceEventsCompaction{}, err
	}
	defer releaseJournalLock(lock)

	path := filepath.Join(l.dir, fileEvents)
	data, err := os.ReadFile(path)
	if err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: read instance log: %w", err)
	}
	result, compacted, err := compactInstanceEventsData(data, int64(len(data)), keepAfter)
	if err != nil || result.Dropped == 0 {
		return result, err
	}
	if err := rewriteOpenInstanceLog(l.file, compacted); err != nil {
		if restoreErr := rewriteOpenInstanceLog(l.file, data); restoreErr != nil {
			return InstanceEventsCompaction{}, fmt.Errorf("journal: compact live instance log: %w (restore failed: %v)", err, restoreErr)
		}
		return InstanceEventsCompaction{}, fmt.Errorf("journal: compact live instance log: %w", err)
	}
	return result, nil
}

func rewriteOpenInstanceLog(file *os.File, data []byte) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}
