package rollup

import (
	"bufio"
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
// by ingest.go, it is never a decode error.
func readEvents(runDir string) ([]journalEvent, error) {
	f, err := os.Open(filepath.Join(runDir, fileEvents))
	if err != nil {
		return nil, fmt.Errorf("rollup: open %s: %w", fileEvents, err)
	}
	defer func() { _ = f.Close() }()

	var events []journalEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev journalEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("rollup: decode event: %w", err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("rollup: scan %s: %w", fileEvents, err)
	}
	return events, nil
}

// readSpans decodes spans/spans.jsonl, tolerating a missing file (a run may
// not have emitted spans, or telemetry may be disabled) — that is not an
// ingest error, just zero spans for the run.
func readSpans(runDir string) ([]telemetry.SpanRecord, error) {
	path := filepath.Join(runDir, dirSpans, fileSpans)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rollup: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var spans []telemetry.SpanRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec telemetry.SpanRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("rollup: decode span: %w", err)
		}
		spans = append(spans, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("rollup: scan %s: %w", path, err)
	}
	return spans, nil
}
