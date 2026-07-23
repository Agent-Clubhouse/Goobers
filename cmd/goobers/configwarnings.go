package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

var loadConfigDirectory = instance.LoadConfigDir

type configReportError struct {
	report *validate.Report
	err    error
}

func (e *configReportError) Error() string { return e.err.Error() }
func (e *configReportError) Unwrap() error { return e.err }

func validationReportFromError(err error) *validate.Report {
	var reportErr *configReportError
	if errors.As(err, &reportErr) {
		return reportErr.report
	}
	return nil
}

func printValidationIssues(w io.Writer, report *validate.Report) {
	if report == nil {
		return
	}
	for _, issue := range report.Issues {
		if issue.Severity != validate.Error {
			continue
		}
		pln(w, issue.CLIString())
	}
	printValidationWarnings(w, report.CLIWarnings())
}

// printValidationWarnings is the shared CLI rendering seam for validator
// warnings and milestone #12's compatibility/deprecation producers.
func printValidationWarnings(w io.Writer, warnings []validate.CodedWarning) {
	for _, warning := range warnings {
		pln(w, warning.String())
	}
}

func journalValidationWarnings(log *journal.InstanceLog, warnings []validate.CodedWarning) error {
	for _, warning := range warnings {
		if err := log.Append(journal.Event{
			Type: journal.EventRunnerAnnotation,
			Runner: map[string]any{
				"kind":        "config.validation.warning",
				"code":        string(warning.Code),
				"severity":    string(warning.Severity),
				"scope":       warning.Scope,
				"explanation": warning.Explanation,
			},
		}); err != nil {
			return fmt.Errorf("journal config validation warning: %w", err)
		}
	}
	return nil
}
