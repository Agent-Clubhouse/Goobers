package main

import (
	"errors"
	"io"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
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

func printValidationWarnings(w io.Writer, warnings []validate.CodedWarning) {
	for _, warning := range warnings {
		pln(w, warning.String())
	}
}
