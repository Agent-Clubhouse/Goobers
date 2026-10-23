package main

import (
	"errors"
	"flag"
	"io"

	"github.com/goobers/goobers/internal/cloud/cutover"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
)

const bootstrapHelp = `Usage: goobers-cloud bootstrap --instance-id ID --backup-ref REF [OPTIONS]

Prepare a new cloud install for cutover: create the database and roles, apply
schema migrations, create the first lease row and object-store bucket, and
leave admissions paused. Every step is idempotent.

Bootstrap refuses before changing anything while any step depends on a backend
that has not shipped; the refusal names each step as "not available until #N".

Options:
  --instance-id ID                 Instance identifier (required)
  --backup-ref REF                 Restorable backup identifier or PITR mark (required)
  --lease-name NAME                Controller lease name (default controller)
  --database-direct-url URL        Direct URL (prefer env/file for credentials)
  --database-direct-url-file PATH  Read direct URL from a mounted Secret
  --database-auth MODE             password or entra (default password)

Environment:
  GOOBERS_DATABASE_DIRECT_URL  Direct PostgreSQL URL
`

var bootstrapSteps = cutover.PGSteps

func runBootstrapRole(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		write(stdout, bootstrapHelp)
		return 0
	}
	flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	instanceID := flags.String("instance-id", "", "instance identifier")
	backupRef := flags.String("backup-ref", environmentOr("GOOBERS_MIGRATION_BACKUP_REF", ""), "restorable backup identifier or PITR mark")
	leaseName := flags.String("lease-name", cutover.ControllerLeaseName, "controller lease name")
	directURLArg := flags.String("database-direct-url", "", "direct PostgreSQL URL")
	directURLFile := flags.String("database-direct-url-file", "", "file containing the direct PostgreSQL URL")
	authMode := flags.String("database-auth", environmentOr("GOOBERS_DATABASE_AUTH", "password"), "database authentication mode: password or entra")
	flags.Usage = func() { write(stderr, bootstrapHelp) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *instanceID == "" || *backupRef == "" {
		flags.Usage()
		return 2
	}
	// Refuse before connecting: an unavailable step needs no database to report.
	if err := cutover.Preflight(bootstrapSteps(nil, *instanceID, *leaseName, *backupRef, "")); err != nil {
		writef(stderr, "goobers-cloud bootstrap: %v\n", err)
		return 1
	}
	directURL, err := cloudpg.DatabaseURL(*directURLArg, *directURLFile, "GOOBERS_DATABASE_DIRECT_URL")
	if err != nil {
		writef(stderr, "goobers-cloud bootstrap: direct database URL: %v\n", err)
		return 2
	}
	config := cloudpg.Config{TransactionURL: directURL, DirectURL: directURL}
	if code := configureDatabaseAuth("bootstrap", *authMode, &config, stderr); code != 0 {
		return code
	}
	ctx, stop := signalContext()
	defer stop()
	pools, err := newCloudPools(ctx, config)
	if err != nil {
		writef(stderr, "goobers-cloud bootstrap: %s\n", cloudpg.SafeDiagnostic(err))
		return 1
	}
	defer pools.Close()
	results, err := cutover.Bootstrap(ctx, bootstrapSteps(pools.Direct, *instanceID, *leaseName, *backupRef, defaultMigrationHolder()))
	for _, result := range results {
		state := "unchanged"
		if result.Changed {
			state = "done"
		}
		writef(stdout, "%s: %s\n", result.Name, state)
	}
	if err != nil {
		writef(stderr, "goobers-cloud bootstrap: %s\n", cloudpg.SafeDiagnostic(err))
		return 1
	}
	return 0
}
