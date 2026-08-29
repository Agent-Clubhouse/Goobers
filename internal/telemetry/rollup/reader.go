package rollup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/telemetry"
)

// On-disk names, mirrored from internal/journal's layout.go / the telemetry
// span exporter — see mirror.go's package comment for why these are literal
// constants here rather than an import.
const (
	fileRunYAML = "run.yaml"
	fileEvents  = "events.jsonl"
	dirSpans    = "spans"
	fileSpans   = "spans.jsonl"
)

func readRunIdentity(runDir string) (runIdentity, error) {
	data, err := os.ReadFile(filepath.Join(runDir, fileRunYAML))
	if err != nil {
		return runIdentity{}, fmt.Errorf("rollup: read %s: %w", fileRunYAML, err)
	}
	var id runIdentity
	if err := yaml.Unmarshal(data, &id); err != nil {
		return runIdentity{}, fmt.Errorf("rollup: decode %s: %w", fileRunYAML, err)
	}
	if id.RunID == "" {
		return runIdentity{}, fmt.Errorf("rollup: %s has no runId", fileRunYAML)
	}
	// An unknown schema version is refused rather than ingested with fields
	// silently zero-valued (#2054) — mirrors internal/journal.Reader.Identity's
	// same refusal of a run.yaml this build does not own the shape of.
	if id.Schema != runSchema {
		return runIdentity{}, fmt.Errorf("rollup: %s has unknown schema %q (want %q)", fileRunYAML, id.Schema, runSchema)
	}
	return id, nil
}

// readEvents decodes events.jsonl in file order (which is seq order — the
// journal is append-only). A reader tolerates unknown fields and unknown event
// types (the journal's own "read leniently, write strictly" forward-compat
// policy, README.md #8) — an unrecognized event.Type simply isn't switched on
// by ingest.go, it is never a decode error. A torn final line from a crash
// mid-append (no trailing newline — internal/journal's writer only fsyncs
// after a complete newline-terminated record, so an interrupted write always
// leaves an incomplete tail, never a corrupt-but-complete line) is silently
// dropped rather than failing the whole ingest — the same rule
// internal/journal.Reader.Events applies on the writer side (issue #127; a
// crashed, not-yet-Recovered run must not fail every rollup query).
func readEvents(runDir string) ([]journalEvent, error) {
	data, err := os.ReadFile(filepath.Join(runDir, fileEvents))
	if err != nil {
		return nil, fmt.Errorf("rollup: read %s: %w", fileEvents, err)
	}
	events, err := decodeJSONLTolerant[journalEvent](data)
	if err != nil {
		return nil, fmt.Errorf("rollup: decode %s: %w", fileEvents, err)
	}
	if err := validateJournalEventSchemas(events); err != nil {
		return nil, fmt.Errorf("rollup: decode %s: %w", fileEvents, err)
	}
	return events, nil
}

// readInstanceEventsFrom decodes the instance journal at
// <instance-root>/scheduler/events.jsonl — the same envelope and file name
// (fileEvents) as a run's own events.jsonl, just under the scheduler directory
// instead of a run directory, and thus tolerant of a torn tail the same way
// (issue #128 first made the rollup read this file so scheduler decisions —
// trigger.fired/tick.skipped/claim.* — became queryable).
//
// It decodes only the records at or after byteOffset so a steady-state
// IngestSchedulerLog reads just the newly appended tail instead of the whole
// (potentially multi-GB) journal every tick (#1411). See readJSONLTail for the
// offset/reset contract.
func readInstanceEventsFrom(schedulerDir string, byteOffset int64) (events []journalEvent, newOffset int64, reset bool, err error) {
	events, newOffset, reset, err = readJSONLTail[journalEvent](filepath.Join(schedulerDir, fileEvents), byteOffset)
	if err != nil {
		return nil, 0, false, err
	}
	if err := validateJournalEventSchemas(events); err != nil {
		return nil, 0, false, fmt.Errorf("rollup: decode %s: %w", fileEvents, err)
	}
	return events, newOffset, reset, nil
}

// readSpans decodes spans/spans.jsonl, tolerating a missing file (a run may
// not have emitted spans yet) and a torn final line (JournalSpanExporter
// appends per ExportSpans batch, fsyncing after each — an interrupted process
// mid-write leaves the same incomplete-tail signature events.jsonl can, and
// must be tolerated the same way, not fail the whole ingest). Read in full: a
// single run's own span file is bounded by that run's lifetime, unlike the
// scheduler's (see readSchedulerSpansFrom).
func readSpans(runDir string) ([]telemetry.SpanRecord, error) {
	data, err := os.ReadFile(filepath.Join(runDir, dirSpans, fileSpans))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rollup: read %s: %w", fileSpans, err)
	}
	spans, err := decodeJSONLTolerant[telemetry.SpanRecord](data)
	if err != nil {
		return nil, fmt.Errorf("rollup: decode %s: %w", fileSpans, err)
	}
	return spans, nil
}

