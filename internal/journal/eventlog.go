package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// appendEvent marshals, scrubs, writes, and fsyncs one event line to f, assigning
// seq (the next value of *seq), schema, and time. It is the shared durability
// core both Run and InstanceLog use, so a run's events.jsonl and the instance's
// scheduler/events.jsonl honor the identical contract: one line, one fsync,
// before the call returns (§4).
func appendEvent(f *os.File, seq *uint64, scrubber Scrubber, now func() time.Time, ev Event) (Event, error) {
	*seq++
	ev.Seq = *seq
	ev.Schema = EventSchema
	ev.Time = now()

	line, err := json.Marshal(ev)
	if err != nil {
		*seq--
		return Event{}, fmt.Errorf("journal: marshal event: %w", err)
	}
	line = scrubber.Scrub(line)
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		return Event{}, fmt.Errorf("journal: append event: %w", err)
	}
	if err := syncFile(f); err != nil {
		return Event{}, fmt.Errorf("journal: fsync event: %w", err)
	}
	return ev, nil
}

// truncateTornTail removes a torn final region from path, sized tornBytes as
// reported by readEvents — a partial (non-newline-terminated) record and/or NUL
// zero-fill left by a crash — so the next append starts on a clean record
// boundary. A no-op when tornBytes is 0.
func truncateTornTail(path string, tornBytes int) error {
	if tornBytes == 0 {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := os.Truncate(path, fi.Size()-int64(tornBytes)); err != nil {
		return fmt.Errorf("journal: truncate torn record: %w", err)
	}
	return nil
}
