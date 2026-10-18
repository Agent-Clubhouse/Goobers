package journal

import "errors"

// InstanceLogCursor incrementally reads the instance journal for a caller that
// keeps a projection of it between reads. The first Next reads the whole
// current generation; every later one parses only the records appended since,
// so a recurring reader pays for what the journal grew by instead of
// re-parsing its whole history on every pass (#7031).
//
// A cursor is not safe for concurrent use.
type InstanceLogCursor struct {
	dir         string
	seq         uint64
	state       InstanceLogState
	initialized bool
}

// NewInstanceLogCursor returns a cursor over the instance journal at dir that
// has not read anything yet.
func NewInstanceLogCursor(dir string) *InstanceLogCursor {
	return &InstanceLogCursor{dir: dir}
}

// Next returns the events appended since the previous call. reset reports that
// the caller's projection no longer describes the journal — this is the first
// read, or the journal was compacted into a new generation, removed or
// recreated since — so the caller must discard it and rebuild from events,
// which then hold the current generation in full.
func (c *InstanceLogCursor) Next() ([]Event, bool, error) {
	reset := false
	for {
		state, err := ReadInstanceLogState(c.dir)
		if err != nil {
			return nil, false, err
		}
		if !c.initialized || !c.state.SameJournal(state) {
			// Stay uninitialised until a read succeeds, so a failed read is
			// retried as a reset rather than handed back as an increment.
			c.seq = 0
			c.state = state
			c.initialized = false
			reset = true
		}
		if !state.Exists {
			c.initialized = true
			return nil, reset, nil
		}
		batch, err := ReadInstanceLogAfterSeq(c.dir, c.seq)
		if err != nil {
			return nil, false, err
		}
		current, err := ReadInstanceLogState(c.dir)
		if err != nil {
			return nil, false, err
		}
		if !state.SameJournal(current) {
			// The journal was replaced while it was read: the batch may mix
			// generations, so drop it and read the replacement from scratch.
			c.initialized = false
			continue
		}
		for _, event := range batch {
			if event.Seq > c.seq {
				c.seq = event.Seq
			}
		}
		c.initialized = true
		return batch, reset, nil
	}
}

// InstanceLogLastSeq returns the highest sequence committed to the instance
// journal at dir, reading only the journal's tail. It is the watermark to hand
// ReadInstanceLogAfterSeq later to find only what was appended after now. Zero
// means the journal is empty or missing, or that no sequence was found within
// the tail budget, in which case a later ReadInstanceLogAfterSeq reads the
// whole journal rather than missing anything.
func InstanceLogLastSeq(dir string) (uint64, error) {
	path, _, err := resolveInstanceEventsPath(dir)
	if err != nil {
		return 0, err
	}
	seq, _, _, err := tailSequence(path)
	if errors.Is(err, errTailBudgetExhausted) {
		return 0, nil
	}
	return seq, err
}
