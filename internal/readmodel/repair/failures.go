package repair

import "time"

const (
	runFailureLimit        = 256
	runFailureRepeatWindow = 30 * time.Minute
)

// runFailure counts observations while a run remains in the bounded diagnostic
// cache. Counts restart after eviction or sweeper restart; count_since makes that
// scope explicit in warnings. No error strings or run-ID metric labels are kept.
// Stats.Failures remains the exact aggregate count, including suppressed warnings.
type runFailure struct {
	count      uint64
	suppressed uint64
	firstSeen  time.Time
	lastSeen   time.Time
	lastWarn   time.Time
}

// recordRunFailure announces a run's first failure immediately, then summarizes
// repeated failures at most once per window. Both sweep directions share counts.
func (s *Sweeper) recordRunFailure(runID, message string, err error, attrs ...any) {
	s.stats.Failures++
	now := s.options.Now()
	record := s.failureRecord(runID, now)
	record.count++
	record.lastSeen = now
	if record.count > 1 && now.Sub(record.lastWarn) < runFailureRepeatWindow {
		record.suppressed++
		return
	}
	args := []any{"run_id", runID, "error", err,
		"failure_count", record.count, "count_since", record.firstSeen.UTC(),
		"suppressed_failures", record.suppressed}
	s.options.Logger.Warn(message, append(args, attrs...)...)
	record.lastWarn = now
	record.suppressed = 0
}

func (s *Sweeper) failureRecord(runID string, now time.Time) *runFailure {
	if record := s.failures[runID]; record != nil {
		return record
	}
	if s.failures == nil {
		s.failures = make(map[string]*runFailure)
	}
	if len(s.failures) >= runFailureLimit {
		s.evictOldestFailure()
	}
	record := &runFailure{firstSeen: now}
	s.failures[runID] = record
	return record
}

// Eviction examines at most runFailureLimit records, independent of run history.
func (s *Sweeper) evictOldestFailure() {
	var oldestID string
	var oldest *runFailure
	for runID, record := range s.failures {
		if oldest == nil || record.lastSeen.Before(oldest.lastSeen) {
			oldestID, oldest = runID, record
		}
	}
	delete(s.failures, oldestID)
}
