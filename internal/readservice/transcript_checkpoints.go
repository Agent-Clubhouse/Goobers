package readservice

import (
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

func isTranscriptCheckpoint(event journal.Event) bool {
	return event.Runner["partial"] == true &&
		(event.Name == "transcript.partial" || strings.HasSuffix(event.Name, ".transcript.partial"))
}

func completedTranscriptCaptures(records []journal.EventRecord) map[string]bool {
	completed := make(map[string]bool)
	for _, record := range records {
		event := record.Event
		id, _ := event.Runner["transcriptCaptureComplete"].(string)
		if event.KnownSchema() && event.Type == journal.EventSpanRecorded && id != "" && event.Ref != nil {
			completed[id] = true
		}
	}
	return completed
}

func supersededTranscriptCheckpoint(event journal.Event, completed map[string]bool) bool {
	id, _ := event.Runner["transcriptCapture"].(string)
	return isTranscriptCheckpoint(event) && id != "" && completed[id]
}
