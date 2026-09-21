package journal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// ReadInstanceLogWindow reads a bounded suffix of committed instance events.
// truncated reports omitted history, an incomplete boundary record, or an
// event-count limit. Corruption is an error, not an empty healthy observation.
func ReadInstanceLogWindow(dir string, maxBytes, maxEvents int) ([]Event, bool, error) {
	if maxBytes < 1 || maxBytes > 8<<20 || maxEvents < 1 || maxEvents > 4096 {
		return nil, false, fmt.Errorf("journal: invalid instance window bounds")
	}
	path, _, err := resolveInstanceEventsPath(dir)
	if err != nil {
		return nil, false, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("journal: instance window source is not regular")
	}
	start := max(int64(0), info.Size()-int64(maxBytes))
	data := make([]byte, int(info.Size()-start))
	n, readErr := file.ReadAt(data, start)
	if readErr != nil && readErr != io.EOF {
		return nil, false, readErr
	}
	truncated := start > 0 || n < len(data)
	data = data[:n]
	if start > 0 {
		boundary := bytes.IndexByte(data, '\n')
		if boundary < 0 {
			return nil, true, nil
		}
		data = data[boundary+1:]
	}
	// A crash can leave a partial final write; never promote it to a record.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		boundary := bytes.LastIndexByte(data, '\n')
		if boundary < 0 {
			return nil, true, nil
		}
		data = data[:boundary+1]
		truncated = true
	}
	return decodeInstanceWindow(data, maxEvents, truncated)
}

func decodeInstanceWindow(data []byte, limit int, truncated bool) ([]Event, bool, error) {
	events := make([]Event, 0, min(limit, 64))
	data = bytes.TrimSuffix(data, []byte{'\n'})
	for len(data) > 0 {
		boundary := bytes.LastIndexByte(data, '\n')
		line := bytes.ReplaceAll(data[boundary+1:], []byte{0}, nil)
		if boundary < 0 {
			data = nil
		} else {
			data = data[:boundary]
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if len(events) == limit {
			truncated = true
			break
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, true, fmt.Errorf("journal: malformed instance window record")
		}
		events = append(events, event)
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events, truncated, nil
}