// readSchedulerSpansFrom decodes <instance-root>/scheduler/spans/spans.jsonl,
// the rolling record of scheduler-kind spans (dispatch/tick decisions, not
// bound to any one run). Unlike a run's own spans file, this one accumulates
// for the instance's entire lifetime — 75K spans / 55.6MB observed in the wild
// — and JournalSpanExporter.appendSpans only ever appends (O_APPEND), so it
// gets the same byte-offset cursor treatment readInstanceEventsFrom gives
// events.jsonl: a steady-state ingest reads only the newly appended tail
// instead of delete+reinserting every span ever recorded. See readJSONLTail
// for the offset/reset contract.
func readSchedulerSpansFrom(schedulerDir string, byteOffset int64) (spans []telemetry.SpanRecord, newOffset int64, reset bool, err error) {
	return readJSONLTail[telemetry.SpanRecord](filepath.Join(schedulerDir, dirSpans, fileSpans), byteOffset)
}

// readJSONLTail decodes the newline-delimited records in path at or after
// byteOffset — shared by readInstanceEventsFrom and readSchedulerSpansFrom,
// the two append-only, unboundedly-growing logs the rollup ingests
// incrementally.
//
// It returns the decoded records, the offset just past the last COMPLETE
// record (where the next ingest resumes — a torn final line is re-read next
// time, the same tolerance decodeJSONLTolerant applies), and reset=true when
// the file is now shorter than byteOffset (rotation/compaction/truncation), in
// which case it re-reads from the head — safe because both callers'
// downstream writes are idempotent (ON CONFLICT DO NOTHING for events keyed by
// seq; delete-then-insert for spans keyed by span id) so replaying an
// already-ingested prefix is harmless, just redundant work. A missing file
// (no `goobers up` yet, or no scheduler spans emitted yet) is not an error,
// just zero records at offset 0.
func readJSONLTail[T any](path string, byteOffset int64) (records []T, newOffset int64, reset bool, err error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("rollup: stat %s: %w", path, err)
	}
	start := byteOffset
	if start < 0 || info.Size() < start {
		// The file shrank below where we last resumed — it was rotated,
		// compacted, or truncated. Re-read from the head; the caller's own
		// idempotent write keeps already-ingested rows untouched.
		start = 0
		reset = true
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("rollup: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if start > 0 {
		if _, err = file.Seek(start, io.SeekStart); err != nil {
			return nil, 0, false, fmt.Errorf("rollup: seek %s: %w", path, err)
		}
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, 0, false, fmt.Errorf("rollup: read %s: %w", path, err)
	}
	records, err = decodeJSONLTolerant[T](data)
	if err != nil {
		return nil, 0, false, fmt.Errorf("rollup: decode %s: %w", path, err)
	}
	// Advance only past the last complete (newline-terminated) record. -1 (no
	// newline in this window) leaves the offset unchanged so the whole tail is
	// re-read once it completes.
	if nl := bytes.LastIndexByte(data, '\n'); nl >= 0 {
		start += int64(nl) + 1
	}
	return records, start, reset, nil
}

// decodeJSONLTolerant splits data on its last newline: everything before it
// is a set of complete, durably-written lines that MUST each unmarshal into T
// (a decode failure there is real corruption, surfaced as an error); anything
// after the last newline is an in-flight write interrupted mid-record and is
// silently discarded, never returned or treated as an error — mirrors
// internal/journal/reader.go's readEvents torn-tail handling exactly, just
// generalized over any newline-delimited record type.
func decodeJSONLTolerant[T any](data []byte) ([]T, error) {
	nl := bytes.LastIndexByte(data, '\n')
	if nl < 0 {
		return nil, nil // no complete record yet — the whole file is a torn write
	}
	complete := data[:nl+1]

	var out []T
	scanner := bufio.NewScanner(bytes.NewReader(complete))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec T
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("corrupt record at line boundary: %w", err)
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func validateJournalEventSchemas(events []journalEvent) error {
	for _, event := range events {
		if event.Schema != eventSchema {
			return fmt.Errorf(
				"event schema %q is unsupported (supported %q); upgrade Goobers to a binary that supports %s",
				event.Schema, eventSchema, event.Schema,
			)
		}
	}
	return nil
}
