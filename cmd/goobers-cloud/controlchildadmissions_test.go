package main

import (
	"bytes"
	"strings"
	"testing"

	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/cloud/childengine"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	sharedauth "github.com/goobers/goobers/internal/podauth"
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

func TestEngineChildAuthorityFailsClosedWithoutItsDependencies(t *testing.T) {
	owner := func() *lease.Handle { return nil }
	key, err := sharedauth.NewSignedKey(bytes.Repeat([]byte{3}, sharedauth.MinSignedKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	admissions := &childengine.Fenced{}
	for name, tc := range map[string]struct {
		planes controlPlanes
		db     databaseServices
		auth   controlAuth
		want   string
	}{
		"no admissions": {planes: controlPlanes{}, want: "engine child admissions"},
		"no temporal":   {planes: controlPlanes{engineChildren: admissions}, auth: controlAuth{GrantKeys: key}, want: "Temporal client"},
		"no grant key":  {planes: controlPlanes{engineChildren: admissions}, db: databaseServices{Temporal: struct{ client.Client }{}}, want: "credential-grant key"},
	} {
		t.Run(name, func(t *testing.T) {
			err := constructEngineChildAuthority(t.Context(), tc.db, tc.auth, &lease.Manager{}, owner, &tc.planes)
			if err == nil || !strings.Contains(err.Error(), tc.want) || tc.planes.EngineChildAuthority != nil {
				t.Fatalf("construct = %v (plane %v), want a refusal naming %q", err, tc.planes.EngineChildAuthority != nil, tc.want)
			}
		})
	}
}

func TestEngineChildAuthorityIsControllerSideAfterAdmissions(t *testing.T) {
	admissions, authority := -1, -1
	for i, seam := range controlAssembly {
		switch seam.field {
		case "EngineChildAdmissions":
			admissions = i
		case "EngineChildAuthority":
			authority = i
			if !seam.controller || seam.construct == nil {
				t.Fatalf("engine child authority seam = %+v, want a controller-side constructor", seam)
			}
		}
	}
	if admissions < 0 || authority <= admissions {
		t.Fatalf("engine child authority (%d) must compose after the admissions (%d)", authority, admissions)
	}
}

func TestChildWorkflowsComposeAfterTheAuthorityAndFailClosedWithoutIt(t *testing.T) {
	planes := controlPlanes{}
	if err := constructChildWorkflows(t.Context(), databaseServices{}, controlAuth{}, &lease.Manager{}, func() *lease.Handle { return nil }, &planes); err == nil || planes.ChildWorkflows != nil {
		t.Fatalf("construct without the authority = %v (plane %v)", err, planes.ChildWorkflows != nil)
	}
	authority, workflows := -1, -1
	for i, seam := range controlAssembly {
		switch seam.field {
		case "EngineChildAuthority":
			authority = i
		case "ChildWorkflows":
			workflows = i
			if !seam.controller || seam.construct == nil {
				t.Fatalf("child workflows seam = %+v, want a controller-side constructor", seam)
			}
		}
	}
	if authority < 0 || workflows <= authority {
		t.Fatalf("child workflows (%d) must compose after the engine child authority (%d)", workflows, authority)
	}
}
