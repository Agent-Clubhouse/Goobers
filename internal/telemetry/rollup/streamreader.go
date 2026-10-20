package rollup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Match the journal writer's maximum complete event size. Oversized complete
// records exceed the supported journal format, so they are counted and skipped; an oversized torn tail remains at
// its original cursor until a newline arrives. Never allocate the whole tail.
const maxSchedulerRecordBytes = 8 << 20

// readJSONLTail visits one complete record at a time. Its memory use is bounded
// by one record rather than the entire scheduler backlog, including during a
// rebuild or after generation rotation. The visitor must not retain records.
// The caller commits cursor advancement and projections in one transaction.
func readJSONLTail[T any](ctx context.Context, path string, byteOffset int64, visit func(T) error) (newOffset int64, reset bool, skipped jsonlSkips, err error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, false, jsonlSkips{}, nil
	}
	if err != nil {
		return 0, false, jsonlSkips{}, fmt.Errorf("rollup: stat %s: %w", path, err)
	}
	start := byteOffset
	if start < 0 || info.Size() < start {
		start, reset = 0, true
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, false, jsonlSkips{}, fmt.Errorf("rollup: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return 0, false, jsonlSkips{}, fmt.Errorf("rollup: seek %s: %w", path, err)
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	for {
		line, size, oversized, err := readSchedulerLine(ctx, reader)
		if errors.Is(err, io.EOF) {
			return start, reset, skipped, nil // leave a torn final record unread
		}
		if err != nil {
			return 0, false, jsonlSkips{}, fmt.Errorf("rollup: read %s: %w", path, err)
		}
		var record T
		var decodeErr error
		if oversized {
			decodeErr = fmt.Errorf("record exceeds %d-byte limit", maxSchedulerRecordBytes)
		} else if line = bytes.TrimSpace(line); len(line) != 0 {
			decodeErr = json.Unmarshal(line, &record)
		}
		if decodeErr != nil {
			skipped = skipped.add(jsonlSkips{count: 1, first: fmt.Errorf("rollup: decode %s: corrupt record at byte offset %d: %w", path, start, decodeErr)})
		} else if len(line) != 0 {
			if err := visit(record); err != nil {
				return 0, false, jsonlSkips{}, err
			}
		}
		start += size
	}
}

// readSchedulerLine drains oversized records without retaining them and checks
// cancellation between buffer reads, even for a malformed record with no LF.
func readSchedulerLine(ctx context.Context, reader *bufio.Reader) (line []byte, size int64, oversized bool, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, size, oversized, err
		}
		part, err := reader.ReadSlice('\n')
		size += int64(len(part))
		if size > maxSchedulerRecordBytes {
			oversized, line = true, nil
		}
		if !oversized {
			line = append(line, part...)
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, size, oversized, err
		}
	}
}
