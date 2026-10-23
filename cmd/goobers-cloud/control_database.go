package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	cloudmigrate "github.com/goobers/goobers/internal/cloud/migrate"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/cloud/pgroles"
	"github.com/goobers/goobers/internal/version"
)

// Each role has its own credentials. No API constructor receives a controller
// pool, and no request path can borrow the lease manager's connection.
type controlDatabases struct {
	API, Controller databaseServices
	InstanceID      string
}

type controlDatabaseURL struct{ url, file, direct, directFile string }

func (o controlDatabaseURL) config(role string, total, direct int) (cloudpg.Config, error) {
	prefix := "GOOBERS_" + strings.ToUpper(role) + "_DATABASE_"
	url, err := cloudpg.DatabaseURL(o.url, o.file, prefix+"URL")
	if err != nil {
		return cloudpg.Config{}, fmt.Errorf("%s database URL: %w", role, err)
	}
	directURL, err := cloudpg.DatabaseURL(o.direct, o.directFile, prefix+"DIRECT_URL")
	if err != nil {
		return cloudpg.Config{}, fmt.Errorf("%s direct database URL: %w", role, err)
	}
	return cloudpg.Config{TransactionURL: url, DirectURL: directURL, MaxConnections: int32(total), DirectMaxConnections: int32(direct)}, nil
}

type controlDatabaseOptions struct {
	apiURL, apiFile, apiDirect, apiDirectFile                             string
	controllerURL, controllerFile, controllerDirect, controllerDirectFile string
	instanceID, auth                                                      string
	max, direct                                                           int
}

func controlInstanceDatabase(instance string) (string, error) {
	if instance == "" || strings.EqualFold(instance, "default") || strings.EqualFold(instance, "change-me") {
		return "", errors.New("control requires a non-placeholder --instance-id or GOOBERS_INSTANCE_ID")
	}
	return pgroles.InstanceDatabase(instance)
}

var runControlWithDatabases = serveControl

func runControlConfigured(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		write(stdout, controlHelp)
		return 0
	}
	f := flag.NewFlagSet("control", flag.ContinueOnError)
	f.SetOutput(stderr)
	f.Usage = func() { write(stderr, controlHelp) }
	var o controlOptions
	var db controlDatabaseOptions
	o.flags(f)
	db.flags(f)
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if f.NArg() != 0 {
		f.Usage()
		return 2
	}
	if o.checkAssembly {
		return checkControlAssembly(stdout)
	}
	if err := validateControlOptions(o); err != nil {
		writef(stderr, "goobers-cloud control: %v\n", err)
		return 2
	}
	name, err := controlInstanceDatabase(db.instanceID)
	if err != nil {
		writef(stderr, "goobers-cloud control: %v\n", err)
		return 2
	}
	apiConfig, err := (controlDatabaseURL{db.apiURL, db.apiFile, db.apiDirect, db.apiDirectFile}).config("api", db.max-db.max/2, db.direct)
	if err != nil {
		writef(stderr, "goobers-cloud control: %v\n", err)
		return 2
	}
	controllerConfig, err := (controlDatabaseURL{db.controllerURL, db.controllerFile, db.controllerDirect, db.controllerDirectFile}).config("controller", db.max/2, db.direct)
	if err != nil {
		writef(stderr, "goobers-cloud control: %v\n", err)
		return 2
	}
	for _, config := range []*cloudpg.Config{&apiConfig, &controllerConfig} {
		if code := configureDatabaseAuth("control", db.auth, config, stderr); code != 0 {
			return code
		}
		if err := validateControlDatabaseConfig(*config, name); err != nil {
			writef(stderr, "goobers-cloud control: %v\n", err)
			return 2
		}
	}
	ctx, stop := signalContext()
	defer stop()
	api, err := openControlDatabase(ctx, apiConfig, "goobers_api", name)
	if err != nil {
		writef(stderr, "goobers-cloud control: API database: %s\n", cloudpg.SafeDiagnostic(err))
		return 1
	}
	defer api.Close()
	controller, err := openControlDatabase(ctx, controllerConfig, "goobers_controller", name)
	if err != nil {
		writef(stderr, "goobers-cloud control: controller database: %s\n", cloudpg.SafeDiagnostic(err))
		return 1
	}
	defer controller.Close()
	databases, err := newControlDatabases(ctx, api, controller, db.instanceID)
	if err == nil {
		err = runControlWithDatabases(ctx, databases, o, log.New(stderr, "goobers-cloud control: ", log.LstdFlags))
	}
	if err != nil {
		writef(stderr, "goobers-cloud control: %s\n", cloudpg.SafeDiagnostic(err))
		return 1
	}
	return 0
}

func validateControlDatabaseConfig(config cloudpg.Config, name string) error {
	transaction, direct, err := cloudpg.PoolConfigs(config)
	if err != nil {
		return err
	}
	for _, pool := range []*pgxpool.Config{transaction, direct} {
		if pool.ConnConfig.Database != name {
			return fmt.Errorf("both role URLs must target instance database %q", name)
		}
	}
	return nil
}

func openControlDatabase(ctx context.Context, config cloudpg.Config, role, name string) (*cloudpg.Pools, error) {
	pools, err := newCloudPools(ctx, config)
	if err != nil {
		return nil, err
	}
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for _, pool := range []*pgxpool.Pool{pools.Transaction, pools.Direct} {
		var user, session, database string
		if err := pool.QueryRow(startup, "SELECT current_user, session_user, current_database()").Scan(&user, &session, &database); err != nil {
			pools.Close()
			return nil, err
		}
		if user != role || session != role || database != name {
			pools.Close()
			return nil, fmt.Errorf("database connection must authenticate as %s in %s", role, name)
		}
	}
	return pools, nil
}

func newControlDatabases(ctx context.Context, api, controller *cloudpg.Pools, instance string) (controlDatabases, error) {
	result := controlDatabases{InstanceID: instance}
	for _, target := range []struct {
		pools    *cloudpg.Pools
		services *databaseServices
	}{{api, &result.API}, {controller, &result.Controller}} {
		compatibility := checkCloudSchema(ctx, target.pools.Transaction, version.Get().Version)
		if compatibility != nil && !cloudmigrate.IsIncompatible(compatibility) {
			return result, compatibility
		}
		store, err := newAcceptanceStore(target.pools.Transaction, instance)
		if err != nil {
			return result, err
		}
		*target.services = newDatabaseServices(target.pools, store, compatibility)
	}
	return result, nil
}
