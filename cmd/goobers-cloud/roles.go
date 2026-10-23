package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/workerhost"
)

const (
	apiHelp = `Usage: goobers-cloud api [DATABASE OPTIONS]

Serve the cloud control-plane API.

Database options:
  --database-url URL               PgBouncer transaction-pool URL
  --database-direct-url URL        Direct PostgreSQL URL for session-bound work
  --database-max-conns N           Total connection budget (default 16)
  --database-direct-max-conns N    Direct-pool reservation (default 2)
  --database-auth MODE             password or entra (default password)
`
	controllerHelp = `Usage: goobers-cloud controller [DATABASE OPTIONS]

Run cloud control-plane reconciliation.

Database options are the same as for the api role.
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
	return runDatabaseRole("api", apiHelp, args, stdout, stderr, runAPIHost)
}

func runControllerRole(args []string, stdout, stderr io.Writer) int {
	return runDatabaseRole("controller", controllerHelp, args, stdout, stderr, runControllerHost)
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
	newWorkerHost             = workerhost.New
	runWorkerHost             = func(ctx context.Context, host *workerhost.Host) error { return host.Run(ctx) }
	signalContext             = signals.SetupSignalContext
	newCloudPools             = cloudpg.NewPools
	newDefaultAzureCredential = func() (azcore.TokenCredential, error) {
		return azidentity.NewDefaultAzureCredential(nil)
	}
	runAPIHost        = unavailableDatabaseRole
	runControllerHost = unavailableDatabaseRole
)

type databaseRole func(context.Context, *cloudpg.Pools) error

var errRoleUnavailable = errors.New("role wiring is not available yet")

func unavailableDatabaseRole(context.Context, *cloudpg.Pools) error { return errRoleUnavailable }

func runDatabaseRole(role, help string, args []string, stdout, stderr io.Writer, serve databaseRole) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		write(stdout, help)
		return 0
	}
	flags := flag.NewFlagSet(role, flag.ContinueOnError)
	flags.SetOutput(stderr)
	transactionURL := flags.String("database-url", "", "PgBouncer transaction-pool URL")
	directURL := flags.String("database-direct-url", "", "direct PostgreSQL URL")
	maxConnections := flags.Int("database-max-conns", int(cloudpg.DefaultMaxConnections), "total database connection budget")
	directConnections := flags.Int("database-direct-max-conns", int(cloudpg.DefaultDirectMaxConnections), "direct-pool connection reservation")
	authMode := flags.String("database-auth", "password", "database authentication mode: password or entra")
	flags.Usage = func() { write(stderr, help) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*transactionURL) == "" || strings.TrimSpace(*directURL) == "" {
		flags.Usage()
		return 2
	}
	config := cloudpg.Config{
		TransactionURL: *transactionURL, DirectURL: *directURL,
		MaxConnections: int32(*maxConnections), DirectMaxConnections: int32(*directConnections),
	}
	switch *authMode {
	case "password":
	case "entra":
		credential, err := newDefaultAzureCredential()
		if err != nil {
			writef(stderr, "goobers-cloud %s: initialize Entra credential: %v\n", role, err)
			return 1
		}
		config.TokenProvider = cloudpg.EntraTokenProvider{Credential: credential}
	default:
		writef(stderr, "goobers-cloud %s: invalid --database-auth %q (want password or entra)\n", role, *authMode)
		return 2
	}
	ctx, stop := signalContext()
	defer stop()
	pools, err := newCloudPools(ctx, config)
	if err != nil {
		writef(stderr, "goobers-cloud %s: %v\n", role, err)
		return 1
	}
	defer pools.Close()
	if err := pools.Transaction.Ping(ctx); err != nil {
		writef(stderr, "goobers-cloud %s: transaction pool: %v\n", role, err)
		return 1
	}
	if err := pools.Direct.Ping(ctx); err != nil {
		writef(stderr, "goobers-cloud %s: direct pool: %v\n", role, err)
		return 1
	}
	if err := serve(ctx, pools); err != nil {
		writef(stderr, "goobers-cloud %s: %v\n", role, err)
		return 1
	}
	return 0
}

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
