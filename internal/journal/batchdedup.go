package journal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// AppendBatchIfAbsent deduplicates at most 128 runner annotations with one streaming scan
// under the active writer lock. key must be pure and return an empty string
// for unrelated history. Memory is bounded by one event and the input keys,
// not journal history. An error may leave a durable prefix; retry the batch.
func (r *Run) AppendBatchIfAbsent(ctx context.Context, batch []Event, key func(Event) string) (int, error) {
	if len(batch) > 128 || key == nil {
		return 0, fmt.Errorf("invalid journal deduplication batch")
	}
	pending := make(map[string]bool, len(batch))
	for _, event := range batch {
		if event.Type != EventRunnerAnnotation {
			return 0, fmt.Errorf("journal deduplication batch requires non-lifecycle annotations")
		}
		identity := key(event)
		if identity == "" {
			return 0, fmt.Errorf("empty journal deduplication identity")
		}
		pending[identity] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}
	if err := scanDeduplicationHistory(ctx, filepath.Join(r.dir, fileEvents), func(event Event) { delete(pending, key(event)) }); err != nil {
		return 0, err
	}
	appended := 0
	for _, event := range batch {
		if err := ctx.Err(); err != nil {
			return appended, err
		}
		identity := key(event)
		if !pending[identity] {
			continue
		}
		if err := r.append(event); err != nil {
			return appended, err
		}
		delete(pending, identity)
		appended++
		if err := r.checkpoint(); err != nil {
			return appended, err
		}
		if r.observer != nil {
			r.observer(r.id.RunID, r.seq)
		}
	}
	return appended, nil
}

func scanDeduplicationHistory(ctx context.Context, path string, visit func(Event)) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxEventBytes)
	scanner.Split(committedEventLine)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := bytes.TrimSpace(bytes.TrimLeft(bytes.TrimSpace(scanner.Bytes()), "\x00"))
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("journal: corrupt deduplication history: %w", err)
		}
		if err := validateEventSchemas([]Event{event}); err != nil {
			return err
		}
		visit(event)
	}
	return scanner.Err()
}

func committedEventLine(data []byte, atEOF bool) (int, []byte, error) {
	if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
		return newline + 1, data[:newline], nil
	}
	if atEOF && len(data) != 0 {
		return 0, nil, fmt.Errorf("journal: repair torn append boundary before deduplication")
	}
	return 0, nil, nil
}
