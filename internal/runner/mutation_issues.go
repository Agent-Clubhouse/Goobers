package runner

import (
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

// Malformed sidecar records remain visible diagnostics, but losing the
// diagnostic itself is a journal failure, not a successful stage dispatch.
func recordMutationSidecarIssues(jr interface{ Append(journal.Event) error }, stage string, attempt int, class journal.AttemptClass, issues []string) error {
	if len(issues) == 0 {
		return nil
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventError, Stage: stage, Attempt: attempt, AttemptClass: class,
		Error: &journal.ErrorDetail{Code: "mutation_sidecar_read_failed", Message: strings.Join(issues, "; ")},
	}); err != nil {
		return fmt.Errorf("runner: journal mutation sidecar issues for %q: %w", stage, err)
	}
	return nil
}
