package artifactset

import (
	"bytes"
	"errors"
	"io"

	"golang.org/x/exp/trace"
)

// Native trace strings are stored uncompressed in v2 string-table batches.
// Preserve valid bytes only when the configured scrubber leaves them unchanged;
// replacing strings in place would corrupt length fields and references.
// Older v1 formats use a separate parser and are deliberately unsupported.
func sanitizeGoTrace(scrubber Scrubber, data []byte) ([]byte, error) {
	if len(data) < 16 || len(data) > MaxPayloadBytes {
		return nil, errors.New("invalid Go trace size")
	}
	switch string(data[:16]) {
	case "go 1.22 trace\x00\x00\x00", "go 1.23 trace\x00\x00\x00", "go 1.25 trace\x00\x00\x00", "go 1.26 trace\x00\x00\x00":
	default:
		return nil, errors.New("unsupported Go trace version")
	}
	reader, err := trace.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid Go trace")
	}
	// The pinned v2 parser bounds batch bytes, string bytes and stack frames
	// before allocation, and compacts ID tables only when already dense.
	// Bound emitted events as well as the enclosing payload bytes.
	const maxTraceEvents = 1 << 20
	observed := false
	for count := 0; ; count++ {
		event, err := reader.ReadEvent()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || count >= maxTraceEvents {
			// Parser errors may contain raw string-table content. Never expose it.
			return nil, errors.New("malformed or oversized Go trace")
		}
		observed = observed || event.Kind() != trace.EventSync
	}
	if !observed {
		return nil, errors.New("empty Go trace")
	}
	if !bytes.Equal(data, scrubber.Scrub(data)) {
		return nil, errors.New("native Go trace requires unsafe binary redaction")
	}
	return bytes.Clone(data), nil
}
