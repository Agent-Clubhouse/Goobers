package rollup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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
	return events, nil
}

// readInstanceEvents decodes the instance journal at
// <instance-root>/scheduler/events.jsonl — the same envelope and file name
// (fileEvents) as a run's own events.jsonl, just under the scheduler
// directory instead of a run directory, and thus tolerant of a torn tail the
// same way. Before issue #128, this file was never read by the rollup at
// all — Rebuild only ever scanned run directories — so scheduler decisions
// (trigger.fired/tick.skipped/claim.*) were unqueryable regardless of how
// long the daemon had been running. A missing scheduler directory (no
// `goobers up` has run yet) is not an error, just zero scheduler events.
func readInstanceEvents(schedulerDir string) ([]journalEvent, error) {
	path := filepath.Join(schedulerDir, fileEvents)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rollup: read %s: %w", path, err)
	}
	events, err := decodeJSONLTolerant[journalEvent](data)
	if err != nil {
		return nil, fmt.Errorf("rollup: decode %s: %w", path, err)
	}
	return events, nil
}

// readSpans decodes spans/spans.jsonl, tolerating a missing file (a run may
// not have emitted spans yet) and a torn final line (JournalSpanExporter
// appends per ExportSpans batch, fsyncing after each — an interrupted process
// mid-write leaves the same incomplete-tail signature events.jsonl can, and
// must be tolerated the same way, not fail the whole ingest).
func readSpans(runDir string) ([]telemetry.SpanRecord, error) {
	path := filepath.Join(runDir, dirSpans, fileSpans)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rollup: read %s: %w", path, err)
	}
	spans, err := decodeJSONLTolerant[telemetry.SpanRecord](data)
	if err != nil {
		return nil, fmt.Errorf("rollup: decode %s: %w", path, err)
	}
	return spans, nil
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
