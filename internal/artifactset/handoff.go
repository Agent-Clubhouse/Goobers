package artifactset

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/handoffcheck"
)

const maxReportedIssues = 5

// HandoffError reports why an application/json entry is not a sound JSON
// handoff. It carries only issue codes, paths, and offsets, never payload
// values, so it is safe to show to the producing agent for a targeted retry.
type HandoffError struct {
	Entry string
	// SchemaID names the declared schema the entry violated, when the failure
	// is a schema check rather than a structural JSON problem.
	SchemaID string
	Issues   []handoffcheck.Issue
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
	if e.SchemaID != "" {
		return fmt.Sprintf("%v: entry %q does not satisfy schema %q: %s", ErrInvalid, e.Entry, e.SchemaID, strings.Join(parts, "; "))
	}
	return fmt.Sprintf("%v: entry %q is not a valid JSON handoff: %s", ErrInvalid, e.Entry, strings.Join(parts, "; "))
}

// Is reports a HandoffError as ErrInvalid so callers can match it with errors.Is.
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

// CheckSchemas validates each prepared payload named in schemas against its
// declared schema at the completion boundary (#6868). It runs on the exact
// sanitized bytes that Publish will record, so a payload that was rewritten or
// written directly after an accepted publish_output still cannot bypass the
// contract. A declared slot with no prepared entry is reported as missing.
func (p *Prepared) CheckSchemas(schemas map[string]*handoffcheck.Schema) error {
	if len(schemas) == 0 {
		return nil
	}
	slots := make([]string, 0, len(schemas))
	for slot := range schemas {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	for _, slot := range slots {
		var entry *preparedEntry
		for i := range p.entries {
			if p.entries[i].name == slot {
				entry = &p.entries[i]
				break
			}
		}
		if entry == nil {
			return &PublicationError{Code: MissingSlotCode, Slot: slot}
		}
		verdict := schemas[slot].Check(entry.data)
		if !verdict.Valid {
			return &HandoffError{Entry: slot, SchemaID: verdict.SchemaID, Issues: verdict.Issues}
		}
	}
	return nil
}
