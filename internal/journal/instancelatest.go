package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

// InstanceLatest caches the last matching committed event. Each read observes
// the current generation and file identity, then scans only appended bytes.
// Replacement, shrinkage or a same-size rewrite invalidates the cache.
// As with journal cursors, growth of the same inode must be append-only. Cold
// reads walk backwards, retaining at most one bounded record and a read chunk;
// they never materialize the journal. Older records before the last match are
// not validated, just as with bounded phase reconstruction.
//
// A match with empty metadata is still authoritative: predicates must include
// legacy barrier events when they supersede newer-schema application evidence.
// The predicate must be stable and must not call back into this cache.
type InstanceLatest struct {
	mu     sync.Mutex
	dir    string
	match  func(Event) bool
	info   os.FileInfo
	offset int64
	event  Event
	found  bool
}

// NewInstanceLatest creates a read-only cursor for one stable event predicate.
func NewInstanceLatest(dir string, match func(Event) bool) *InstanceLatest {
	return &InstanceLatest{dir: dir, match: match}
}

// Read refreshes the cache from a fixed-size snapshot of the current file.
// A concurrent append is observed on the next read. No writer lock or retained
// file descriptor is needed; independent process appends are observed too.
// The returned event is cached and must not be mutated by the caller.
func (c *InstanceLatest) Read(ctx context.Context) (Event, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Event{}, false, err
	}
	path, err := InstanceEventsPath(c.dir)
	if err != nil {
		return Event{}, false, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		c.info, c.offset, c.event, c.found = nil, 0, Event{}, false
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Event{}, false, err
	}
	if !info.Mode().IsRegular() {
		return Event{}, false, errors.New("journal: latest-event source is not regular")
	}
	start := int64(0)
	event, found := Event{}, false
	if c.info != nil && os.SameFile(c.info, info) {
		if info.Size() == c.info.Size() && info.ModTime().Equal(c.info.ModTime()) {
			return c.event, c.found, nil
		}
		if info.Size() > c.info.Size() {
			start, event, found = c.offset, c.event, c.found
		}
	}
	latest, matched, end, err := latestEventBetween(ctx, file, start, info.Size(), c.match)
	if err != nil {
		return Event{}, false, err
	}
	if matched {
		event, found = latest, true
	}
	c.info, c.offset, c.event, c.found = info, end, event, found
	return event, found, nil
}

func latestEventBetween(ctx context.Context, file *os.File, lower, upper int64, match func(Event) bool) (Event, bool, int64, error) {
	end, position := upper, upper
	tail := true
	var buffer, pending []byte
	for position > lower {
		if err := ctx.Err(); err != nil {
			return Event{}, false, 0, err
		}
		start := max(lower, position-tailChunkSize)
		var err error
		buffer, err = readLatestChunk(file, buffer, pending, start, position)
		if err != nil {
			return Event{}, false, 0, err
		}
		window := buffer
		position = start
		// Ignore an incomplete final append. Remember the complete boundary so
		// its eventual completion is included in the next incremental read.
		if tail {
			nl := bytes.LastIndexByte(window, '\n')
			if nl < 0 {
				if len(window) >= maxEventBytes {
					return Event{}, false, 0, errors.New("journal: torn record exceeds event limit")
				}
				pending = window
				continue
			}
			end, window, tail = start+int64(nl)+1, window[:nl], false
		}
		for len(window) > 0 {
			if err := ctx.Err(); err != nil {
				return Event{}, false, 0, err
			}
			nl := bytes.LastIndexByte(window, '\n')
			if nl < 0 && start > lower {
				break
			}
			line := window[nl+1:]
			if event, matched, decodeErr := matchingLatestEvent(line, match); decodeErr != nil || matched {
				return event, matched, end, decodeErr
			}
			if nl < 0 {
				window = nil
			} else {
				window = window[:nl]
			}
		}
		if len(window) >= maxEventBytes {
			return Event{}, false, 0, errors.New("journal: record exceeds event limit")
		}
		pending = window
	}
	if tail {
		end = lower
	}
	return Event{}, false, end, nil
}

func matchingLatestEvent(line []byte, match func(Event) bool) (Event, bool, error) {
	if len(line) >= maxEventBytes {
		return Event{}, false, errors.New("journal: record exceeds event limit")
	}
	line = bytes.TrimSpace(bytes.TrimLeft(bytes.TrimSpace(line), "\x00"))
	if len(line) == 0 {
		return Event{}, false, nil
	}
	var event Event
	if err := json.Unmarshal(line, &event); err != nil {
		return Event{}, false, fmt.Errorf("journal: corrupt latest-event record: %w", err)
	}
	if !event.KnownSchema() {
		return Event{}, false, unsupportedPayloadSchema("event", event.Schema, EventSchema)
	}
	return event, match(event), nil
}

// Reuse a geometrically grown buffer so a record spanning many chunks does
// not cause quadratic allocation. Only the unfinished record crosses reads.
func readLatestChunk(file *os.File, buffer, pending []byte, start, end int64) ([]byte, error) {
	n := int(end - start)
	size := n + len(pending)
	if cap(buffer) < size {
		buffer = make([]byte, size, max(size, 2*cap(buffer)))
	} else {
		buffer = buffer[:size]
	}
	copy(buffer[n:], pending)
	_, err := file.ReadAt(buffer[:n], start)
	return buffer, err
}
