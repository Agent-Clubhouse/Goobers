package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	cloudmigrate "github.com/goobers/goobers/internal/cloud/migrate"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/signals"
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
	directURL, err := databaseURL(*directURLArg, *directURLFile, "GOOBERS_DATABASE_DIRECT_URL")
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
)

type databaseRole func(context.Context, *cloudpg.Pools, error) error

var errRoleUnavailable = errors.New("role wiring is not available yet")

func unavailableDatabaseRole(context.Context, *cloudpg.Pools, error) error { return errRoleUnavailable }

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
	transactionURL, err := databaseURL(*transactionURLArg, *transactionURLFile, "GOOBERS_DATABASE_URL")
	if err != nil {
		writef(stderr, "goobers-cloud %s: transaction database URL: %v\n", role, err)
		return 2
	}
	directURL, err := databaseURL(*directURLArg, *directURLFile, "GOOBERS_DATABASE_DIRECT_URL")
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
	if err := pools.Transaction.Ping(ctx); err != nil {
		writef(stderr, "goobers-cloud %s: transaction pool: %s\n", role, cloudpg.SafeDiagnostic(err))
		return 1
	}
	if err := pools.Direct.Ping(ctx); err != nil {
		writef(stderr, "goobers-cloud %s: direct pool: %s\n", role, cloudpg.SafeDiagnostic(err))
		return 1
	}
	compatibilityErr := checkCloudSchema(ctx, pools.Transaction, version.Get().Version)
	if compatibilityErr != nil && !cloudmigrate.IsIncompatible(compatibilityErr) {
		writef(stderr, "goobers-cloud %s: check database schema compatibility: %s\n", role, cloudpg.SafeDiagnostic(compatibilityErr))
		return 1
	}
	if err := serve(ctx, pools, compatibilityErr); err != nil {
		writef(stderr, "goobers-cloud %s: %v\n", role, err)
		return 1
	}
	return 0
}

func databaseURL(argument, filename, environment string) (string, error) {
	if strings.TrimSpace(argument) != "" && strings.TrimSpace(filename) != "" {
		return "", errors.New("choose either the URL argument or file, not both")
	}
	if value := strings.TrimSpace(argument); value != "" {
		return value, nil
	}
	if strings.TrimSpace(filename) != "" {
		content, err := os.ReadFile(filename)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", filename, err)
		}
		if value := strings.TrimSpace(string(content)); value != "" {
			return value, nil
		}
		return "", fmt.Errorf("%s is empty", filename)
	}
	if value := strings.TrimSpace(os.Getenv(environment)); value != "" {
		return value, nil
	}
	return "", fmt.Errorf("set the corresponding flag, file, or %s", environment)
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
