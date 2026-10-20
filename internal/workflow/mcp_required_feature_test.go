package workflow

import (
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
	v20 "github.com/goobers/goobers/internal/workflow/v_2_0"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
)

func TestMCPServerRequiredIsDSL31Feature(t *testing.T) {
	required := false
	spec := apiv1.GooberSpec{
		MCPServers: []apiv1.MCPServer{{
			Name:     "context",
			Required: &required,
			Command:  "context-server",
		}},
	}

	for _, version := range []string{v20.DSLVersion, v30.DSLVersion} {
		t.Run(version, func(t *testing.T) {
			_, err := FeaturesForGoober(Definition{DSLVersion: version}, spec)
			if err == nil || !strings.Contains(err.Error(), `requires dslVersion "3.1"`) {
				t.Fatalf("FeaturesForGoober error = %v, want DSL 3.1 refusal", err)
			}
		})
	}

	features, err := FeaturesForGoober(Definition{DSLVersion: supportmatrix.V31DSLVersion}, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(features, func(feature Feature) bool {
		return feature.ID == featureGooberMCPServersRequired
	}) {
		t.Fatalf("features = %+v, want %q", features, featureGooberMCPServersRequired)
	}
}

func TestCompileMaterializesMCPRequiredDefaultForDSL31Only(t *testing.T) {
	goobers := map[string]apiv1.GooberSpec{
		"coder": {
			MCPServers: []apiv1.MCPServer{{Name: "context", Command: "context-server"}},
		},
	}
	for _, tc := range []struct {
		version      string
		wantRequired *bool
	}{
		{version: v30.DSLVersion},
		{version: supportmatrix.V31DSLVersion, wantRequired: func() *bool { value := true; return &value }()},
	} {
		t.Run(tc.version, func(t *testing.T) {
			machine, err := compileAcknowledged(
				Definition{Name: "mcp-default", Version: 1, DSLVersion: tc.version, Spec: linearSpec()},
				WithGoobers(goobers),
			)
			if err != nil {
				t.Fatal(err)
			}
			spec, ok := machine.Goober("coder")
			if !ok || len(spec.MCPServers) != 1 {
				t.Fatalf("compiled goober = %+v, found=%v", spec, ok)
			}
			got := spec.MCPServers[0].Required
			if tc.wantRequired == nil {
				if got != nil {
					t.Fatalf("DSL %s required = %v, want legacy nil", tc.version, *got)
				}
			} else if got == nil || *got != *tc.wantRequired {
				t.Fatalf("DSL %s required = %v, want true", tc.version, got)
			}
			if goobers["coder"].MCPServers[0].Required != nil {
				t.Fatal("compiler mutated the shared authoring declaration")
			}
		})
	}
}
