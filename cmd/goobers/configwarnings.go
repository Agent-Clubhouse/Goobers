package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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

// validationIssueSummary renders a report's error-severity issues as a single
// line for scopes that must carry them inside a returned error rather than
// print to a stream they own. Empty when there is nothing at error severity.
func validationIssueSummary(report *validate.Report) string {
	if report == nil {
		return ""
	}
	var lines []string
	for _, issue := range report.Issues {
		if issue.Severity != validate.Error {
			continue
		}
		lines = append(lines, issue.CLIString())
	}
	return strings.Join(lines, "; ")
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

// appendHarnessPreflightWarnings surfaces harnesses whose live startup
// preflight probe failed (#2812) as report warnings, without failing the
// caller — see harnessPreflightFailures' doc comment for why this is
// deliberately non-fatal. Sorted by harness name for deterministic output.
func appendHarnessPreflightWarnings(report *validate.Report, failures harnessPreflightFailures) ([]validate.CodedWarning, error) {
	if len(failures) == 0 {
		return nil, nil
	}
	if report == nil {
		return nil, errors.New("validation report is nil")
	}
	names := make([]string, 0, len(failures))
	for h := range failures {
		names = append(names, string(h))
	}
	sort.Strings(names)
	coded := make([]validate.CodedWarning, 0, len(failures))
	for _, name := range names {
		h := apiv1.Harness(name)
		message := fmt.Sprintf("harness %q failed its startup preflight probe and is unavailable until this is resolved: %v", name, failures[h])
		report.Issues = append(report.Issues, validate.Issue{
			Code:     validate.WarningHarnessPreflightFailed,
			Severity: validate.Warning,
			Kind:     "Harness",
			Name:     name,
			Message:  message,
		})
		coded = append(coded, validate.CodedWarning{
			Code:        validate.WarningHarnessPreflightFailed,
			Severity:    validate.Warning,
			Scope:       "Harness/" + name,
			Explanation: message,
		})
	}
	return coded, nil
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

func appendSkillPackageCollisionWarnings(configDir string, report *validate.Report, goobers map[string]apiv1.GooberSpec) ([]validate.CodedWarning, error) {
	if report == nil {
		return nil, errors.New("validation report is nil")
	}
	type collision struct {
		gaggle string
		skill  string
	}
	seen := map[collision]struct{}{}
	for _, goober := range goobers {
		for _, skill := range goober.Skills {
			scoped, shared, ok := skillPackageDirs(configDir, goober.Gaggle, skill)
			if !ok {
				continue
			}
			scopedInfo, scopedErr := os.Stat(scoped)
			if scopedErr != nil && !os.IsNotExist(scopedErr) {
				return nil, fmt.Errorf("stat gaggle skill %q package: %w", skill, scopedErr)
			}
			sharedInfo, sharedErr := os.Stat(shared)
			if sharedErr != nil && !os.IsNotExist(sharedErr) {
				return nil, fmt.Errorf("stat instance skill %q package: %w", skill, sharedErr)
			}
			if scopedErr == nil && scopedInfo.IsDir() && sharedErr == nil && sharedInfo.IsDir() {
				seen[collision{gaggle: goober.Gaggle, skill: skill}] = struct{}{}
			}
		}
	}
	collisions := make([]collision, 0, len(seen))
	for item := range seen {
		collisions = append(collisions, item)
	}
	sort.Slice(collisions, func(i, j int) bool {
		if collisions[i].gaggle != collisions[j].gaggle {
			return collisions[i].gaggle < collisions[j].gaggle
		}
		return collisions[i].skill < collisions[j].skill
	})
	warnings := make([]validate.CodedWarning, 0, len(collisions))
	for _, item := range collisions {
		explanation := fmt.Sprintf("gaggle-level and instance-level packages both define skill %q; the gaggle-level definition takes effect", item.skill)
		report.Issues = append(report.Issues, validate.Issue{
			Code:     validate.WarningSkillPackageCollision,
			Severity: validate.Warning,
			Kind:     "Gaggle",
			Name:     item.gaggle,
			Message:  explanation,
		})
		warnings = append(warnings, validate.CodedWarning{
			Code:        validate.WarningSkillPackageCollision,
			Severity:    validate.Warning,
			Scope:       "Gaggle/" + item.gaggle,
			Explanation: explanation,
		})
	}
	return warnings, nil
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
