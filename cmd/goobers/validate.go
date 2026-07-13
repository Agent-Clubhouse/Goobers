package main

import (
	"errors"
	"flag"
	"io"
	"os"

	"github.com/goobers/goobers/internal/instance"
)

func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers validate [path]\n\n"+
			"Validate an instance's instance.yaml and config/ directory (default\n"+
			"path \".\"). Exit codes: 0 = valid, 1 = validation errors, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
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

	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}

	if _, err := instance.LoadConfig(l.ConfigFile()); err != nil {
		pf(stdout, "INVALID instance.yaml:\n  %v\n", err)
		return 1
	}

	set, report, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil && !errors.Is(err, instance.ErrInvalidConfig) {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if report != nil {
		for _, issue := range report.Issues {
			pln(stdout, issue.String())
		}
	}
	if errors.Is(err, instance.ErrInvalidConfig) {
		pf(stdout, "\nconfig directory failed validation\n")
		return 1
	}

	pf(stdout, "OK: instance.yaml valid; config/ valid (%d gaggle(s), %d goober(s), %d workflow(s))\n",
		len(set.Gaggles), len(set.Goobers), len(set.Workflows))
	return 0
}
