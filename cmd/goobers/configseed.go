package main

import (
	"context"
	"flag"
	"io"

	"github.com/goobers/goobers/internal/configmirror"
	"github.com/goobers/goobers/internal/runtimewiring"
)

const configSeedHelp = `Usage: goobers config-seed --mirror <absolute-path> --instance <absolute-path>

Seed a dedicated worker instance from the daemon's rendered-config mirror.
The mirror is read-only; no config-repository credentials are needed.
The instance path must be a child of a private worker volume, not a mount point.
A complete validated tree is published at once. A repeated init-container run
validates and retains its completed seed, even if the mirror has since changed.
It refuses to overwrite an unrelated instance. Recreate the private worker
volume to seed a newer generation; this command is not a live config updater.
`

func runConfigSeed(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("config-seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mirror := fs.String("mirror", "", "absolute path to the read-only rendered config mirror")
	destination := fs.String("instance", "", "absolute path to a dedicated private worker instance")
	fs.Usage = helpUsage(stderr, "config-seed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *mirror == "" || *destination == "" {
		fs.Usage()
		return 2
	}
	if err := configmirror.Seed(context.Background(), *mirror, *destination, runtimewiring.ValidateSeededWorkerConfig); err != nil {
		pf(stderr, "error: seed worker config: %v\n", err)
		return 1
	}
	pln(stdout, "Worker configuration seed is complete and validated.")
	return 0
}
