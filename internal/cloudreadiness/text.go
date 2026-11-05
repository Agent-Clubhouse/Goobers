package cloudreadiness

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// WriteText writes the operator-facing grouped readiness report.
func WriteText(w io.Writer, result Result) error {
	var writeErr error
	writef := func(format string, args ...any) {
		if writeErr != nil {
			return
		}
		_, writeErr = fmt.Fprintf(w, format, args...)
	}
	if result.CloudReady {
		writef("cloud-ready\n")
		return writeErr
	}
	writef("Cloud cutover blockers by workflow:\n")
	for _, workflow := range result.Workflows {
		writef("\n%s/%s (DSL %s)\n", workflow.Gaggle, workflow.Name, workflow.DSLVersion)
		for _, blocker := range workflow.Blockers {
			stage := ""
			if blocker.Stage != "" {
				stage = fmt.Sprintf(" stage %q", blocker.Stage)
			}
			writef("  - %s%s %s: %s\n", blocker.Code, stage, blocker.Path, blocker.Message)
			writef("    unblock: %s", blocker.Action.Description)
			if blocker.Action.IssueURL != "" {
				writef(" (%s)", blocker.Action.IssueURL)
			}
			if blocker.Action.Command != "" {
				writef(" (`%s`)", blocker.Action.Command)
			}
			writef("\n")
		}
	}

	writef("\nBlockers grouped by remedy:\n")
	for _, group := range result.Blockers {
		label := group.Code
		if group.Feature != "" {
			label += " " + group.Feature
		}
		writef("  %s — %s\n", label, group.Action.Description)
		for _, occurrence := range group.Occurrences {
			stage := ""
			if occurrence.Stage != "" {
				stage = fmt.Sprintf(" stage %q", occurrence.Stage)
			}
			writef("    - %s/%s%s %s\n",
				occurrence.Gaggle, occurrence.Workflow, stage, occurrence.Path)
		}
	}

	issues := make([]string, len(result.Summary.P4Issues))
	for i, issue := range result.Summary.P4Issues {
		issues[i] = "#" + strconv.Itoa(issue)
	}
	waiting := fmt.Sprintf("%d waiting on P4 items", result.Summary.WaitingOnP4)
	if len(issues) > 0 {
		waiting += ": " + strings.Join(issues, ", ")
	}
	writef("\nblocked: %d workflows, %d blockers (%d config fixes, %s)\n",
		result.Summary.BlockedWorkflows,
		result.Summary.TotalBlockers,
		result.Summary.ConfigFixes,
		waiting,
	)
	return writeErr
}
