package journal

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// EventsBounded returns the same committed prefix as Events, with allocation
// and record ceilings. Exceeding either bound refuses instead of treating a
// truncated prefix as complete custody evidence.
func (r *Reader) EventsBounded(maxBytes int64, maxEvents int) ([]Event, error) {
	if maxBytes < 1 || maxBytes > 64<<20 || maxEvents < 1 || maxEvents > 100000 {
		return nil, errors.New("journal: invalid event read bounds")
	}
	f, err := os.Open(filepath.Join(r.dir, fileEvents))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes || bytes.Count(data, []byte{'\n'}) > maxEvents {
		return nil, errors.New("journal: event read exceeds bound")
	}
	records, _, err := parseEventRecords(data)
	if err != nil {
		return nil, err
	}
	events := make([]Event, len(records))
	for i, record := range records {
		events[i] = record.Event
	}
	return events, validateEventSchemas(events)
}
