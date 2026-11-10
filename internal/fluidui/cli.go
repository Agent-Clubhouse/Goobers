package fluidui

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// ReportCLIHelp documents `goobers labs fluid-ui report`.
const ReportCLIHelp = `Usage: goobers labs fluid-ui report [--json] [path]

Labs (experimental): rank the Fluid UI customization shapes users apply as
candidates for the standard Portal (#8419). Shapes are ranked by distinct
users who kept them applied for at least 7 days, then by a lower reversion
rate, then by distinct users. One user's repeated revisions count once.
Unmet needs list request categories the customization schema cannot express.

The report reads the learning log under <path>/labs/fluid-ui (default path
"."). It holds only counts, shapes, and category labels: never prompt text,
identities, panel IDs, saved-view names, or routes.

Candidates are advisory product proposals. They never change global defaults
or any user's customization.
`

// RunReportCLI implements `goobers labs fluid-ui report`.
func RunReportCLI(args []string, fs *flag.FlagSet, stdout, stderr io.Writer) int {
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, ReportCLIHelp) }
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	report, err := ForInstance(root).Report()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return 1
		}
		return 0
	}
	writeReport(stdout, report)
	return 0
}

func writeReport(w io.Writer, report Report) {
	_, _ = fmt.Fprintf(w, "Labs Fluid UI promotion candidates\n%s\n\n", report.Advisory)
	_, _ = fmt.Fprintf(w, "%d distinct user(s). Sustained means kept applied for at least %d days.\n\n", report.Users, report.SustainedAfterDays)
	if len(report.Candidates) == 0 {
		_, _ = fmt.Fprintln(w, "No customization shapes recorded yet.")
	} else {
		table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(table, "RANK\tSHAPE\tUSERS\tSUSTAINED\tACTIVE\tREVERTED\tREVERSION")
		for _, c := range report.Candidates {
			_, _ = fmt.Fprintf(table, "%d\t%s\t%d\t%d\t%d\t%d\t%.0f%%\n", c.Rank, strings.Join(c.Shape, " + "), c.Users, c.SustainedUsers, c.ActiveUsers, c.RevertedUsers, c.ReversionRate*100)
		}
		_ = table.Flush()
	}
	_, _ = fmt.Fprintln(w, "\nUnmet needs")
	if len(report.UnmetNeeds) == 0 {
		_, _ = fmt.Fprintln(w, "None recorded.")
		return
	}
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "NEED\tUSERS")
	for _, need := range report.UnmetNeeds {
		_, _ = fmt.Fprintf(table, "%s\t%d\n", need.Label, need.Users)
	}
	_ = table.Flush()
}
