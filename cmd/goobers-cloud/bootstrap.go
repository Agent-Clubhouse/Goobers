package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/cutover"
	cloudmigrate "github.com/goobers/goobers/internal/cloud/migrate"
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
  --admin-url URL                  Server administrator URL: creates roles and databases
  --admin-url-file PATH            Read the administrator URL from a mounted Secret
  --entra-principal ROLE=OBJECTID  Create ROLE as an Entra principal (repeatable)
  --database-direct-url URL        Instance owner URL: runs migrations (prefer env/file)
  --database-direct-url-file PATH  Read the owner URL from a mounted Secret
  --database-auth MODE             password or entra, for the owner (default password)

The administrator and owner are separate identities: the administrator never
runs migrations and the owner never creates roles.

Environment:
  GOOBERS_DATABASE_ADMIN_URL   Administrator PostgreSQL URL
  GOOBERS_DATABASE_DIRECT_URL  Owner PostgreSQL URL
`

var bootstrapSteps = cutover.PGSteps

type principalList map[string]string

func (p principalList) String() string { return "" }
func (p principalList) Set(value string) error {
	role, id, ok := strings.Cut(value, "=")
	if !ok || role == "" || id == "" {
		return errors.New("want ROLE=OBJECTID")
	}
	p[role] = id
	return nil
}

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
	adminURLArg := flags.String("admin-url", "", "server administrator PostgreSQL URL")
	adminURLFile := flags.String("admin-url-file", "", "file containing the administrator PostgreSQL URL")
	entra := principalList{}
	flags.Var(entra, "entra-principal", "Entra principal for a role, ROLE=OBJECTID (repeatable)")
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
	static, _ := bootstrapSteps(cutover.Connect{}, *instanceID, *leaseName, *backupRef, "", entra)
	// Refuse before connecting: an unavailable step needs no database to report.
	if err := cutover.Preflight(static); err != nil {
		writef(stderr, "goobers-cloud bootstrap: %v\n", err)
		return 1
	}
	adminURL, err := cloudpg.DatabaseURL(*adminURLArg, *adminURLFile, "GOOBERS_DATABASE_ADMIN_URL")
	if err != nil {
		writef(stderr, "goobers-cloud bootstrap: administrator database URL: %v\n", err)
		return 2
	}
	directURL, err := cloudpg.DatabaseURL(*directURLArg, *directURLFile, "GOOBERS_DATABASE_DIRECT_URL")
	if err != nil {
		writef(stderr, "goobers-cloud bootstrap: owner database URL: %v\n", err)
		return 2
	}
	config := cloudpg.Config{TransactionURL: directURL, DirectURL: directURL}
	if code := configureDatabaseAuth("bootstrap", *authMode, &config, stderr); code != 0 {
		return code
	}
	ctx, stop := signalContext()
	defer stop()
	var opened *cloudpg.Pools
	defer func() {
		if opened != nil {
			opened.Close()
		}
	}()
	connect := cutover.Connect{
		Report: cloudmigrate.WarningTo(stderr, "bootstrap"),
		Admin:  func(ctx context.Context) (*pgx.Conn, error) { return pgx.Connect(ctx, adminURL) },
		Owner: func(ctx context.Context) (*pgxpool.Pool, error) {
			pools, err := newCloudPools(ctx, config)
			if err != nil {
				return nil, err
			}
			opened = pools
			return pools.Direct, nil
		},
	}
	steps, release := bootstrapSteps(connect, *instanceID, *leaseName, *backupRef, defaultMigrationHolder(), entra)
	defer release()
	results, err := cutover.Bootstrap(ctx, steps)
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
