package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
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
	planes := claimProviderPlanes()
	database.Acceptances = &acceptance.Store{}
	if err := constructClaims(t.Context(), database, controlAuth{}, manager, owner, &planes); err != nil || planes.Claims == nil {
		t.Fatalf("construct = %v, claims composed %v", err, planes.Claims != nil)
	}
	// No owner means the live lease refuses this before a DB write. A missing
	// ReleaseMarker would instead return claims_provider_cleanup_unavailable.
	_, err := planes.Claims.Release(t.Context(), httpapi.ClaimRequest{ProviderCleanup: true, Gaggle: "g", Provider: "github", ItemID: "7", RunID: "run-1", ClaimedAt: time.Now()})
	var intervention *httpapi.InterventionError
	if !errors.As(err, &intervention) || intervention.Code != "recovering" {
		t.Fatalf("composed cleanup = %v, want live-lease refusal", err)
	}
}

type claimConfiguration struct{ appliedOnly }

func (claimConfiguration) InstanceConfig() *instance.Config { return nil }
func (claimConfiguration) Applied() (cloudconfig.Candidate, bool) {
	return cloudconfig.Candidate{}, false
}

func claimProviderPlanes() controlPlanes {
	return controlPlanes{Configuration: claimConfiguration{}, journalReader: &journalpg.Backend{}, generations: noGenerations{}}
}

func TestClaimsProviderRequiresItsTrustedSources(t *testing.T) {
	for _, missing := range []string{"pins", "configuration", "journal", "generations"} {
		t.Run(missing, func(t *testing.T) {
			p := claimProviderPlanes()
			db := databaseServices{Acceptances: &acceptance.Store{}}
			switch missing {
			case "pins":
				db.Acceptances = nil
			case "configuration":
				p.Configuration = nil
			case "journal":
				p.journalReader = nil
			case "generations":
				p.generations = nil
			}
			if _, err := newControlProvider(db, &p); err == nil {
				t.Fatal("missing dependency accepted")
			}
		})
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

func TestClaimsAssemblyFollowsItsProviderSources(t *testing.T) {
	seen := map[string]bool{}
	for _, seam := range controlAssembly {
		if seam.field == "Claims" && (!seen["Configuration"] || !seen["Journal"]) {
			t.Fatal("claims assembled before its trusted provider sources")
		}
		seen[seam.field] = true
	}
}
