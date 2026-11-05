package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/control"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/instance"
)

type resolveConfiguration struct{ control.Configuration }

func (resolveConfiguration) InstanceConfig() *instance.Config { return nil }

type resolveGenerationSource func(context.Context, string) (childworkflow.Generation, error)

func (f resolveGenerationSource) Acquire(ctx context.Context, digest string) (childworkflow.Generation, error) {
	return f(ctx, digest)
}

func TestResolveConstructorFailsClosedWithoutItsDependencies(t *testing.T) {
	source := resolveGenerationSource(func(context.Context, string) (childworkflow.Generation, error) {
		t.Fatal("composition acquired a generation before the controller epoch")
		return childworkflow.Generation{}, nil
	})
	db := databaseServices{InstanceID: "instance-a", Pools: &cloudpg.Pools{Transaction: &pgxpool.Pool{}}}
	complete := controlPlanes{Configuration: resolveConfiguration{}, generations: source}
	for name, tc := range map[string]struct {
		db     databaseServices
		planes controlPlanes
		want   string
	}{
		"no applied policy":   {db, controlPlanes{generations: source}, "applied configuration"},
		"no generations":      {db, controlPlanes{Configuration: resolveConfiguration{}}, "generation source"},
		"no database":         {databaseServices{InstanceID: "instance-a"}, complete, "controller database"},
		"no transaction pool": {databaseServices{InstanceID: "instance-a", Pools: &cloudpg.Pools{}}, complete, "controller database"},
		"no instance":         {databaseServices{Pools: db.Pools}, complete, "instance"},
	} {
		t.Run(name, func(t *testing.T) {
			err := constructResolve(t.Context(), tc.db, controlAuth{}, nil, nil, &tc.planes)
			if err == nil || !strings.Contains(err.Error(), tc.want) || tc.planes.Resolve != nil {
				t.Fatalf("construct = %v, resolve composed %v; want %q refusal", err, tc.planes.Resolve != nil, tc.want)
			}
		})
	}
	if err := constructResolve(t.Context(), db, controlAuth{}, nil, nil, &complete); err != nil || complete.Resolve == nil {
		t.Fatalf("compose before the first reload = %v, resolve composed %v", err, complete.Resolve != nil)
	}
}

func TestResolveIsAControllerSideSeamAfterConfiguration(t *testing.T) {
	configured := false
	for _, seam := range controlAssembly {
		if seam.field == "Configuration" {
			configured = true
		}
		if seam.field != "Resolve" {
			continue
		}
		if !configured || !seam.controller || seam.construct == nil || seam.requires == nil {
			t.Fatalf("resolve seam = %+v, want a controller constructor after Configuration", seam)
		}
		if missing := seam.requires(controlOptions{}); missing != "--config-dir" {
			t.Fatalf("missing option = %q, want --config-dir", missing)
		}
		return
	}
	t.Fatal("no Resolve seam")
}
