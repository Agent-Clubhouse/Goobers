package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
)

func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	checkHarness := fs.Bool("check-harness", false, "also verify every referenced agent harness is installed and signed in")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers validate [--check-harness] [path]\n\n"+
			"Validate an instance's instance.yaml and config/ directory (default\n"+
			"path \".\"). --check-harness additionally preflights every agent harness\n"+
			"referenced by a goober (GBO-011) — installed, signed in, actionable\n"+
			"guidance otherwise. Exit codes: 0 = valid, 1 = validation errors, 2 = usage/IO error.\n")
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

	// api/validate's cross-reference checks (above) mirror most of
	// workflow.Compile's own semantic analysis (CheckReachability/
	// CheckSchedules/CheckGateOutcomes/CheckAdmission), but this is the one
	// point that actually calls Compile with the same options `up`/`run` use
	// at daemon startup — including WithKnownChecks, which nothing else here
	// validates (#124). A config that fails this would also fail to start
	// the daemon; catching that now, at `validate` time, is the whole point.
	if _, err := compiledMachines(set, goobersByName(set)); err != nil {
		pf(stdout, "\nINVALID workflow: %v\n", err)
		return 1
	}

	if *checkHarness {
		if !checkHarnesses(set.Goobers, stdout, stderr) {
			return 1
		}
	}
	return 0
}

// harnessAdapterFor is the harness-adapter lookup checkHarnesses uses.
// Package-level so tests can substitute a fake lookup without depending on a
// real, installed, signed-in Copilot CLI.
var harnessAdapterFor = adapterFor

// checkHarnesses preflights every distinct harness referenced by set's
// goobers (GBO-011), printing actionable guidance per failure. Returns false
// if any harness failed its preflight.
func checkHarnesses(goobers []apiv1.Goober, stdout, stderr io.Writer) bool {
	seen := map[apiv1.Harness]bool{}
	ok := true
	for _, g := range goobers {
		h := g.Spec.Harness
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true

		adapter, err := harnessAdapterFor(h)
		if err != nil {
			pf(stdout, "HARNESS %s: %v\n", h, err)
			ok = false
			continue
		}
		if err := adapter.Preflight(context.Background()); err != nil {
			pf(stdout, "HARNESS %s: %v\n", h, err)
			ok = false
			continue
		}
		pf(stdout, "HARNESS %s: OK\n", h)
	}
	return ok
}

// adapterFor returns the default adapter for a goober-declared harness kind.
// v1/V0 supports only the GitHub Copilot CLI (GBO-040).
func adapterFor(h apiv1.Harness) (harness.Adapter, error) {
	switch h {
	case apiv1.HarnessCopilot:
		return &harness.CopilotAdapter{Command: []string{"copilot"}}, nil
	default:
		return nil, errUnknownHarness(h)
	}
}

type errUnknownHarness apiv1.Harness

func (e errUnknownHarness) Error() string {
	return "unknown harness " + string(e) + " (V0 supports \"copilot\" only)"
}
