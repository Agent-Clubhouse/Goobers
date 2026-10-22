package journal

import "fmt"

// MonotonicSeq verifies that events carry contiguous sequence numbers.
func MonotonicSeq(events []Event) error {
	for i, event := range events {
		want := uint64(i + 1)
		if event.Seq != want {
			return fmt.Errorf("journal: event %d (%s) has seq %d, want %d", i, event.Type, event.Seq, want)
		}
	}
	return nil
}
