package main

import (
	"flag"
	"io"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
)

func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	demo := fs.Bool("demo", false, "seed a credential-free runnable demo workflow")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers init [--demo] [path]\n\n"+
			"Scaffold an instance root at path (default \".\"): instance.yaml, config/\n"+
			"(seeded with a starter example), runs/, scheduler/, workcopies/, and a\n"+
			"telemetry.db placeholder. Re-running is safe — existing pieces are left\n"+
			"untouched. --demo seeds an offline deterministic tour requiring no repo\n"+
			"or credentials.\n")
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

	var res *instance.InitResult
	var err error
	if *demo {
		res, err = instance.InitDemo(root)
	} else {
		res, err = instance.Init(root)
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	abs, err := filepath.Abs(res.Root)
	if err != nil {
		abs = res.Root
	}
	if len(res.Created) == 0 {
		pf(stdout, "instance already initialized at %s (nothing to do)\n", abs)
		return 0
	}
	pf(stdout, "initialized instance at %s\n", abs)
	for _, c := range res.Created {
		pf(stdout, "  created  %s\n", c)
	}
	for _, s := range res.Skipped {
		pf(stdout, "  skipped  %s (already exists)\n", s)
	}
	instanceConfigCreated := false
	configSeeded := false
	for _, created := range res.Created {
		switch created {
		case instance.ConfigFileName:
			instanceConfigCreated = true
		case instance.ConfigDirName:
			configSeeded = true
		}
	}
	if *demo && configSeeded {
		pf(stdout, demoTourBanner, abs)
	} else if !*demo && instanceConfigCreated && configSeeded {
		pf(stdout, starterSetupBanner, abs, abs, abs)
	}
	return 0
}

const starterSetupBanner = `
Before goobers up can dispatch work:
  1. Replace your-org/your-repo in:
       %s/instance.yaml
       %s/config/gaggles/example/gaggle.yaml
  2. Export a GitHub token: export GOOBERS_GITHUB_TOKEN=...
  3. Add the 'goobers' label to an open issue in that repository.
  4. Start the daemon: goobers up %s
`

const demoTourBanner = `
Demo tour (run these from %s):
  goobers up          # in one terminal
  goobers run demo    # watch stages execute and gate branch
  goobers trace <id>  # see the journal the run left behind
`
