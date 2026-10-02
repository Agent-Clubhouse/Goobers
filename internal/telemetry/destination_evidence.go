package telemetry

import "time"

// ExporterDeliveryCounters are local bounded queue observations, not remote
// acknowledgements. Durable Azure delivery is reported separately by Replay.
type ExporterDeliveryCounters struct {
	Accepted uint64 `json:"accepted"`
	Dropped  uint64 `json:"dropped"`
	Failures uint64 `json:"failures"`
}

// ExporterReplayHealthSnapshot exposes delivery evidence without spool paths,
// connection strings, resource keys, or arbitrary error text.
type ExporterReplayHealthSnapshot struct {
	AccountingReady      bool       `json:"accountingReady"`
	PendingRecords       int        `json:"pendingRecords"`
	PendingBytes         int64      `json:"pendingBytes"`
	OldestPendingSeconds int64      `json:"oldestPendingSeconds"`
	LastSuccess          *time.Time `json:"lastSuccess,omitempty"`
	LastFailure          *time.Time `json:"lastFailure,omitempty"`
	FailureClass         string     `json:"failureClass,omitempty"`
	ActiveFailure        bool       `json:"activeFailure"`
}

// RecordUnavailable retains an initialization failure without sensitive detail.
func (h *ExporterHealth) RecordUnavailable(err error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unavailableReason = exporterFailureReason(err)
}

func (h *ExporterHealth) setReplayRoot(root string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.replayRoot = root
}

func (h *ExporterHealth) observeJournal(snapshot func() JournalExportStats) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.journalSnapshot = snapshot
}

func (h *ExporterHealth) observeDiagnostics(snapshot func() DiagnosticExportStats) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.diagnosticSnapshot = snapshot
}

func (s *ExporterHealthSnapshot) addDestinationEvidence(root string, journalSnapshot func() JournalExportStats, diagnosticSnapshot func() DiagnosticExportStats) {
	if root != "" {
		r := InspectAzureReplayRoot(root)
		s.Replay = &ExporterReplayHealthSnapshot{AccountingReady: r.AccountingReady, PendingRecords: r.PendingRecords, PendingBytes: r.PendingBytes, OldestPendingSeconds: int64(r.OldestPendingAge.Seconds()), LastSuccess: timePtr(r.LastSuccess), LastFailure: timePtr(r.LastFailure), FailureClass: r.FailureClass, ActiveFailure: r.ActiveFailure}
	}
	if journalSnapshot != nil {
		j := journalSnapshot()
		s.Journal = &ExporterDeliveryCounters{Accepted: j.Accepted, Dropped: j.Dropped, Failures: j.ExportFailures}
	}
	if diagnosticSnapshot != nil {
		d := diagnosticSnapshot()
		s.Diagnostics = &ExporterDeliveryCounters{Accepted: d.Accepted, Dropped: d.Dropped, Failures: d.Failures}
	}
}
