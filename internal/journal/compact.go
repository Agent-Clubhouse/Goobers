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
	// StaleGenerationsRemoved counts obsolete generation files reclaimed
	// after the pointer advanced (see instancegen.go).
	StaleGenerationsRemoved int
	// StaleGenerationCleanupErr is non-nil when one or more obsolete
	// generation files could not be removed. The compaction itself still
	// succeeded — this is a diagnostic for the caller to surface, not a
	// failure, since stranded generations only waste disk.
	StaleGenerationCleanupErr error
}

// CompactInstanceEvents rewrites the instance journal at dir, keeping complete
// records whose event time is at or after keepAfter, run.started records at
// or after keepRunStartsAfter, the init.completed marker, plus the latest
// scheduled trigger per workflow as restart checkpoints. A zero keepAfter
// keeps every record (a no-op on the journal — used when the caller only wants
// the surrounding db-vacuum maintenance). Records are preserved as their
// ORIGINAL raw line bytes, never re-marshaled, so any forward-compatible
// unknown fields survive compaction unchanged. Kept records retain their
// original seq, so the journal stays seq-monotonic.
//
// Compaction never rewrites dir's current events file in place: it writes the
// compacted content to a new generation (see instancegen.go) and advances the
// generation pointer, so a live InstanceLog (or any other reader with an
// already-open handle on the previous generation) is never disturbed — safe
// even while a daemon is appending, unlike an in-place rewrite. A missing
// journal is not an error. A torn final record (crash mid-append) is
// preserved verbatim so the next OpenInstanceLog repairs it as usual.
//
// dryRun computes what would be dropped (Kept/Dropped and the projected
// AfterBytes) without writing anything.
func CompactInstanceEvents(dir string, keepAfter, keepRunStartsAfter time.Time, dryRun bool) (InstanceEventsCompaction, error) {
	lock, err := acquireJournalLock(dir, "instance log")
	if err != nil {
		return InstanceEventsCompaction{}, err
	}
	defer releaseJournalLock(lock)

	currentGen, err := resolveInstanceEventsGeneration(dir)
	if err != nil {
		return InstanceEventsCompaction{}, err
	}
	path := filepath.Join(dir, instanceEventsFilename(currentGen))
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return InstanceEventsCompaction{}, nil
	}
	if err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: stat instance log: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: read instance log: %w", err)
	}
	result, compacted, err := compactInstanceEventsData(data, info.Size(), keepAfter, keepRunStartsAfter)
	if err != nil {
		return InstanceEventsCompaction{}, err
	}
	if result.Dropped == 0 {
		return result, nil // nothing aged out — leave the journal untouched
	}
	if dryRun {
		return result, nil
	}
	nextGen := currentGen + 1
	nextPath := filepath.Join(dir, instanceEventsFilename(nextGen))
	if err := writeFileSynced(nextPath, compacted, 0o644); err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: write next-generation instance log: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: fsync instance log dir: %w", err)
	}
	if err := advanceInstanceEventsPointer(dir, nextGen); err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: advance instance log pointer: %w", err)
	}
	if newInfo, statErr := os.Stat(nextPath); statErr == nil {
		result.AfterBytes = newInfo.Size()
	}
	result.StaleGenerationsRemoved, result.StaleGenerationCleanupErr = cleanupStaleInstanceEventsGenerations(dir, nextGen)
	return result, nil
}

