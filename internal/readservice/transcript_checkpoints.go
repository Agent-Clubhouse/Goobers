package readservice

import (
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

func isTranscriptCheckpoint(event journal.Event) bool {
	return event.Runner["partial"] == true &&
		(event.Name == "transcript.partial" || strings.HasSuffix(event.Name, ".transcript.partial"))
}

func completedTranscriptCaptures(run runRead) map[string]bool {
	completed := make(map[string]bool)
	for _, record := range run.records {
		event := record.Event
		id, _ := event.Runner["transcriptCaptureComplete"].(string)
		if event.KnownSchema() && event.Type == journal.EventSpanRecorded && id != "" && event.Ref != nil {
			// A durable marker is not proof that its bytes are still available.
			// Preserve sequence-addressed access to any surviving partials when
			// final storage is missing or corrupt (including recovery failures).
			if _, err := run.reader.ArtifactBytesBounded(*event.Ref, journal.MaxCheckpointScrubBytes); err == nil {
				completed[id] = true
			}
		}
	}
	return completed
}

func supersededTranscriptCheckpoint(event journal.Event, completed map[string]bool) bool {
	id, _ := event.Runner["transcriptCapture"].(string)
	return isTranscriptCheckpoint(event) && id != "" && completed[id]
}
