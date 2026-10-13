package readmodel

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/journal"
)

// StageIssueURL reads the optional URL published alongside a claimed id/title.
// Missing or invalid URLs leave the work item readable without a hyperlink.
func StageIssueURL(event journal.Event) string {
	if event.Type != journal.EventStageFinished || event.Status != "success" {
		return ""
	}
	raw, ok := event.Outputs["url"].(string)
	if !ok || len(raw) > 2048 || !utf8.ValidString(raw) || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") ||
		parsed.Hostname() == "" || parsed.User != nil {
		return ""
	}
	return raw
}
