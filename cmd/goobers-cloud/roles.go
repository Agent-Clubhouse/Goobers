package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"

	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/workerhost"
)

const (
	apiHelp = `Usage: goobers-cloud api

Serve the cloud control-plane API. The P1 skeleton reserves this role while
the extracted API host is wired into the cloud composition root.
`
	controllerHelp = `Usage: goobers-cloud controller

Run cloud control-plane reconciliation. The P1 skeleton reserves this role
while the extracted controller host is wired into the cloud composition root.
`
	migrateHelp = `Usage: goobers-cloud migrate

Apply cloud data-store migrations. The P1 skeleton reserves this role while
the migration runner is wired into the cloud composition root.
`
	workerHelp = `Usage: goobers-cloud worker [OPTIONS]

Run the shared Temporal engine worker host.

Options:
  --temporal-hostport HOST:PORT  Temporal frontend address (default localhost:7233)
  --temporal-namespace NAME      Temporal namespace (default default)
  --task-queue NAME              Task queue to serve (repeatable; default goobers)
  --health-addr ADDRESS          Serve /healthz and /readyz on this address
  --drain-timeout DURATION       Graceful drain bound (default 30s)
  --max-concurrent-activities N  Activity concurrency (default 1000)
`
)

func runAPIRole(args []string, stdout, stderr io.Writer) int {
	return runReservedRole("api", apiHelp, args, stdout, stderr)
}

func runControllerRole(args []string, stdout, stderr io.Writer) int {
	return runReservedRole("controller", controllerHelp, args, stdout, stderr)
}

func runMigrateRole(args []string, stdout, stderr io.Writer) int {
	return runReservedRole("migrate", migrateHelp, args, stdout, stderr)
}

func runReservedRole(role, help string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		write(stdout, help)
		return 0
	}
	if len(args) != 0 {
		write(stderr, help)
		return 2
	}
	writef(stderr, "goobers-cloud %s: role wiring is not available in the P1 skeleton\n", role)
	return 1
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("task queue must not be empty")
	}
	*values = append(*values, value)
	return nil
}

var (
	newWorkerHost = workerhost.New
	runWorkerHost = func(ctx context.Context, host *workerhost.Host) error { return host.Run(ctx) }
	signalContext = signals.SetupSignalContext
)

func runWorkerRole(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		write(stdout, workerHelp)
		return 0
	}
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var queues stringList
	flags.Var(&queues, "task-queue", "task queue to serve (repeatable)")
	hostPort := flags.String("temporal-hostport", "localhost:7233", "Temporal frontend host:port")
	namespace := flags.String("temporal-namespace", "default", "Temporal namespace")
	healthAddr := flags.String("health-addr", "", "serve liveness and readiness probes on this address")
	drainTimeout := flags.Duration("drain-timeout", workerhost.DefaultDrainTimeout, "graceful drain bound")
	maxActivities := flags.Int("max-concurrent-activities", workerhost.DefaultMaxConcurrentActivities, "maximum concurrent activities")
	flags.Usage = func() { write(stderr, workerHelp) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	if len(queues) == 0 {
		queues = append(queues, "goobers")
	}
	host, err := newWorkerHost(workerhost.Config{
		HostPort:                *hostPort,
		Namespace:               *namespace,
		TaskQueues:              queues,
		HealthAddr:              *healthAddr,
		MaxConcurrentActivities: *maxActivities,
		DrainTimeout:            *drainTimeout,
		BuildVersion:            version.Get().Version,
	})
	if err != nil {
		writef(stderr, "goobers-cloud worker: %v\n", err)
		return 2
	}
	ctx, stop := signalContext()
	defer stop()
	writef(stdout, "goobers-cloud worker: serving task queue(s) %s on %s (namespace %s)\n", strings.Join(queues, ", "), *hostPort, *namespace)
	if err := runWorkerHost(ctx, host); err != nil {
		writef(stderr, "goobers-cloud worker: %v\n", err)
		if errors.Is(err, workerhost.ErrAbandonedWork) {
			return 3
		}
		return 1
	}
	writeln(stdout, "goobers-cloud worker: drained cleanly")
	return 0
}
