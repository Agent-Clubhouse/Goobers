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

	"github.com/goobers/goobers/internal/blobstore"
	cloudacceptance "github.com/goobers/goobers/internal/cloud/acceptance"
	cloudclaims "github.com/goobers/goobers/internal/cloud/claims"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	cloudmigrate "github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/cloud/operatorcli"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/version"
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
  --object-store-url URL           Azure Blob service URL
  --object-store-container NAME    Blob container (default goobers)
  --experimental-physical-blob-gc  Enable unqualified physical deletion (#7843)

Environment:
  GOOBERS_DATABASE_URL         PgBouncer transaction-pool URL
  GOOBERS_DATABASE_DIRECT_URL  Direct PostgreSQL URL
  GOOBERS_OBJECT_STORE_URL     Azure Blob service URL
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

var (
	signalContext             = signals.SetupSignalContext
	newCloudPools             = cloudpg.NewPools
	newAzureObjectStore       = objstore.NewAzure
	newObjectMetadata         = objstore.NewMetadataWithOptions
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
	Blobs               blobstore.Store
	Surrenders          dispatcher.SurrenderPlane
	Objects             *objstore.Metadata
	Acceptances         triggerqueue.SchedulerPolicyStore
	SchemaCompatibility error
	NewClaims           func(*cloudlease.Manager, *cloudlease.Handle) (cloudclaims.Services, error)
}

type databaseRole func(context.Context, databaseServices) error

func runMigrateStatus(args []string, stdout, stderr io.Writer) int {
	return runReadOnlyDatabaseRole("migrate status", operatorcli.Help("migrate status"), args, stdout, stderr, func(ctx context.Context, services databaseServices) error {
		status, err := loadMigrationStatus(ctx, services.Pools.Transaction, version.Get().Version)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(status)
	})
}

func runLeaseStatus(args []string, stdout, stderr io.Writer) int {
	return runReadOnlyDatabaseRole("lease status", operatorcli.Help("lease status"), args, stdout, stderr, func(ctx context.Context, services databaseServices) error {
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
	return runDatabaseRoleMode(role, help, args, stdout, stderr, serve, true)
}

func runReadOnlyDatabaseRole(role, help string, args []string, stdout, stderr io.Writer, serve databaseRole) int {
	return runDatabaseRoleMode(role, help, args, stdout, stderr, serve, false)
}

func runDatabaseRoleMode(role, help string, args []string, stdout, stderr io.Writer, serve databaseRole, initializeServices bool) int {
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
	objectStoreURL := flags.String("object-store-url", os.Getenv("GOOBERS_OBJECT_STORE_URL"), "Azure Blob service URL")
	objectStoreContainer := flags.String("object-store-container", environmentOr("GOOBERS_OBJECT_STORE_CONTAINER", "goobers"), "Azure Blob container")
	experimentalPhysicalBlobGC := flags.Bool(
		"experimental-physical-blob-gc", false,
		"enable experimental physical blob deletion (#7843)",
	)
	flags.Usage = func() { write(stderr, help) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || (initializeServices && (strings.TrimSpace(*objectStoreURL) == "" || strings.TrimSpace(*objectStoreContainer) == "")) {
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
	services := databaseServices{Pools: pools, SchemaCompatibility: compatibilityErr}
	if !initializeServices {
		if err := serve(ctx, services); err != nil {
			writef(stderr, "goobers-cloud %s: %v\n", role, err)
			return 1
		}
		return 0
	}
	objectCredential, err := newDefaultAzureCredential()
	if err != nil {
		writef(stderr, "goobers-cloud %s: object-store credential initialization failed\n", role)
		return 1
	}
	blobs, err := newAzureObjectStore(*objectStoreURL, *objectStoreContainer, objectCredential)
	if err != nil {
		writef(stderr, "goobers-cloud %s: object-store initialization failed\n", role)
		return 1
	}
	objects, err := newObjectMetadata(pools.Direct, blobs, objstore.MetadataOptions{
		ExperimentalPhysicalDelete: *experimentalPhysicalBlobGC,
	})
	if err != nil {
		writef(stderr, "goobers-cloud %s: object metadata initialization failed\n", role)
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
	services.Blobs, services.Objects, services.Acceptances = blobs, objects, acceptances
	services.Surrenders = objstore.SurrenderPlane{
		Metadata: objects, InstanceID: environmentOr("GOOBERS_INSTANCE_ID", "default"),
	}
	services.NewClaims = func(manager *cloudlease.Manager, handle *cloudlease.Handle) (cloudclaims.Services, error) {
		return cloudclaims.New(
			pools.Transaction,
			environmentOr("GOOBERS_INSTANCE_ID", "default"),
			manager, handle,
		)
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
	return runWorker(args, stdout, stderr)
}
