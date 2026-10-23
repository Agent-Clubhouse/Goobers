package main

import (
	"strings"
	"testing"

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
)

// appliedOnly is a Configuration that also reports its applied generation.
type appliedOnly struct{ control.Configuration }

func (appliedOnly) Applied() (cloudconfig.Candidate, bool) { return cloudconfig.Candidate{}, false }

func TestEngineChildAdmissionsFailClosedWithoutTheirDependencies(t *testing.T) {
	owner := func() *lease.Handle { return nil }
	for name, tc := range map[string]struct {
		planes controlPlanes
		db     databaseServices
		want   string
	}{
		"no configuration":       {planes: controlPlanes{}, want: "applied configuration"},
		"guard without applied":  {planes: controlPlanes{Configuration: struct{ control.Configuration }{}}, want: "applied configuration"},
		"no controller database": {planes: controlPlanes{Configuration: appliedOnly{}}, want: "controller database"},
		"no object store":        {planes: controlPlanes{Configuration: appliedOnly{}}, db: databaseServices{InstanceID: "instance-a", Pools: &cloudpg.Pools{}}, want: "--object-store-url"},
	} {
		t.Run(name, func(t *testing.T) {
			err := constructEngineChildAdmissions(t.Context(), tc.db, controlAuth{}, &lease.Manager{}, owner, &tc.planes)
			if err == nil || !strings.Contains(err.Error(), tc.want) || tc.planes.EngineChildAdmissions != nil {
				t.Fatalf("construct = %v (plane %v), want a refusal naming %q", err, tc.planes.EngineChildAdmissions != nil, tc.want)
			}
		})
	}
}

func TestEngineChildAdmissionsAreControllerSideAfterConfiguration(t *testing.T) {
	configuration, children := -1, -1
	for i, seam := range controlAssembly {
		switch seam.field {
		case "Configuration":
			configuration = i
		case "EngineChildAdmissions":
			children = i
			if !seam.controller || seam.construct == nil {
				t.Fatalf("engine child admissions seam = %+v, want a controller-side constructor", seam)
			}
		}
	}
	if configuration < 0 || children <= configuration {
		t.Fatalf("engine child admissions (%d) must compose after Configuration (%d)", children, configuration)
	}
}
