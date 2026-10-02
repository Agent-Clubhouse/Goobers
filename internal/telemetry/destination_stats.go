package telemetry

// Counters count destination delivery attempts, so the same committed record
// offered to two destinations contributes two accepted records. Durable replay
// accounting remains available per destination rather than merging spool state.
func (c *Client) namedJournalStats() JournalExportStats {
	var total JournalExportStats
	for _, s := range c.DestinationJournalStats() {
		total.Accepted += s.Accepted
		total.Dropped += s.Dropped
		total.ExportFailures += s.ExportFailures
		total.InvalidMetadata += s.InvalidMetadata
		total.DroppedRecordTooLarge += s.DroppedRecordTooLarge
		total.DroppedLockContention += s.DroppedLockContention
		total.DroppedQueueFull += s.DroppedQueueFull
		total.DroppedStopping += s.DroppedStopping
		total.DroppedShutdown += s.DroppedShutdown
		total.QueuedRecords += s.QueuedRecords
		total.QueuedBytes += s.QueuedBytes
		total.CatchupDeferred += s.CatchupDeferred
		total.SinkPanics = s.SinkPanics // Process-wide, not per destination.
	}
	return total
}
