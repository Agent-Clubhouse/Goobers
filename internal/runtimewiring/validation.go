package runtimewiring

import (
	"strings"

	"github.com/goobers/goobers/api/validate"
)

// ValidationIssueSummary renders error-severity diagnostics on one line.
func ValidationIssueSummary(report *validate.Report) string {
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
