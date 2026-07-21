package main

import (
	"encoding/json"
	"flag"
	"io"
	"text/tabwriter"

	"github.com/goobers/goobers/internal/supportmatrix"
)

const versionsHelp = "Usage: goobers versions [--json]\n\n" +
	"Print the version-support matrix this build of goobers declares: the\n" +
	"minimum Go toolchain it compiles against, the OS/arch targets it claims to\n" +
	"support (with their tier), and where the current host stands in that matrix.\n\n" +
	"The matrix is host-declared — a build-time constant, not a live probe — so it\n" +
	"answers \"what does this binary claim to support?\" for operators and support\n" +
	"bundles. Distinct from `goobers version`, which reports this build's own\n" +
	"version/commit/date.\n\n" +
	"Default output is human-readable. --json emits a structured object with keys:\n" +
	"minGoVersion, platforms[] (os, arch, tier), host (os, arch, goVersion,\n" +
	"supported, tier) — machine-readable for scripts and support bundles.\n\n" +
	"Exit codes: 0 = OK, 2 = usage error.\n"

// runVersions backs `goobers versions`. With no flags it renders the declared
// support matrix as a human-readable table; with --json it emits the structured
// supportmatrix.Report. It mirrors `goobers version`'s --json shape (#1098) so
// the two version surfaces feel the same.
func runVersions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("versions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit structured JSON instead of the human-readable table")
	fs.Usage = helpUsage(stderr, "versions")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	report := supportmatrix.NewReport()
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			pf(stderr, "error: encode support matrix: %v\n", err)
			return 1
		}
		return 0
	}

	writeSupportMatrix(stdout, report)
	return 0
}

// writeSupportMatrix renders the report as a stable, human-readable block: the
// minimum Go toolchain, the platform table, and the current host's standing.
func writeSupportMatrix(w io.Writer, r supportmatrix.Report) {
	pf(w, "goobers support matrix\n")
	pf(w, "  minimum Go toolchain: %s\n\n", r.MinGoVersion)

	pf(w, "  supported platforms:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range r.Platforms {
		pf(tw, "    %s/%s\t%s\n", p.OS, p.Arch, p.Tier)
	}
	_ = tw.Flush()

	pf(w, "\n  this host: %s/%s (%s) — %s\n",
		r.Host.OS, r.Host.Arch, r.Host.GoVersion, hostStanding(r.Host))
}

// hostStanding is the one-word summary of where the running host sits in the
// declared matrix.
func hostStanding(h supportmatrix.Host) string {
	if !h.Supported {
		return "not in the declared support matrix"
	}
	return string(h.Tier)
}
