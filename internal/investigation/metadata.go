package investigation

import (
	"errors"
	"net/url"
	"regexp"
)

var absoluteMetadataPath = regexp.MustCompile(`(^|[\s"'(])(/[^\s]|[A-Za-z]:[\\/]|\\\\)`)

// validateMetadata keeps host paths and credential-bearing URL authorities out
// of the portable manifest. Payload bytes remain in independently scrubbed
// artifacts; metadata is deliberately a much smaller comparison surface.
func validateMetadata(e Evidence) error {
	locator, err := url.Parse(e.Subject.Item.URI)
	if err != nil || locator.Hostname() == "" || locator.User != nil || (locator.Scheme != "http" && locator.Scheme != "https") {
		return errors.New("investigation: invalid or credential-bearing issue locator")
	}
	values := []string{e.Subject.Repository, e.Subject.Item.Description, e.Environment.Platform, e.Reproduction.Symptom}
	values = appendDimensionStrings(values, e.Environment.Dimensions)
	for _, refs := range [][]EvidenceRef{e.Diagnosis.Evidence, e.Attachments} {
		for _, ref := range refs {
			values = append(values, ref.Description)
			values = appendDimensionStrings(values, ref.CaptureContext)
		}
	}
	for _, value := range values {
		if absoluteMetadataPath.MatchString(value) {
			return errors.New("investigation: metadata contains an absolute host path")
		}
	}
	return nil
}

func appendDimensionStrings(values []string, dimensions map[string]any) []string {
	for _, value := range dimensions {
		if text, ok := value.(string); ok {
			values = append(values, text)
		}
	}
	return values
}
