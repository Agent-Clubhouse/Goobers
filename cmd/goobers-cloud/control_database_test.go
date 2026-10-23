package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
)

func TestControlRequiresNonPlaceholderInstanceBeforeDatabase(t *testing.T) {
	for _, value := range []string{"", "default", "change-me", "DEFAULT", "../bad"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("GOOBERS_INSTANCE_ID", value)
			code, _, stderr := runForTest("control")
			if code != 2 || !strings.Contains(stderr, "instance") {
				t.Fatalf("code=%d error=%s", code, stderr)
			}
		})
	}
}

func TestControlDatabaseFlagsMatchReferenceAndOverrideEnvironment(t *testing.T) {
	t.Setenv("GOOBERS_INSTANCE_ID", "environment")
	var o controlDatabaseOptions
	f := flag.NewFlagSet("test", flag.ContinueOnError)
	o.flags(f)
	args := []string{"--instance-id", "team-blue", "--database-auth", "password"}
	urls := map[string]string{
		"api-database-url-file":               "postgres://goobers_api:api-secret@localhost/goobers_team_blue",
		"api-database-direct-url-file":        "postgres://goobers_api:api-secret@localhost/goobers_team_blue",
		"controller-database-url-file":        "postgres://goobers_controller:controller-secret@localhost/goobers_team_blue",
		"controller-database-direct-url-file": "postgres://goobers_controller:controller-secret@localhost/goobers_team_blue",
	}
	for name, url := range urls {
		path := filepath.Join(t.TempDir(), "url")
		if err := os.WriteFile(path, []byte(url+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--"+name, path)
	}
	if err := f.Parse(args); err != nil {
		t.Fatal(err)
	}
	if o.instanceID != "team-blue" {
		t.Fatal("flag did not override environment")
	}
	database, err := controlInstanceDatabase(o.instanceID)
	if err != nil || database != "goobers_team_blue" {
		t.Fatalf("database=%s err=%v", database, err)
	}
	for role, options := range map[string]controlDatabaseURL{"api": {o.apiURL, o.apiFile, o.apiDirect, o.apiDirectFile}, "controller": {o.controllerURL, o.controllerFile, o.controllerDirect, o.controllerDirectFile}} {
		cfg, err := options.config(role, 8, 2)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateControlDatabaseConfig(cfg, database); err != nil {
			t.Fatal(err)
		}
		transaction, direct, err := cloudpg.PoolConfigs(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if transaction.ConnConfig.User != "goobers_"+role || direct.ConnConfig.User != "goobers_"+role {
			t.Fatalf("%s credentials crossed", role)
		}
	}
}

func TestControlRejectsCrossInstanceURLsWithoutConnecting(t *testing.T) {
	t.Setenv("GOOBERS_INSTANCE_ID", "blue")
	t.Setenv("GOOBERS_API_DATABASE_URL", "postgres://goobers_api:secret@localhost/goobers_wrong")
	t.Setenv("GOOBERS_API_DATABASE_DIRECT_URL", "postgres://goobers_api:secret@localhost/goobers_blue")
	t.Setenv("GOOBERS_CONTROLLER_DATABASE_URL", "postgres://goobers_controller:secret@localhost/goobers_blue")
	t.Setenv("GOOBERS_CONTROLLER_DATABASE_DIRECT_URL", "postgres://goobers_controller:secret@localhost/goobers_blue")
	prior := newCloudPools
	newCloudPools = func(context.Context, cloudpg.Config) (*cloudpg.Pools, error) {
		t.Fatal("opened pool before instance validation")
		return nil, errors.New("unreachable")
	}
	t.Cleanup(func() { newCloudPools = prior })
	code, _, stderr := runForTest("control")
	if code != 2 || !strings.Contains(stderr, "goobers_blue") || strings.Contains(stderr, "secret") {
		t.Fatalf("code=%d error=%s", code, stderr)
	}
}

func TestControlAssemblyRoutesEachConstructorToItsRole(t *testing.T) {
	api, controller := &pgxpool.Pool{}, &pgxpool.Pool{}
	db := controlDatabases{API: databaseServices{Pools: &cloudpg.Pools{Transaction: api, Direct: api}}, Controller: databaseServices{Pools: &cloudpg.Pools{Transaction: controller, Direct: controller}}}
	prior := controlAssembly
	controlAssembly = append([]controlAssemblySeam(nil), prior...)
	t.Cleanup(func() { controlAssembly = prior })
	controllerFields := map[string]bool{"Configuration": true, "Resolve": true, "Epoch": true}
	for i := range controlAssembly {
		field := controlAssembly[i].field
		want := api
		if controllerFields[field] {
			want = controller
		}
		controlAssembly[i].construct = func(_ context.Context, services databaseServices, _ controlAuth, _ func() *lease.Handle, p *controlPlanes) error {
			if services.Pools.Transaction != want || services.Pools.Direct != want {
				t.Fatalf("%s received wrong role's pool", field)
			}
			value := reflect.ValueOf(populatedControlPlanes()).FieldByName(field)
			reflect.ValueOf(p).Elem().FieldByName(field).Set(value)
			return nil
		}
	}
	if _, err := composeControlPlanes(t.Context(), db, controlAuth{}, func() *lease.Handle { return nil }); err != nil {
		t.Fatal(err)
	}
}
