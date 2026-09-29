package artifactset

import (
	"errors"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/handoffcheck"
)

const maxReportedIssues = 5

// HandoffError reports why an application/json entry is not a sound JSON
// handoff. It carries only issue codes, paths, and offsets, never payload
// values, so it is safe to show to the producing agent for a targeted retry.
type HandoffError struct {
	Entry  string
	Issues []handoffcheck.Issue
}

func (e *HandoffError) Error() string {
	parts := make([]string, 0, maxReportedIssues+1)
	for i, issue := range e.Issues {
		if i == maxReportedIssues {
			parts = append(parts, fmt.Sprintf("and %d more", len(e.Issues)-maxReportedIssues))
			break
		}
		part := issue.Code
		if issue.Path != "" {
			part += " at " + issue.Path
		}
		if issue.Offset > 0 {
			part += fmt.Sprintf(" (byte %d)", issue.Offset)
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("%v: entry %q is not a valid JSON handoff: %s", ErrInvalid, e.Entry, strings.Join(parts, "; "))
}

func (e *HandoffError) Is(target error) bool { return target == ErrInvalid }

// checkJSONHandoff deterministically classifies structural JSON problems
// (truncation, trailing data, duplicate keys, prose or code-fence wrapping)
// before the sanitizer, whose rejections are intentionally opaque.
func checkJSONHandoff(name, mediaType string, data []byte) error {
	if mediaType != "application/json" {
		return nil
	}
	verdict := handoffcheck.CheckSyntax(data)
	if verdict.Valid {
		return nil
	}
	return &HandoffError{Entry: name, Issues: verdict.Issues}
}

// AsHandoffError extracts a structured handoff failure from err.
func AsHandoffError(err error) (*HandoffError, bool) {
	var he *HandoffError
	return he, errors.As(err, &he)
}
