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
	fs.Usage = func() {
		pf(stderr, "Usage: goobers init [path]\n\n"+
			"Scaffold an instance root at path (default \".\"): instance.yaml, config/\n"+
			"(seeded with a starter example), runs/, scheduler/, workcopies/, and a\n"+
			"telemetry.db placeholder. Re-running is safe — existing pieces are left\n"+
			"untouched.\n")
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

	res, err := instance.Init(root)
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
	return 0
}