func compactInstanceEventsData(
	data []byte,
	beforeBytes int64,
	keepAfter, keepRunStartsAfter time.Time,
) (InstanceEventsCompaction, []byte, error) {
	result := InstanceEventsCompaction{BeforeBytes: beforeBytes, AfterBytes: beforeBytes}

	// Only complete (newline-terminated) records are eligible; anything after
	// the last newline is a torn in-flight write, kept verbatim as the tail.
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return result, data, nil
	}
	complete := data[:end+1]
	tail := data[end+1:]

	type record struct {
		line       []byte
		time       time.Time
		triggerKey string
		runStarted bool
		initDone   bool
	}
	var records []record
	latestTrigger := make(map[string]int)
	for _, line := range bytes.SplitAfter(complete, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var meta struct {
			Schema   string    `json:"schema"`
			Seq      uint64    `json:"seq"`
			Time     time.Time `json:"time"`
			Type     EventType `json:"type"`
			Gaggle   string    `json:"gaggle"`
			Workflow string    `json:"workflow"`
			Reason   string    `json:"reason"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(line), &meta); err != nil {
			return InstanceEventsCompaction{}, nil, fmt.Errorf("journal: compact decode record: %w", err)
		}
		if meta.Schema != EventSchema {
			return InstanceEventsCompaction{}, nil, unsupportedPayloadSchema("event", meta.Schema, EventSchema)
		}
		rec := record{line: line, time: meta.Time}
		rec.runStarted = meta.Type == EventRunStarted
		rec.initDone = meta.Type == EventInitCompleted
		if meta.Type == EventTriggerFired && meta.Workflow != "" &&
			(meta.Reason == "" || meta.Reason == "scheduled" || bytes.HasPrefix([]byte(meta.Reason), []byte("catch-up "))) {
			rec.triggerKey = meta.Gaggle + "\x00" + meta.Workflow
			latestTrigger[rec.triggerKey] = len(records)
		}
		records = append(records, rec)
	}

	var kept bytes.Buffer
	for i, rec := range records {
		keepTriggerCheckpoint := rec.triggerKey != "" && latestTrigger[rec.triggerKey] == i
		keepBudgetHistory := rec.runStarted &&
			(keepRunStartsAfter.IsZero() || !rec.time.Before(keepRunStartsAfter))
		if !keepAfter.IsZero() && rec.time.Before(keepAfter) && !keepTriggerCheckpoint && !keepBudgetHistory && !rec.initDone {
			result.Dropped++
			continue
		}
		kept.Write(rec.line)
		result.Kept++
	}
	if result.Dropped == 0 {
		return result, data, nil
	}
	kept.Write(tail)
	result.AfterBytes = int64(kept.Len())
	return result, kept.Bytes(), nil
}

// Compact atomically checkpoints records at or after keepAfter while the
// instance log remains open — including by other independently-opened
// InstanceLog handles or unrelated readers, which never see this rewrite at
// all: it never touches the generation they have open (see instancegen.go).
// Scheduled trigger, recent run-start checkpoints, and the init completion
// marker are also retained so restart reconciliation and guided target
// classification preserve durable state.
func (l *InstanceLog) Compact(keepAfter, keepRunStartsAfter time.Time) (InstanceEventsCompaction, error) {
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

	currentGen, err := resolveInstanceEventsGeneration(l.dir)
	if err != nil {
		return InstanceEventsCompaction{}, err
	}
	path := filepath.Join(l.dir, instanceEventsFilename(currentGen))
	data, err := os.ReadFile(path)
	if err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: read instance log: %w", err)
	}
	result, compacted, err := compactInstanceEventsData(data, int64(len(data)), keepAfter, keepRunStartsAfter)
	if err != nil || result.Dropped == 0 {
		return result, err
	}
	nextGen := currentGen + 1
	nextPath := filepath.Join(l.dir, instanceEventsFilename(nextGen))
	if err := writeFileSynced(nextPath, compacted, 0o644); err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: checkpoint live instance log: %w", err)
	}
	if err := fsyncDir(l.dir); err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: fsync instance log dir: %w", err)
	}
	if err := advanceInstanceEventsPointer(l.dir, nextGen); err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: advance instance log pointer: %w", err)
	}
	if err := l.reopenFile(nextPath); err != nil {
		return InstanceEventsCompaction{}, err
	}
	result.StaleGenerationsRemoved, result.StaleGenerationCleanupErr = cleanupStaleInstanceEventsGenerations(l.dir, nextGen)
	return result, nil
}
