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

	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/signals"
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

Environment:
  GOOBERS_DATABASE_URL         PgBouncer transaction-pool URL
  GOOBERS_DATABASE_DIRECT_URL  Direct PostgreSQL URL
`
	controllerHelp = `Usage: goobers-cloud controller [DATABASE OPTIONS]

Run cloud control-plane reconciliation.

Database options are the same as for the api role.
`
	migrateHelp = `Usage: goobers-cloud migrate

Apply cloud data-store migrations. The P1 skeleton reserves this role while
the migration runner is wired into the cloud composition root.
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

var (
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
	switch *authMode {
	case "password":
	case "entra":
		credential, err := newDefaultAzureCredential()
		if err != nil {
			writef(stderr, "goobers-cloud %s: Entra credential initialization failed\n", role)
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
	if err := serve(ctx, pools); err != nil {
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

func runWorkerRole(args []string, stdout, stderr io.Writer) int {
	return runWorker(args, stdout, stderr)
}
