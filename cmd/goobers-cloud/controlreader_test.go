package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
)

func TestControlReaderFailsClosedWithoutItsSources(t *testing.T) {
	owner := func() *lease.Handle { return nil }
	for name, tc := range map[string]struct {
		planes controlPlanes
		db     databaseServices
		want   string
	}{
		"no configuration":      {want: "applied configuration"},
		"guard without applied": {planes: controlPlanes{Configuration: struct{ control.Configuration }{}}, want: "applied configuration"},
		"no journal plane":      {planes: controlPlanes{Configuration: appliedOnly{}}, db: databaseServices{Pools: &cloudpg.Pools{}}, want: "journal plane"},
	} {
		t.Run(name, func(t *testing.T) {
			err := constructReader(t.Context(), tc.db, controlAuth{}, nil, owner, &tc.planes)
			if err == nil || !strings.Contains(err.Error(), tc.want) || tc.planes.Reader != nil {
				t.Fatalf("construct = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

func TestControlReaderComposesAfterConfigurationAndJournal(t *testing.T) {
	position := map[string]int{}
	for i, seam := range controlAssembly {
		position[seam.field] = i
		if seam.field == "Reader" && (!seam.controller || seam.construct == nil) {
			t.Fatalf("reader seam = %+v, want a controller-side constructor", seam)
		}
	}
	if position["Reader"] <= position["Configuration"] || position["Reader"] <= position["Journal"] {
		t.Fatalf("reader at %d must follow Configuration (%d) and Journal (%d)", position["Reader"], position["Configuration"], position["Journal"])
	}
}
