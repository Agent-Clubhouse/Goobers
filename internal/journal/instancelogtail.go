package journal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/goobers/goobers/internal/readprobe"
)

// InstanceLogTail incrementally reads complete events appended after it opens.
// It follows compaction generations without replaying records retained in the
// replacement generation.
type InstanceLogTail struct {
	dir        string
	file       *os.File
	generation int
	offset     int64
	seq        uint64
}

// OpenInstanceLogTail starts an incremental reader at the current journal end.
func OpenInstanceLogTail(dir string) (*InstanceLogTail, error) {
	lock, err := acquireJournalLock(dir, "instance log tail")
	if err != nil {
		return nil, err
	}
	defer releaseJournalLock(lock)

	path, generation, err := resolveInstanceEventsPath(dir)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("journal: open instance log tail: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("journal: stat instance log tail: %w", err)
	}
	seq, tornBytes, _, err := tailSequence(path)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("journal: initialize instance log tail: %w", err)
	}
	return &InstanceLogTail{
		dir:        dir,
		file:       file,
		generation: generation,
		offset:     info.Size() - int64(tornBytes),
		seq:        seq,
	}, nil
}

// Events returns complete events appended since the previous call.
func (t *InstanceLogTail) Events() ([]Event, error) {
	var (
		events    []Event
		bytesRead int
	)
	defer func() { readprobe.RecordInstanceTailRead(bytesRead) }()

	for {
		appended, read, err := t.readCurrent()
		bytesRead += read
		if err != nil {
			return nil, err
		}
		events = append(events, appended...)

		_, generation, err := resolveInstanceEventsPath(t.dir)
		if err != nil {
			return nil, err
		}
		if generation == t.generation {
			return events, nil
		}

		// Compaction may have raced the first read. Its old generation is now
		// frozen, so one final drain captures everything in its snapshot.
		appended, read, err = t.readCurrent()
		bytesRead += read
		if err != nil {
			return nil, err
		}
		events = append(events, appended...)

		path, generation, err := resolveInstanceEventsPath(t.dir)
		if err != nil {
			return nil, err
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("journal: open rotated instance log tail: %w", err)
		}
		appended, offset, read, err := readEventsAfterSeq(file, t.seq)
		bytesRead += read
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := t.file.Close(); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("journal: close rotated instance log tail: %w", err)
		}
		t.file = file
		t.generation = generation
		t.offset = offset
		t.accept(appended)
		events = append(events, appended...)
	}
}

func (t *InstanceLogTail) readCurrent() ([]Event, int, error) {
	info, err := t.file.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("journal: stat instance log tail: %w", err)
	}
	if info.Size() <= t.offset {
		return nil, 0, nil
	}
	data := make([]byte, info.Size()-t.offset)
	n, err := t.file.ReadAt(data, t.offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, n, fmt.Errorf("journal: read instance log tail: %w", err)
	}
	data = data[:n]
	records, tornBytes, err := parseEventRecords(data)
	if err != nil {
		return nil, n, err
	}
	t.offset += int64(n - tornBytes)
	events := eventsAfter(records, t.seq)
	t.accept(events)
	return events, n, nil
}

func (t *InstanceLogTail) accept(events []Event) {
	for _, event := range events {
		if event.Seq > t.seq {
			t.seq = event.Seq
		}
	}
}

func eventsAfter(records []EventRecord, seq uint64) []Event {
	events := make([]Event, 0, len(records))
	for _, record := range records {
		if record.Event.Seq > seq {
			events = append(events, record.Event)
		}
	}
	return events
}

func readEventsAfterSeq(file *os.File, seq uint64) ([]Event, int64, int, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("journal: stat rotated instance log tail: %w", err)
	}
	size := info.Size()
	if seq == 0 {
		data := make([]byte, size)
		n, err := file.ReadAt(data, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, n, fmt.Errorf("journal: read instance log tail: %w", err)
		}
		data = data[:n]
		records, tornBytes, err := parseEventRecords(data)
		if err != nil {
			return nil, 0, n, err
		}
		readprobe.RecordInstanceTailRecords(len(records))
		return eventsAfter(records, seq), size - int64(tornBytes), n, nil
	}
	start := size
	var window []byte
	for {
		chunkStart := start - tailChunkSize
		if chunkStart < 0 {
			chunkStart = 0
		}
		chunk := make([]byte, start-chunkStart)
		if _, err := file.ReadAt(chunk, chunkStart); err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, len(window), fmt.Errorf("journal: read rotated instance log tail: %w", err)
		}
		window = append(chunk, window...)
		start = chunkStart

		candidate := window
		if start > 0 {
			nl := bytes.IndexByte(candidate, '\n')
			if nl < 0 {
				continue
			}
			candidate = candidate[nl+1:]
		}
		records, tornBytes, err := parseEventRecords(candidate)
		if err != nil {
			return nil, 0, len(window), err
		}
		readprobe.RecordInstanceTailRecords(len(records))
		for i := len(records) - 1; i >= 0; i-- {
			if records[i].Event.Seq <= seq {
				return eventsAfter(records[i+1:], seq), size - int64(tornBytes), len(window), nil
			}
		}
		if start == 0 {
			return eventsAfter(records, seq), size - int64(tornBytes), len(window), nil
		}
	}
}

// Close releases the generation file held by the tail reader.
func (t *InstanceLogTail) Close() error {
	if t == nil || t.file == nil {
		return nil
	}
	return t.file.Close()
}
