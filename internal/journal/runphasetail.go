package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/readprobe"
)

// Bounded tail read for run-phase reconstruction (#2755).
//
// # What this replaces, and why
//
// Phase reads the ENTIRE run journal and JSON-parses every record, only to
// look backwards from the end for the one event that decides the phase. The
// daemon's startup reconciliation does that for every run directory under
// `gaggles/*/runs/` — 54,333 of them on the self-hosting instance — so boot
// costs O(all runs ever) journal parses to discover the handful that are still
// running. Measured: minutes of ~200% CPU before the daemon is usable, growing
// with history that will never be running again.
//
// The decisive event is, by construction, at or near the END of the journal:
// reconstructPhase scans backwards and stops at the first run.finished,
// run.resumed, stage.rerun.requested, gate.overridden, or executed terminal
// gate it meets. A terminal run's decisive record is at the tail. So the phase
// is almost always decidable from the last few kilobytes.
//
// # Why this is not a second, weaker source of truth
//
// PhaseBounded answers with reconstructPhase's rules over the same event log —
// it just stops reading once the answer cannot change. The journal is still
// the source of truth; state.json is still never consulted (it can lag a
// crash-fsynced run.finished, #242, which is exactly why the scan does not
// take the cheap route of trusting the checkpoint). Where the bounded scan
// cannot reach a decision within its budget it falls back to the full read
// rather than guessing, so its answer is Phase's answer by construction.
// TestPhaseBoundedAgreesWithPhase pins that equivalence over the fixture
// corpus.
//
// # What it does NOT preserve
//
// Phase surfaces corruption anywhere in the journal, because it parses every
// record. The bounded scan parses every record it walks — so it still refuses a
// corrupt journal it can see — but it stops at the decisive record, and a
// corrupt record buried in the history BEFORE that one no longer fails the
// active-run scan. That narrowing is deliberate: one unreadable historical
// record in a run that finished months ago must not stop the daemon from
// booting, and the full read still runs on every path that renders or resumes
// the run.

// runPhaseTailBudget bounds how many bytes PhaseBounded reads before giving up
// and falling back to the full read.
//
// Sized at maxEventBytes so a single legitimately huge record — the shape that
// makes a journal expensive in the first place — can still be walked past
// without forcing the fallback. A budget below the largest real record would
// send every run through the full read this exists to avoid, and the
// optimization would do nothing while looking like it worked.
const runPhaseTailBudget = maxEventBytes // 8 MiB

// runPhaseChunkSize is the first backward step. Small on purpose: a terminal
// run's run.finished is its last record, so one chunk decides essentially every
// run, and reading 64 KiB per run to answer from the last 400 bytes multiplies
// the boot scan's I/O by two orders of magnitude for nothing.
const runPhaseChunkSize = 16 << 10

// runPhaseMaxChunkSize caps the geometric growth of the backward step, so a run
// that genuinely needs a deep scan pays a bounded number of reads rather than
// one read per 16 KiB.
const runPhaseMaxChunkSize = 1 << 20

// errPhaseTailBudgetExhausted signals that the bounded scan could not decide the
// phase within its budget and the caller must fall back to the full read.
var errPhaseTailBudgetExhausted = errors.New("journal: run phase tail budget exhausted")

// PhaseBounded returns the same phase Phase does, reading a bounded tail of the
// event log instead of all of it. Cancellation is checked between bounded
// filesystem reads and event records, including during the full-read fallback.
//
// Use it where phase is needed for many runs at once and the journals are not
// otherwise being read — the daemon's active-run reconciliation is the case it
// exists for. A caller that is going to read the events anyway must use
// PhaseFromEvents on the records it already has (#1557), not this.
func (r *Reader) PhaseBounded(ctx context.Context) (RunPhase, error) {
	path := filepath.Join(r.dir, fileEvents)
	phase, decided, bytesRead, err := tailPhaseContext(ctx, path)
	readprobe.RecordRunPhaseBytes(bytesRead)
	if err == nil && decided {
		return phase, nil
	}
	if err != nil && !errors.Is(err, errPhaseTailBudgetExhausted) {
		return "", err
	}
	return phaseContext(ctx, path)
}

