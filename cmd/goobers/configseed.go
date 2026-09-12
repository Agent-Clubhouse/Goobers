package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/goobers/goobers/internal/configmirror"
	"github.com/goobers/goobers/internal/instance"
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
	if err := configmirror.Seed(context.Background(), *mirror, *destination, validateSeededWorkerConfig); err != nil {
		pf(stderr, "error: seed worker config: %v\n", err)
		return 1
	}
	pln(stdout, "Worker configuration seed is complete and validated.")
	return 0
}

func validateSeededWorkerConfig(root string) error {
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return err
	}
	if config.WorkflowSource != nil || config.ConfigMirrorPath != "" {
		return errors.New("worker seed must not fetch config sources or publish mirrors")
	}
	set, report, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		return fmt.Errorf("seeded worker config is invalid: %w (%s)", err, validationIssueSummary(report))
	}
	// Parsing YAML alone is insufficient: first dispatch also needs every
	// referenced instruction body and skill package, including large prose.
	_, _, err = loadSnapshotGooberInputs(layout.ConfigDir(), set)
	if err != nil {
		return err
	}
	// The digest loader also verifies asset bytes against preserved source
	// modes, including when an init-container retry reuses a completed seed.
	_, err = configDirectoryDigest(layout.ConfigDir())
	return err
}
