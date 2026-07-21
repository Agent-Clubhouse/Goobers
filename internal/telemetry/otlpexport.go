package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
)

// ExportJournalOTLP writes journaled OTLP trace requests whose spans start in
// [since, until). A zero until leaves the window open-ended.
func ExportJournalOTLP(runsDirs []string, since, until time.Time, out io.Writer) error {
	if since.IsZero() {
		return fmt.Errorf("telemetry export requires a non-zero since timestamp")
	}
	if !until.IsZero() && since.After(until) {
		return fmt.Errorf("telemetry export since timestamp must not be after until timestamp")
	}

	seenRuns := make(map[string]string)
	for _, runsDir := range runsDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			return fmt.Errorf("read run journals in %s: %w", runsDir, err)
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("unsupported run journal at %s: symbolic links are not allowed", filepath.Join(runsDir, entry.Name()))
			}
			if !entry.IsDir() {
				continue
			}
			runID := entry.Name()
			runDir := filepath.Join(runsDir, runID)
			if previous, exists := seenRuns[runID]; exists {
				return fmt.Errorf("run %q exists in multiple journal roots: %s and %s", runID, previous, runDir)
			}
			seenRuns[runID] = runDir
			if err := exportRunOTLP(runID, runDir, since, until, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func exportRunOTLP(runID, runDir string, since, until time.Time, out io.Writer) error {
	path := filepath.Join(runDir, spansDirName, otlpFileName)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("run %q: journaled OTLP data is missing at %s; the run may predate OTLP journal support", runID, path)
	}
	if err != nil {
		return fmt.Errorf("run %q: inspect journaled OTLP data at %s: %w", runID, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("run %q: unsupported journaled OTLP data at %s: expected a regular %s file", runID, path, OTLPJSONContentType)
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("run %q: open journaled OTLP data at %s: %w", runID, path, err)
	}
	defer func() { _ = file.Close() }()

	reader := bufio.NewReader(file)
	recordNumber := 0
	for {
		record, readErr := reader.ReadBytes('\n')
		if errors.Is(readErr, io.EOF) {
			if len(record) != 0 {
				return fmt.Errorf("run %q: corrupt OTLP journal record %d in %s: missing newline delimiter", runID, recordNumber+1, path)
			}
			break
		}
		if readErr != nil {
			return fmt.Errorf("run %q: read journaled OTLP data at %s: %w", runID, path, readErr)
		}

		recordNumber++
		record = record[:len(record)-1]
		filtered, selected, err := filterOTLPRecord(record, since, until)
		if err != nil {
			return fmt.Errorf("run %q: %w in %s", runID, recordError(recordNumber, err), path)
		}
		if !selected {
			continue
		}
		framed := append(filtered, '\n')
		written, err := out.Write(framed)
		if err != nil {
			return fmt.Errorf("write journaled OTLP export: %w", err)
		}
		if written != len(framed) {
			return fmt.Errorf("write journaled OTLP export: %w", io.ErrShortWrite)
		}
	}
	if recordNumber == 0 {
		return fmt.Errorf("run %q: journaled OTLP data at %s contains no records", runID, path)
	}
	return nil
}

func filterOTLPRecord(record []byte, since, until time.Time) ([]byte, bool, error) {
	if len(bytes.TrimSpace(record)) == 0 {
		return nil, false, newOTLPRecordError("corrupt", "record is empty")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(record, &fields); err != nil {
		return nil, false, newOTLPRecordError("corrupt", "decode OTLP/JSON: %v", err)
	}
	if _, ok := fields["resourceSpans"]; !ok || len(fields) != 1 {
		return nil, false, newOTLPRecordError("unsupported", "expected %s", OTLPJSONMessageType)
	}

	traces, err := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(record)
	if err != nil {
		return nil, false, newOTLPRecordError("corrupt", "decode OTLP/JSON: %v", err)
	}
	total := traces.SpanCount()
	if total == 0 {
		return nil, false, newOTLPRecordError("unsupported", "%s contains no spans", OTLPJSONMessageType)
	}

	selected := 0
	traces.ResourceSpans().RemoveIf(func(resourceSpans ptrace.ResourceSpans) bool {
		resourceSpans.ScopeSpans().RemoveIf(func(scopeSpans ptrace.ScopeSpans) bool {
			scopeSpans.Spans().RemoveIf(func(span ptrace.Span) bool {
				start := span.StartTimestamp().AsTime()
				inWindow := !start.Before(since) && (until.IsZero() || start.Before(until))
				if inWindow {
					selected++
				}
				return !inWindow
			})
			return scopeSpans.Spans().Len() == 0
		})
		return resourceSpans.ScopeSpans().Len() == 0
	})

	if selected == 0 {
		return nil, false, nil
	}
	if selected == total {
		return append([]byte(nil), record...), true, nil
	}
	filtered, err := (&ptrace.JSONMarshaler{}).MarshalTraces(traces)
	if err != nil {
		return nil, false, fmt.Errorf("encode filtered OTLP/JSON: %w", err)
	}
	return filtered, true, nil
}

type otlpRecordError struct {
	kind  string
	cause error
}

func (e *otlpRecordError) Error() string { return e.cause.Error() }
func (e *otlpRecordError) Unwrap() error { return e.cause }

func newOTLPRecordError(kind, format string, args ...any) error {
	return &otlpRecordError{kind: kind, cause: fmt.Errorf(format, args...)}
}

func recordError(recordNumber int, err error) error {
	var recordErr *otlpRecordError
	if errors.As(err, &recordErr) {
		return fmt.Errorf("%s OTLP journal record %d: %w", recordErr.kind, recordNumber, recordErr.cause)
	}
	return fmt.Errorf("process OTLP journal record %d: %w", recordNumber, err)
}