// tailPhase scans events.jsonl backwards for the record reconstructPhase would
// stop at, reading at most runPhaseTailBudget bytes.
//
// It reports decided=false with errPhaseTailBudgetExhausted when the budget runs
// out first. A whole-file scan that finds no decisive record is decided: an
// empty or never-finished journal is PhaseRunning, and a full read would reach
// the same answer more slowly.
//
// bytesRead is how much of the journal the scan actually touched, so a test can
// assert the bound in work rather than in wall time.
func tailPhaseContext(ctx context.Context, path string) (phase RunPhase, decided bool, bytesRead int, err error) {
	if err := ctx.Err(); err != nil {
		return "", false, 0, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No log yet is the same "nothing terminal happened" reconstructPhase
			// reports for an empty event slice.
			return PhaseRunning, true, 0, nil
		}
		return "", false, 0, fmt.Errorf("journal: open events log: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()

	info, err := file.Stat()
	if err != nil {
		return "", false, 0, fmt.Errorf("journal: stat events log: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return PhaseRunning, true, 0, nil
	}

	var (
		window    []byte // bytes read so far, ending at EOF
		start     = size // file offset the window begins at
		chunkSize = int64(runPhaseChunkSize)
		tailKnown bool
		tail      int // bytes after the file's last newline: a torn final append
	)
	for start > 0 {
		if err := ctx.Err(); err != nil {
			return "", false, len(window), err
		}
		chunkStart := start - chunkSize
		if chunkStart < 0 {
			chunkStart = 0
		}
		chunk := make([]byte, start-chunkStart)
		if _, readErr := file.ReadAt(chunk, chunkStart); readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", false, len(window), fmt.Errorf("journal: read events log tail: %w", readErr)
		}
		window = append(chunk, window...)
		start = chunkStart
		if chunkSize < runPhaseMaxChunkSize {
			chunkSize *= 2
		}

		// The torn region is every byte after the last newline in the FILE, the
		// same rule readEventRecords applies. Once a newline is in the window that
		// boundary is known; a window covering the whole file with no newline in
		// it is one torn partial record and nothing else.
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
			found, ph, scanErr := decisivePhaseInChunkContext(ctx, window[:len(window)-tail], start == 0)
			if scanErr != nil {
				return "", false, len(window), scanErr
			}
			if found {
				return ph, true, len(window), nil
			}
		}

		// The whole file held no decisive record, so the run never reached a
		// terminal event — reconstructPhase's PhaseRunning default, reached
		// without a full parse.
		if start == 0 {
			return PhaseRunning, true, len(window), nil
		}
		if int64(len(window)) >= runPhaseTailBudget {
			return "", false, len(window), errPhaseTailBudgetExhausted
		}
	}
	return PhaseRunning, true, len(window), nil
}

