package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/harness"
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

func appendGooberHarnessWarnings(report *validate.Report, warnings []gooberHarnessWarning) ([]validate.CodedWarning, error) {
	if len(warnings) == 0 {
		return nil, nil
	}
	if report == nil {
		return nil, errors.New("validation report is nil")
	}
	coded := make([]validate.CodedWarning, 0, len(warnings))
	for _, warning := range warnings {
		var code validate.WarningCode
		switch warning.Warning.Kind {
		case harness.ConfigWarningModelFallback:
			code = validate.WarningModelFallback
		case harness.ConfigWarningModelUnverified:
			// Deliberately not a validation finding. This warning reports that
			// the harness could not be reached to enumerate models, which is a
			// property of the machine running validation, not of the config
			// being validated. Surfacing it here would make `goobers validate`
			// report a different result on a CI runner than on a developer
			// machine, and --strict (used by test/configvalidate for every
			// checked-in tree) would turn that difference into a failure.
			// Callers that care about verification state read the warning off
			// ConfigResolution instead.
			continue
		default:
			return nil, fmt.Errorf("unknown harness configuration warning kind %q", warning.Warning.Kind)
		}
		report.Issues = append(report.Issues, validate.Issue{
			Code:     code,
			Severity: validate.Warning,
			Kind:     "Goober",
			Name:     warning.Goober,
			Message:  warning.Warning.Message,
		})
		coded = append(coded, validate.CodedWarning{
			Code:        code,
			Severity:    validate.Warning,
			Scope:       "Goober/" + warning.Goober,
			Explanation: warning.Warning.Message,
		})
	}
	return coded, nil
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
