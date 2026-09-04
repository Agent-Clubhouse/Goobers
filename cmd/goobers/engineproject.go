package main

import (
	"context"
	"flag"
	"io"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel/intake"
)

const engineProjectHelp = "Usage: goobers engine-project [flags] <run-id> [path]\n\n" +
	"Write a completed engine run's standard journal into the instance.\n\n" +
	"Exit codes: 0 = projected or already present, 1 = query/write failure,\n" +
	"2 = usage/config error.\n"

func runEngineProject(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("engine-project", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gaggle := fs.String("gaggle", "", "gaggle owning the run")
	hostPort := fs.String("temporal-hostport", "", "Temporal frontend host:port")
	namespace := fs.String("temporal-namespace", "", "Temporal namespace")
	fs.Usage = helpUsage(stderr, "engine-project")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 || *gaggle == "" {
		pf(stderr, "usage: goobers engine-project --gaggle <name> [flags] <run-id> [path]\n")
		return 2
	}
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		pf(stderr, "error: load instance config: %v\n", err)
		return 2
	}
	engineConfig := cfg.EffectiveEngineConfig()
	if *hostPort == "" {
		*hostPort = engineConfig.HostPort
	}
	if *namespace == "" {
		*namespace = engineConfig.Namespace
	}
	watermarks, err := intake.Open(l.IntakeDB())
	if err != nil {
		pf(stderr, "error: open read-model intake: %v\n", err)
		return 1
	}
	defer func() { _ = watermarks.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := dialEngineTemporal(ctx, client.Options{HostPort: *hostPort, Namespace: *namespace})
	if err != nil {
		pf(stderr, "error: dial temporal at %s: %v\n", *hostPort, err)
		return 1
	}
	defer c.Close()
	dir, err := engine.ProjectCompletedRunForGaggle(ctx, c, fs.Arg(0), *gaggle, l.ForGaggle(*gaggle).RunsDir(), watermarks.Observed)
	if err != nil {
		pf(stderr, "error: project run %s: %v\n", fs.Arg(0), err)
		return 1
	}
	pf(stdout, "journal written: %s\n", dir)
	return 0
}
