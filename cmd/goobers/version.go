package main

import (
	"encoding/json"
	"flag"
	"io"

	"github.com/goobers/goobers/internal/version"
)

const versionHelp = "Usage: goobers version [--json]\n" +
	"       goobers --version [--json]\n\n" +
	"Print the build version, commit, build date, Go toolchain, and platform.\n\n" +
	"Default output is a single human-readable line. --json emits a structured\n" +
	"object with keys: version, commit, date, goVersion, platform — the same\n" +
	"fields, machine-readable for scripts and support bundles.\n\n" +
	"Exit codes: 0 = OK, 2 = usage error.\n"

// runVersion backs both `goobers version` and the `goobers --version` alias.
// With no flags it prints the human-readable line (unchanged from the historical
// one-liner); with --json it emits the structured version.Info object.
func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit structured JSON instead of the human-readable line")
	fs.Usage = helpUsage(stderr, "version")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	info := version.Get()
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			pf(stderr, "error: encode version: %v\n", err)
			return 1
		}
		return 0
	}
	pf(stdout, "goobers %s\n", info)
	return 0
}
