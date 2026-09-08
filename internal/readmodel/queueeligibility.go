package readmodel

import (
	"encoding/json"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/prqueue"
)

// QueueEligibilityEvidence is a compact reference to one historical selection
// observation. Run-list reads never load the potentially large report payload.
// Its lifetime is bounded by the existing run-row retention, with at most one
// reference per run. Problem supersedes older evidence when a newer report is
// missing or invalid; it must never be displayed as an empty queue.
type QueueEligibilityEvidence struct {
	Stage            string
	RecordedAt       string
	Seq              uint64
	Artifact         *journal.Ref
	RepositoryKey    string
	ObservedAt       time.Time
	CompleteSnapshot bool
	MatchingItems    int
	OmittedItems     int
	Problem          string
}

type queueArtifactReader interface {
	ArtifactBytesBounded(journal.Ref, int64) ([]byte, error)
}

func projectQueueEligibility(reader queueArtifactReader, identity journal.RunIdentity, events []journal.Event) *QueueEligibilityEvidence {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if !event.KnownSchema() || event.Type != journal.EventStageFinished {
			continue
		}
		version, present := event.Outputs["queueEligibilityVersion"]
		if !present {
			continue
		}
		if version != "1" {
			return &QueueEligibilityEvidence{Stage: event.Stage, RecordedAt: formatTime(event.Time), Seq: event.Seq, Problem: "Queue eligibility report version is unsupported."}
		}
		return readQueueEligibilityEvidence(reader, identity, event)
	}
	return nil
}

func readQueueEligibilityEvidence(reader queueArtifactReader, identity journal.RunIdentity, event journal.Event) *QueueEligibilityEvidence {
	evidence := &QueueEligibilityEvidence{Stage: event.Stage, RecordedAt: formatTime(event.Time), Seq: event.Seq, Problem: "Queue eligibility report unavailable."}
	// A result may include other artifacts. Bound both the discovery count and
	// total bytes; never trust a recorded Ref.Size as a read ceiling.
	const maxArtifacts = 16
	remaining := int64(prqueue.MaxReportBytes + (64 << 10))
	for i, examined := len(event.Artifacts)-1, 0; i >= 0 && examined < maxArtifacts && remaining > 0; i, examined = i-1, examined+1 {
		ref := event.Artifacts[i]
		data, err := reader.ArtifactBytesBounded(ref, remaining)
		if err != nil {
			evidence.Problem = "Queue eligibility artifact could not be verified within its read bound."
			return evidence
		}
		remaining -= int64(len(data))
		var result struct {
			Queue json.RawMessage `json:"queueEligibility"`
		}
		if json.Unmarshal(data, &result) != nil || len(result.Queue) == 0 {
			continue
		}
		var report prqueue.Report
		if err := json.Unmarshal(result.Queue, &report); err != nil {
			evidence.Problem = "Queue eligibility report is invalid."
			return evidence
		}
		if report.RunID != identity.RunID || report.Workflow != identity.Workflow || report.Gaggle != identity.Gaggle {
			evidence.Problem = "Queue eligibility report does not match the owning run."
			return evidence
		}
		evidence.Artifact = &ref
		evidence.RepositoryKey = report.RepositoryKey
		evidence.ObservedAt = report.ObservedAt
		evidence.CompleteSnapshot = report.CompleteSnapshot
		evidence.MatchingItems = report.MatchingItems
		evidence.OmittedItems = report.OmittedItems
		evidence.Problem = ""
		return evidence
	}
	return evidence
}
