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
