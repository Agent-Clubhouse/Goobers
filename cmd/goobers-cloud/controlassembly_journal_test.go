package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/httpapi"
)

func TestJournalConstructorFailsClosedWithoutItsDependencies(t *testing.T) {
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner := func() *lease.Handle { return nil }
	manager := &lease.Manager{}
	database := databaseServices{Pools: &cloudpg.Pools{}, InstanceID: "instance-a", Blobs: blobs}
	for name, tc := range map[string]struct {
		db      databaseServices
		manager *lease.Manager
		owner   func() *lease.Handle
		want    string
	}{
		"no controller database": {db: databaseServices{InstanceID: "instance-a", Blobs: blobs}, manager: manager, owner: owner, want: "controller database"},
		"no instance":            {db: databaseServices{Pools: &cloudpg.Pools{}, Blobs: blobs}, manager: manager, owner: owner, want: "controller database"},
		"no lease manager":       {db: database, owner: owner, want: "controller lease"},
		"no live owner":          {db: database, manager: manager, want: "controller lease"},
		"no object store":        {db: databaseServices{Pools: &cloudpg.Pools{}, InstanceID: "instance-a"}, manager: manager, owner: owner, want: "span_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			var planes controlPlanes
			err := constructJournal(t.Context(), tc.db, controlAuth{}, tc.manager, tc.owner, &planes)
			if err == nil || !strings.Contains(err.Error(), tc.want) || planes.Journal != nil || len(planes.closers) != 0 {
				t.Fatalf("construct = %v (journal %v), want a refusal naming %q", err, planes.Journal != nil, tc.want)
			}
		})
	}
}

func TestJournalIsAControllerSideSeamWithAConstructor(t *testing.T) {
	for _, seam := range controlAssembly {
		if seam.field != "Journal" {
			continue
		}
		if !seam.controller || seam.construct == nil || seam.issue != "#7818" {
			t.Fatalf("journal seam = %+v, want a controller-side constructor tracked by #7818", seam)
		}
		return
	}
	t.Fatal("no Journal seam")
}

func TestFailedConstructionReleasesEarlierPlanes(t *testing.T) {
	var closed []string
	prior := controlAssembly
	controlAssembly = []controlAssemblySeam{
		{field: "Journal", present: func(p controlPlanes) bool { return p.Journal != nil }, construct: func(_ context.Context, _ databaseServices, _ controlAuth, _ *lease.Manager, _ func() *lease.Handle, p *controlPlanes) error {
			p.Journal = struct{ httpapi.JournalService }{}
			p.closers = append(p.closers, func() { closed = append(closed, "journal") })
			return nil
		}},
		{field: "Claims", present: func(p controlPlanes) bool { return p.Claims != nil }, construct: func(context.Context, databaseServices, controlAuth, *lease.Manager, func() *lease.Handle, *controlPlanes) error {
			return errors.New("claims unavailable")
		}},
	}
	t.Cleanup(func() { controlAssembly = prior })
	if _, err := composeControlPlanes(t.Context(), controlDatabases{}, controlAuth{}, nil, nil); err == nil {
		t.Fatal("construction error was swallowed")
	}
	if strings.Join(closed, ",") != "journal" {
		t.Fatalf("closed = %v, want the earlier journal released", closed)
	}
}
