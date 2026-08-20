package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Bounded tail read for instance-log sequence allocation (#1914).
//
// # What this replaces, and what it must not break
//
// InstanceLog.Append allocates its sequence by taking the cross-process journal
// lock and then reading the ENTIRE journal to find the highest seq. Measured:
// 1.30 s per append at the live instance's 324 MB, growing without bound, from a
// write path that shares the process with every read (design §2.2). Measured in
// the harness: every append reads 100.0% of the file, so the total across N
// appends is quadratic in N.
//
// The reread is not redundant. It happens under the lock and **is** the
// cross-process sequence allocator: #530's "two events sharing seq:5" is exactly
// what a per-handle in-memory counter reintroduces, and
// TestInstanceLogConcurrentAppendsAllocateUniqueMonotonicSequence opens 25
// independent handles for that reason. So the read stays, under the same lock,
// and only its *size* changes.
//
// # Why scanning to the last newline is not enough
//
// readEventRecords strips NUL crash-fill and skips a line that collapses to
// empty. The #116 cascade can therefore leave a **newline-terminated, fill-only
// tail after the last valid event**: reading back to the last newline would find
// a line that yields no event, recover seq=0, and reallocate from 1 — duplicating
// every sequence in the journal. The scan must continue to a line that parses as
// an event with a non-zero Seq.
//
// # Why a budget, and why the fallback is a full read
//
// The live journal contains records up to 2.66 MB (the #1414 residue), and
// maxEventBytes permits 8 MiB, so a scan can legitimately need to walk far. If
// the budget is exhausted before a valid event is found, the correct behaviour is
// to fall back to the full recovery read — **never to allocate from zero**, which
// is the failure mode that duplicates sequences.

// instanceTailBudget bounds how many bytes an append reads to allocate its
// sequence.
//
// Sized above the largest record observed on the live instance (2,661,279 bytes)
// so the fallback fires on genuine pathology rather than on ordinary history: a
// budget below the largest real record would send every append through the full
// read it exists to avoid, and the optimization would do nothing while looking
// like it worked.
const instanceTailBudget = 8 << 20 // 8 MiB, matching maxEventBytes

// tailChunkSize is how much is read per backward step.
const tailChunkSize = 64 << 10

// errTailBudgetExhausted signals that the bounded scan could not establish the
// highest sequence within its budget and the caller must fall back.
var errTailBudgetExhausted = errors.New("journal: instance log tail budget exhausted")

// tailSequence returns the highest event sequence in the journal's tail and the
// size of any torn final region, reading at most instanceTailBudget bytes.
//
// It returns errTailBudgetExhausted when the budget is reached without finding a
// parseable event, so the caller can fall back to a full read. Any other error is
// genuine corruption or I/O failure and is surfaced.
//
// The returned seq is the MAXIMUM over the events seen in the window, not simply
// the last one. Sequences are monotonic in file order under normal operation, so
// these are the same value — but the live instance carries 1,394 duplicate and
// 119 regressed sequences from #530's pre-fix era, and taking the maximum is
// strictly safer than trusting order on a journal that has demonstrably violated
// it.
func tailSequence(path string) (seq uint64, tornBytes, bytesRead int, err error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, 0, nil
		}
		return 0, 0, 0, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()

	info, err := file.Stat()
	if err != nil {
		return 0, 0, 0, err
	}
	size := info.Size()
	if size == 0 {
		return 0, 0, 0, nil
	}

	var (
		window    []byte // bytes read so far, ending at EOF
		start     = size // file offset the window begins at
		tailKnown bool
		tail      int
	)
	for start > 0 {
		chunkStart := start - tailChunkSize
		if chunkStart < 0 {
			chunkStart = 0
		}
		chunk := make([]byte, start-chunkStart)
		if _, readErr := file.ReadAt(chunk, chunkStart); readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, 0, 0, readErr
		}
		window = append(chunk, window...)
		start = chunkStart

		// The torn region is every byte after the last newline in the FILE. Once
		// a newline is present in the window we know where that boundary is; if
		// the window covers the whole file and there is still no newline, the
		// entire file is one torn partial record.
		if !tailKnown {
			if nl := bytes.LastIndexByte(window, '\n'); nl >= 0 {
				tail = len(window) - (nl + 1)
				tailKnown = true
			} else if start == 0 {
				tail = len(window)
				tailKnown = true
			}
		}

		if tailKnown {
			complete := window[:len(window)-tail]
			found, highest, scanErr := highestSeqInChunk(complete, start == 0)
			if scanErr != nil {
				return 0, 0, len(window), scanErr
			}
			if found {
				return highest, tail, len(window), nil
			}
		}

		// A whole-file scan that found nothing is a legitimately empty or
		// fill-only journal, not an exhausted budget: seq 0 is the right answer
		// and the caller must not fall back, because a full read would reach the
		// same conclusion more slowly.
		if start == 0 {
			return 0, tail, len(window), nil
		}
		if int64(len(window)) >= instanceTailBudget {
			return 0, 0, len(window), errTailBudgetExhausted
		}
	}
	return 0, tail, len(window), nil
}

// highestSeqInChunk parses the complete (newline-terminated) lines in buf and
// returns the highest non-zero sequence found.
//
// It applies exactly the acceptance rules readEventRecords does — strip leading
// NUL crash-fill, skip a line that collapses to empty, error on a complete line
// that still fails to parse — so a bounded scan and a full read agree about what
// counts as an event and about what counts as corruption.
//
// atFileStart tells it whether buf begins at file offset 0. It matters: the
// file's first line has no preceding newline, so without this the very first
// record in a journal is never examined, and a single-record journal would
// recover seq=0 and reallocate from 1.
func highestSeqInChunk(buf []byte, atFileStart bool) (found bool, highest uint64, err error) {
	for len(buf) > 0 {
		nl := bytes.LastIndexByte(buf, '\n')
		var line []byte
		if nl < 0 {
			if !atFileStart {
				// A leading partial line belonging to a chunk not yet read, so
				// not a complete record from this window's perspective.
				break
			}
			line, buf = buf, nil
		} else {
			line = buf[nl+1:]
			buf = buf[:nl]
		}

		line = bytes.TrimSpace(line)
		if stripped := bytes.TrimLeft(line, "\x00"); len(stripped) != len(line) {
			line = bytes.TrimSpace(stripped)
		}
		if len(line) == 0 {
			// Fill-only line — the #116 cascade shape. Keep scanning backwards;
			// stopping here is precisely the bug that would recover seq=0.
			continue
		}
		var ev Event
		if unmarshalErr := json.Unmarshal(line, &ev); unmarshalErr != nil {
			return false, 0, fmt.Errorf("journal: corrupt event at seq boundary: %w", unmarshalErr)
		}
		if ev.Schema != EventSchema {
			return false, 0, unsupportedPayloadSchema("event", ev.Schema, EventSchema)
		}
		if ev.Seq == 0 {
			continue
		}
		if ev.Seq > highest {
			highest = ev.Seq
		}
		found = true
	}
	return found, highest, nil
}
