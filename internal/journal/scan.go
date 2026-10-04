package journal

// LastEvent returns the newest event matching pred.
func LastEvent(events []Event, pred func(Event) bool) (Event, bool) {
	index, ok := LastIndex(events, pred)
	if !ok {
		return Event{}, false
	}
	return events[index], true
}

// LastIndex returns the index of the newest event matching pred.
func LastIndex(events []Event, pred func(Event) bool) (int, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if pred(events[i]) {
			return i, true
		}
	}
	return 0, false
}
