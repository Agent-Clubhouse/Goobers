package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	cloudacceptance "github.com/goobers/goobers/internal/cloud/acceptance"
	cloudclaims "github.com/goobers/goobers/internal/cloud/claims"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	cloudmigrate "github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/operatorcli"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/workerhost"
)

const (
	apiHelp = `Usage: goobers-cloud api [DATABASE OPTIONS]

Serve the cloud control-plane API.

Database options:
  --database-url URL               PgBouncer URL (prefer env/file for credentials)
  --database-url-file PATH         Read PgBouncer URL from a mounted Secret
  --database-direct-url URL        Direct URL (prefer env/file for credentials)
  --database-direct-url-file PATH  Read direct URL from a mounted Secret
  --database-max-conns N           Total connection budget (default 16)
  --database-direct-max-conns N    Direct-pool reservation (default 2)
  --database-auth MODE             password or entra (default password)

Environment:
  GOOBERS_DATABASE_URL         PgBouncer transaction-pool URL
  GOOBERS_DATABASE_DIRECT_URL  Direct PostgreSQL URL
`
	controllerHelp = `Usage: goobers-cloud controller [DATABASE OPTIONS]

Run cloud control-plane reconciliation.

Database options are the same as for the api role.
`
	migrateHelp = `Usage: goobers-cloud migrate apply [OPTIONS]

Apply ordered cloud PostgreSQL migrations.

Options:
  --database-direct-url URL        Direct URL (prefer env/file for credentials)
  --database-direct-url-file PATH  Read direct URL from a mounted Secret
  --database-auth MODE             password or entra (default password)
  --backup-ref REF                 Restorable backup identifier or PITR mark (required)
  --instance-id ID                 Goobers instance owning the migration lease (required)
  --holder ID                      Unique migration pod/process identity

Environment:
  GOOBERS_DATABASE_DIRECT_URL  Direct PostgreSQL URL
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
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		write(stdout, migrateHelp)
		return 0
	}
	if len(args) == 0 || args[0] != "apply" {
		write(stderr, migrateHelp)
		return 2
	}
	flags := flag.NewFlagSet("migrate apply", flag.ContinueOnError)
	flags.SetOutput(stderr)
	directURLArg := flags.String("database-direct-url", "", "direct PostgreSQL URL")
	directURLFile := flags.String("database-direct-url-file", "", "file containing the direct PostgreSQL URL")
	authMode := flags.String("database-auth", environmentOr("GOOBERS_DATABASE_AUTH", "password"), "database authentication mode: password or entra")
	backupRef := flags.String("backup-ref", os.Getenv("GOOBERS_MIGRATION_BACKUP_REF"), "restorable backup identifier or PITR mark")
	instanceID := flags.String("instance-id", os.Getenv("GOOBERS_INSTANCE_ID"), "Goobers instance identifier")
	holder := flags.String("holder", environmentOr("GOOBERS_REPLICA_ID", defaultMigrationHolder()), "migration pod/process identity")
	flags.Usage = func() { write(stderr, migrateHelp) }
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*backupRef) == "" || strings.TrimSpace(*instanceID) == "" {
		flags.Usage()
		return 2
	}
	directURL, err := cloudpg.DatabaseURL(*directURLArg, *directURLFile, "GOOBERS_DATABASE_DIRECT_URL")
	if err != nil {
		writef(stderr, "goobers-cloud migrate: direct database URL: %v\n", err)
		return 2
	}
	config := cloudpg.Config{TransactionURL: directURL, DirectURL: directURL}
	if code := configureDatabaseAuth("migrate", *authMode, &config, stderr); code != 0 {
		return code
	}
	ctx, stop := signalContext()
	defer stop()
	pools, err := newCloudPools(ctx, config)
	if err != nil {
		writef(stderr, "goobers-cloud migrate: %s\n", cloudpg.SafeDiagnostic(err))
		return 1
	}
	defer pools.Close()
	if err := runCloudMigrations(ctx, pools, *backupRef, *instanceID, *holder); err != nil {
		writef(stderr, "goobers-cloud migrate: %s\n", cloudpg.SafeDiagnostic(err))
		return 1
	}
	writeln(stdout, "cloud migrations applied")
	return 0
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
	newAcceptanceStore        = cloudacceptance.New
	newDefaultAzureCredential = func() (azcore.TokenCredential, error) {
		return azidentity.NewDefaultAzureCredential(nil)
	}
	runAPIHost         = unavailableDatabaseRole
	runControllerHost  = unavailableDatabaseRole
	checkCloudSchema   = cloudmigrate.CheckPoolCompatibility
	runCloudMigrations = func(ctx context.Context, pools *cloudpg.Pools, backupRef, instanceID, holder string) error {
		catalog, err := cloudmigrate.Catalog()
		if err != nil {
			return err
		}
		return (cloudmigrate.Runner{
			Direct: pools.Direct, Catalog: catalog, BackupRef: backupRef,
			InstanceID: instanceID, Holder: holder,
		}).Apply(ctx)
	}
	loadMigrationStatus = cloudmigrate.Status
	loadLeaseStatus     = cloudlease.Status
	pingCloudPools      = func(ctx context.Context, pools *cloudpg.Pools) (string, error) {
		if err := pools.Transaction.Ping(ctx); err != nil {
			return "transaction", err
		}
		return "direct", pools.Direct.Ping(ctx)
	}
)

type databaseServices struct {
	Pools               *cloudpg.Pools
	Acceptances         triggerqueue.SchedulerPolicyStore
	SchemaCompatibility error
	NewClaims           func(*cloudlease.Manager, *cloudlease.Handle) (cloudclaims.Services, error)
}

type databaseRole func(context.Context, databaseServices) error

func runMigrateStatus(args []string, stdout, stderr io.Writer) int {
	return runDatabaseRole("migrate status", operatorcli.Help("migrate status"), args, stdout, stderr, func(ctx context.Context, services databaseServices) error {
		status, err := loadMigrationStatus(ctx, services.Pools.Transaction, version.Get().Version)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(status)
	})
}

func runLeaseStatus(args []string, stdout, stderr io.Writer) int {
	return runDatabaseRole("lease status", operatorcli.Help("lease status"), args, stdout, stderr, func(ctx context.Context, services databaseServices) error {
		status, err := loadLeaseStatus(ctx, services.Pools.Transaction)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(status)
	})
}

var errRoleUnavailable = errors.New("role wiring is not available yet")

func unavailableDatabaseRole(context.Context, databaseServices) error { return errRoleUnavailable }

func runDatabaseRole(role, help string, args []string, stdout, stderr io.Writer, serve databaseRole) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		write(stdout, help)
		return 0
	}
	flags := flag.NewFlagSet(role, flag.ContinueOnError)
	flags.SetOutput(stderr)
	transactionURLArg := flags.String("database-url", "", "PgBouncer transaction-pool URL")
	transactionURLFile := flags.String("database-url-file", "", "file containing the PgBouncer transaction-pool URL")
	directURLArg := flags.String("database-direct-url", "", "direct PostgreSQL URL")
	directURLFile := flags.String("database-direct-url-file", "", "file containing the direct PostgreSQL URL")
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
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	transactionURL, err := cloudpg.DatabaseURL(*transactionURLArg, *transactionURLFile, "GOOBERS_DATABASE_URL")
	if err != nil {
		writef(stderr, "goobers-cloud %s: transaction database URL: %v\n", role, err)
		return 2
	}
	directURL, err := cloudpg.DatabaseURL(*directURLArg, *directURLFile, "GOOBERS_DATABASE_DIRECT_URL")
	if err != nil {
		writef(stderr, "goobers-cloud %s: direct database URL: %v\n", role, err)
		return 2
	}
	config := cloudpg.Config{
		TransactionURL: transactionURL, DirectURL: directURL,
		MaxConnections: int32(*maxConnections), DirectMaxConnections: int32(*directConnections),
	}
	if code := configureDatabaseAuth(role, *authMode, &config, stderr); code != 0 {
		return code
	}
	ctx, stop := signalContext()
	defer stop()
	pools, err := newCloudPools(ctx, config)
	if err != nil {
		writef(stderr, "goobers-cloud %s: %s\n", role, cloudpg.SafeDiagnostic(err))
		return 1
	}
	defer pools.Close()
	if pool, err := pingCloudPools(ctx, pools); err != nil {
		writef(stderr, "goobers-cloud %s: %s pool: %s\n", role, pool, cloudpg.SafeDiagnostic(err))
		return 1
	}
	compatibilityErr := checkCloudSchema(ctx, pools.Transaction, version.Get().Version)
	if compatibilityErr != nil && !cloudmigrate.IsIncompatible(compatibilityErr) {
		writef(stderr, "goobers-cloud %s: check database schema compatibility: %s\n", role, cloudpg.SafeDiagnostic(compatibilityErr))
		return 1
	}
	acceptances, err := newAcceptanceStore(
		pools.Transaction,
		environmentOr("GOOBERS_INSTANCE_ID", "default"),
	)
	if err != nil {
		writef(stderr, "goobers-cloud %s: acceptance store initialization failed\n", role)
		return 1
	}
	services := databaseServices{
		Pools: pools, Acceptances: acceptances, SchemaCompatibility: compatibilityErr,
		NewClaims: func(manager *cloudlease.Manager, handle *cloudlease.Handle) (cloudclaims.Services, error) {
			return cloudclaims.New(
				pools.Transaction,
				environmentOr("GOOBERS_INSTANCE_ID", "default"),
				manager, handle,
			)
		},
	}
	if err := serve(ctx, services); err != nil {
		writef(stderr, "goobers-cloud %s: %v\n", role, err)
		return 1
	}
	return 0
}

func configureDatabaseAuth(role, authMode string, config *cloudpg.Config, stderr io.Writer) int {
	switch authMode {
	case "password":
	case "entra":
		credential, err := newDefaultAzureCredential()
		if err != nil {
			writef(stderr, "goobers-cloud %s: Entra credential initialization failed\n", role)
			return 1
		}
		config.TokenProvider = cloudpg.EntraTokenProvider{Credential: credential}
	default:
		writef(stderr, "goobers-cloud %s: invalid --database-auth %q (want password or entra)\n", role, authMode)
		return 2
	}
	return 0
}

func environmentOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func defaultMigrationHolder() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-host"
	}
	return fmt.Sprintf("%s-%d", hostname, os.Getpid())
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
