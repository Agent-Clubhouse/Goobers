package readmodel

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/journal"
)

// OutputDisplayTitle is the well-known stage output key a stage uses to
// publish the run's canonical human-readable display title once it has
// claimed work (#5429). It is projected into operator.displayTitle so every
// client renders the same title instead of reconstructing one from
// operator.issue.
const OutputDisplayTitle = "displayTitle"

// MaxDisplayTitleRunes bounds a published display title. A longer value is
// rejected rather than truncated so a clipped title never looks canonical.
const MaxDisplayTitleRunes = 300

// StageDisplayTitle returns the canonical display title a successful
// stage.finished event published, or false when the event did not publish a
// well-formed one. Malformed values (non-string, blank, multi-line, control
// characters, or over MaxDisplayTitleRunes) are ignored so they cannot
// replace an earlier valid title.
func StageDisplayTitle(event journal.Event) (string, bool) {
	if event.Type != journal.EventStageFinished || event.Status != "success" {
		return "", false
	}
	raw, ok := event.Outputs[OutputDisplayTitle].(string)
	if !ok {
		return "", false
	}
	title := strings.TrimSpace(raw)
	if title == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > MaxDisplayTitleRunes {
		return "", false
	}
	if strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return "", false
	}
	return title, true
}
