package main

import (
	"flag"
	"io"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/fluidui"
)

const labsHelp = "Usage: goobers labs fluid-ui report [--json] [path]\n\n" +
	"Labs (experimental) reports. Results are advisory and never change\n" +
	"configuration, gates, global defaults, or any user's customization.\n\n" +
	"Commands:\n" +
	"  fluid-ui report  rank Fluid UI customization shapes as promotion candidates\n"

func labsCLICommand() cliCommand {
	return groupCommand(
		"labs",
		runLabsUsage,
		groupCommand(
			"fluid-ui",
			runLabsUsage,
			subcommand("labs fluid-ui report", "report", apicontract.ActionReadOnlyNavigation, runLabsFluidUIReport).
				withHelp("Labs: rank Fluid UI customization shapes as advisory promotion candidates", fluidui.ReportCLIHelp).
				withExamples("goobers labs fluid-ui report", "goobers labs fluid-ui report --json ./instance"),
		).withHelp("Labs: Fluid UI customization reports", labsHelp),
	).
		withSynopsis(synopsisByID["labs"]).
		withHelp("Labs: experimental, advisory reports", labsHelp).
		withExamples("goobers labs fluid-ui report")
}

func runLabsUsage(_ []string, _, stderr io.Writer) int {
	pf(stderr, "%s", labsHelp)
	return 2
}

func runLabsFluidUIReport(args []string, stdout, stderr io.Writer) int {
	return fluidui.RunReportCLI(args, newCLIFlagSet("labs fluid-ui report", flag.ContinueOnError), stdout, stderr)
}
