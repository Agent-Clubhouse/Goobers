package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// tickSkipRetry remembers a CompactTickSkips pass that could not bring a
// generation under budget, so the next pass does not re-parse that same
// generation until it has grown by a meaningful amount.
type tickSkipRetry struct {
	generation int
	atBytes    int64
}

// CompactTickSkips bounds the instance journal's growth from tick.skipped
// records, the decision a busy scheduler journals for every workflow on every
// tick it cannot start (#7031). The retention window Compact applies is the
// telemetry window — 90 days by default — and an instance with saturated
// workflows accumulates hundreds of megabytes of skips well inside it.
//
// The pass only runs once the current generation exceeds maxBytes, and only
// drops tick.skipped records older than keepAfter: every other record keeps
// the retention Compact gives it. Below the budget it is a stat, not a read,
// so it does not scale with history. When the remaining records alone exceed
// the budget, the pass is fenced off for that generation until it grows by a
// further quarter of the budget, so a journal that cannot get under budget is
// not re-parsed on every pass.
//
// Like Compact, it writes a new generation instead of rewriting the open one,
// and kept records keep their original bytes and sequence.
func (l *InstanceLog) CompactTickSkips(maxBytes int64, keepAfter time.Time) (InstanceEventsCompaction, error) {
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
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return InstanceEventsCompaction{}, nil
	}
	if err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: stat instance log: %w", err)
	}
	result := InstanceEventsCompaction{BeforeBytes: info.Size(), AfterBytes: info.Size()}
	if !l.tickSkipCompactionDue(currentGen, info.Size(), maxBytes) {
		return result, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return InstanceEventsCompaction{}, fmt.Errorf("journal: read instance log: %w", err)
	}
	result, compacted, err := dropTickSkipsBefore(data, keepAfter)
	if err != nil {
		return InstanceEventsCompaction{}, err
	}
	if result.Dropped > 0 {
		if err := l.installCompactedGeneration(currentGen, compacted, &result); err != nil {
			return InstanceEventsCompaction{}, err
		}
		currentGen++
	}
	l.tickSkipRetry = tickSkipRetry{}
	if result.AfterBytes > maxBytes {
		l.tickSkipRetry = tickSkipRetry{generation: currentGen, atBytes: result.AfterBytes}
	}
	return result, nil
}

// tickSkipCompactionDue reports whether a generation of size bytes is over
// budget and not fenced by an earlier pass that left it over budget.
func (l *InstanceLog) tickSkipCompactionDue(generation int, size, maxBytes int64) bool {
	if maxBytes <= 0 || size <= maxBytes {
		return false
	}
	retry := l.tickSkipRetry
	return retry.atBytes == 0 || retry.generation != generation || size >= retry.atBytes+maxBytes/4
}

// dropTickSkipsBefore removes complete tick.skipped records older than
// keepAfter from data. Anything after the final newline is a torn in-flight
// write and is kept verbatim, as compactInstanceEventsData does.
func dropTickSkipsBefore(data []byte, keepAfter time.Time) (InstanceEventsCompaction, []byte, error) {
	size := int64(len(data))
	result := InstanceEventsCompaction{BeforeBytes: size, AfterBytes: size, BytesRead: size}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return result, data, nil
	}
	kept := make([]byte, 0, len(data))
	// The record holding the highest sequence is always kept: a handle opened
	// later takes its next sequence from the generation's tail, so dropping it
	// would let new appends reuse sequences readers have already seen.
	var (
		maxSeq     uint64
		maxLine    []byte
		maxDropped bool
	)
	for rest := data[:end+1]; len(rest) > 0; {
		n := bytes.IndexByte(rest, '\n') + 1
		line := rest[:n]
		rest = rest[n:]
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var meta struct {
			Schema string    `json:"schema"`
			Seq    uint64    `json:"seq"`
			Time   time.Time `json:"time"`
			Type   EventType `json:"type"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(line), &meta); err != nil {
			return InstanceEventsCompaction{}, nil, fmt.Errorf("journal: compact decode record: %w", err)
		}
		if meta.Schema != EventSchema {
			return InstanceEventsCompaction{}, nil, unsupportedPayloadSchema("event", meta.Schema, EventSchema)
		}
		drop := meta.Type == EventTickSkipped && meta.Time.Before(keepAfter)
		if maxLine == nil || meta.Seq >= maxSeq {
			maxSeq, maxLine, maxDropped = meta.Seq, line, drop
		}
		if drop {
			result.Dropped++
			continue
		}
		kept = append(kept, line...)
		result.Kept++
	}
	if maxDropped {
		// Highest sequence, so appending it last keeps the file seq-ordered.
		kept = append(kept, maxLine...)
		result.Dropped--
		result.Kept++
	}
	if result.Dropped == 0 {
		return result, data, nil
	}
	kept = append(kept, data[end+1:]...)
	result.AfterBytes = int64(len(kept))
	return result, kept, nil
}
