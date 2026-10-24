package main

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
)

func TestClaimsConstructorFailsClosedWithoutItsDependencies(t *testing.T) {
	owner := func() *lease.Handle { return nil }
	manager := &lease.Manager{}
	pools := &cloudpg.Pools{Transaction: &pgxpool.Pool{}, Direct: &pgxpool.Pool{}}
	database := databaseServices{Pools: pools, InstanceID: "instance-a"}
	for name, tc := range map[string]struct {
		db      databaseServices
		manager *lease.Manager
		owner   func() *lease.Handle
		want    string
	}{
		"no controller database": {db: databaseServices{InstanceID: "instance-a"}, manager: manager, owner: owner, want: "controller database"},
		"no instance":            {db: databaseServices{Pools: pools}, manager: manager, owner: owner, want: "controller database"},
		"no lease manager":       {db: database, owner: owner, want: "controller lease"},
		"no live owner":          {db: database, manager: manager, want: "controller lease"},
		"no transaction pool":    {db: databaseServices{Pools: &cloudpg.Pools{}, InstanceID: "instance-a"}, manager: manager, owner: owner, want: "requires a pool"},
	} {
		t.Run(name, func(t *testing.T) {
			var planes controlPlanes
			err := constructClaims(t.Context(), tc.db, controlAuth{}, tc.manager, tc.owner, &planes)
			if err == nil || !strings.Contains(err.Error(), tc.want) || planes.Claims != nil {
				t.Fatalf("construct = %v (claims %v), want a refusal naming %q", err, planes.Claims != nil, tc.want)
			}
		})
	}
	var planes controlPlanes
	if err := constructClaims(t.Context(), database, controlAuth{}, manager, owner, &planes); err != nil || planes.Claims == nil {
		t.Fatalf("construct = %v, claims composed %v", err, planes.Claims != nil)
	}
}

func TestClaimsIsAControllerSideSeamWithAConstructor(t *testing.T) {
	for _, seam := range controlAssembly {
		if seam.field != "Claims" {
			continue
		}
		if !seam.controller || seam.construct == nil || seam.issue != "#7818" {
			t.Fatalf("claims seam = %+v, want a controller-side constructor tracked by #7818", seam)
		}
		return
	}
	t.Fatal("no Claims seam")
}