// decisivePhaseInChunk walks the complete (newline-terminated) records in buf
// backwards and returns the phase implied by the last decisive one.
//
// It applies exactly the acceptance rules readEventRecords does — strip leading
// NUL crash-fill, skip a line that collapses to empty, error on a complete line
// that still fails to parse — so a bounded scan and a full read agree about what
// counts as an event and about what counts as corruption.
//
// atFileStart tells it whether buf begins at file offset 0, because the file's
// first record has no preceding newline: without it a single-record journal is
// never examined, and a run whose only event is run.finished would read back as
// still running.
func decisivePhaseInChunkContext(ctx context.Context, buf []byte, atFileStart bool) (found bool, phase RunPhase, err error) {
	var terminalGate *struct {
		gate   string
		branch int
		phase  RunPhase
	}
	for len(buf) > 0 {
		if err := ctx.Err(); err != nil {
			return false, "", err
		}
		nl := bytes.LastIndexByte(buf, '\n')
		var line []byte
		if nl < 0 {
			if !atFileStart {
				// A leading partial line belonging to a chunk not yet read, so not
				// a complete record from this window's perspective.
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
			// Fill-only line — the #116 cascade shape. Keep scanning backwards.
			continue
		}
		var ev Event
		if unmarshalErr := json.Unmarshal(line, &ev); unmarshalErr != nil {
			return false, "", fmt.Errorf("journal: corrupt event at seq boundary: %w", unmarshalErr)
		}
		if terminalGate != nil {
			if ev.Branch != terminalGate.branch {
				continue
			}
			if ev.Gate != terminalGate.gate {
				if ev.Type == EventGatePaused || ev.Type == EventGateStarted {
					continue
				}
				return true, terminalGate.phase, nil
			}
			switch ev.Type {
			case EventGateStarted, EventGateEvaluated:
				return true, terminalGate.phase, nil
			case EventGatePaused:
				return true, PhaseRunning, nil
			}
			continue
		}
		switch ev.Type {
		case EventStageRerunRequested, EventRunResumed, EventGateOverridden:
			return true, PhaseRunning, nil
		case EventRunFinished:
			return true, phaseFromStatus(ev.Status), nil
		case EventGateEvaluated:
			if terminalPhase, terminal := phaseFromTerminalTarget(ev.Target); terminal {
				terminalGate = &struct {
					gate   string
					branch int
					phase  RunPhase
				}{gate: ev.Gate, branch: ev.Branch, phase: terminalPhase}
			}
		}
	}
	if terminalGate != nil && atFileStart {
		return true, terminalGate.phase, nil
	}
	return false, "", nil
}

func phaseContext(ctx context.Context, path string) (phase RunPhase, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PhaseRunning, nil
		}
		return "", fmt.Errorf("journal: open events log: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()

	phase, bytesRead, err := phaseFromReaderContext(ctx, file)
	readprobe.RecordRunPhaseBytes(bytesRead)
	return phase, err
}

// phaseFromReaderContext scans forward so it preserves Phase's corruption
// checks while avoiding an uncancellable whole-file read.
func phaseFromReaderContext(ctx context.Context, reader io.Reader) (RunPhase, int, error) {
	phase := PhaseRunning
	buf := make([]byte, runPhaseChunkSize)
	var pending []byte
	bytesRead := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", bytesRead, err
		}
		n, readErr := reader.Read(buf)
		bytesRead += n
		if err := ctx.Err(); err != nil {
			return "", bytesRead, err
		}
		pending = append(pending, buf[:n]...)
		for {
			nl := bytes.IndexByte(pending, '\n')
			if nl < 0 {
				break
			}
			if err := ctx.Err(); err != nil {
				return "", bytesRead, err
			}
			line := bytes.TrimSpace(pending[:nl])
			if stripped := bytes.TrimLeft(line, "\x00"); len(stripped) != len(line) {
				line = bytes.TrimSpace(stripped)
			}
			if len(line) > 0 {
				var ev Event
				if err := json.Unmarshal(line, &ev); err != nil {
					return "", bytesRead, fmt.Errorf("journal: corrupt event at seq boundary: %w", err)
				}
				switch ev.Type {
				case EventStageRerunRequested, EventRunResumed, EventGateOverridden:
					phase = PhaseRunning
				case EventRunFinished:
					phase = phaseFromStatus(ev.Status)
				}
			}
			pending = pending[nl+1:]
		}
		if len(pending) > maxEventBytes {
			return "", bytesRead, fmt.Errorf("journal: scan events log: event exceeds %d bytes", maxEventBytes)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return phase, bytesRead, nil
			}
			return "", bytesRead, fmt.Errorf("journal: read events log: %w", readErr)
		}
		if n == 0 {
			return "", bytesRead, io.ErrNoProgress
		}
	}
}
