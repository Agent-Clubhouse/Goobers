package main

import (
	"errors"
	"io"
	"io/fs"
	"text/tabwriter"

	configexamples "github.com/goobers/goobers/config-examples"
)

const examplesHelp = "Usage: goobers examples <list|show> [name]\n\n" +
	"Browse the canonical workflow examples embedded in this binary. No source\n" +
	"checkout or instance root is required.\n\n" +
	"Commands:\n" +
	"  list         print the available example names and descriptions\n" +
	"  show <name>  print an example's exact Workflow YAML\n\n" +
	"Run `goobers examples list -h` or `goobers examples show -h` for details.\n"

const examplesListHelp = "Usage: goobers examples list\n\n" +
	"Print the canonical embedded workflow examples, one per line, as the\n" +
	"example name followed by its one-line description. Pass a name to\n" +
	"`goobers examples show` to print its YAML.\n\n" +
	"Exit codes: 0 = listed, 1 = embedded catalog error, 2 = usage error.\n"

const examplesShowHelp = "Usage: goobers examples show <name>\n\n" +
	"Print the exact canonical Workflow YAML embedded in this binary. Use\n" +
	"`goobers examples list` to discover names. No source checkout or instance\n" +
	"root is required.\n\n" +
	"Exit codes: 0 = printed, 1 = unknown name or embedded catalog error,\n" +
	"2 = usage error.\n"

func runExamples(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && isHelpArg(args[0]) {
		pf(stdout, "%s", examplesHelp)
		return 0
	}
	if len(args) > 0 {
		pf(stderr, "error: unknown examples command %q\n", args[0])
	}
	pf(stderr, "%s", examplesHelp)
	return 2
}

func runExamplesList(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && isHelpArg(args[0]) {
		pf(stdout, "%s", examplesListHelp)
		return 0
	}
	if len(args) != 0 {
		pf(stderr, "%s", examplesListHelp)
		return 2
	}

	examples, err := configexamples.WorkflowExamples()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, example := range examples {
		if example.Description == "" {
			pln(tw, example.Name)
			continue
		}
		pf(tw, "%s\t%s\n", example.Name, example.Description)
	}
	if err := tw.Flush(); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func runExamplesShow(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && isHelpArg(args[0]) {
		pf(stdout, "%s", examplesShowHelp)
		return 0
	}
	if len(args) != 1 {
		pf(stderr, "%s", examplesShowHelp)
		return 2
	}

	data, err := configexamples.ReadWorkflowExample(args[0])
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			pf(stderr, "error: unknown workflow example %q; run `goobers examples list` to see available names\n", args[0])
		} else {
			pf(stderr, "error: %v\n", err)
		}
		return 1
	}
	pf(stdout, "%s", data)
	return 0
}

func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}
