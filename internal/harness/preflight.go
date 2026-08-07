package harness

import (
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/boundedagg"
	"github.com/goobers/goobers/internal/journal"
)

const maxPreflightDiagnosticBytes = 4 << 10

type preflightProbeFailure struct {
	message string
	cause   error
}

func (e *preflightProbeFailure) Error() string { return e.message }
func (e *preflightProbeFailure) Unwrap() error { return e.cause }

func preflightProbeError(probe string, result ProcessResult, runErr error, hint string) error {
	detail := strings.TrimSpace(string(journal.NewPatternScrubber().Scrub(result.Transcript)))
	detail = boundedagg.Bound(detail, maxPreflightDiagnosticBytes)

	var message string
	switch {
	case result.ExitCode > 0:
		message = fmt.Sprintf("%s exited %d", probe, result.ExitCode)
	case runErr != nil:
		message = fmt.Sprintf("%s failed: %v", probe, runErr)
	default:
		message = fmt.Sprintf("%s exited %d", probe, result.ExitCode)
	}
	if detail != "" {
		message += ": " + detail
	}
	if hint != "" {
		message += " — " + hint
	}
	return &preflightProbeFailure{message: message, cause: runErr}
}
