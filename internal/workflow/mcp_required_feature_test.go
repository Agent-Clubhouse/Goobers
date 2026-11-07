package workflow

import (
	"reflect"
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

func TestCompileMCPRequirednessByDSLVersion(t *testing.T) {
	required, optional := true, false
	for _, tc := range []struct {
		name         string
		required     *bool
		wantRequired bool
	}{
		{name: "optional", required: &optional},
		{name: "required", required: &required, wantRequired: true},
		{name: "omitted", wantRequired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			goobers := map[string]apiv1.GooberSpec{
				"coder": {
					Capabilities: []string{"agent:model"},
					MCPServers: []apiv1.MCPServer{{
						Name:     "context",
						URL:      "https://example.invalid/mcp",
						Required: tc.required,
					}},
				},
			}
			spec := linearSpec()
			spec.Tasks[0].Capabilities = []string{"agent:model"}
			source := goobers["coder"]
			wantSource := source.DeepCopy()
			for _, version := range []string{supportmatrix.V31DSLVersion, v20.DSLVersion, v30.DSLVersion, supportmatrix.V31DSLVersion} {
				t.Run(version, func(t *testing.T) {
					machine, err := compileAcknowledged(
						Definition{Name: "mcp-requiredness", Version: 1, DSLVersion: version, Spec: spec},
						WithGoobers(goobers),
					)
					if !reflect.DeepEqual(goobers["coder"], *wantSource) {
						t.Fatal("compiler mutated the shared authoring declaration")
					}
					if version != supportmatrix.V31DSLVersion && tc.required != nil {
						if err == nil || !strings.Contains(err.Error(), `requires dslVersion "3.1"`) {
							t.Fatalf("DSL %s Compile error = %v, want explicit requiredness refusal", version, err)
						}
						return
					}
					if err != nil {
						t.Fatalf("Compile acknowledged DSL %s workflow: %v", version, err)
					}
					goober, ok := machine.Goober("coder")
					if !ok || len(goober.MCPServers) != 1 {
						t.Fatalf("compiled goober = %+v, found=%v", goober, ok)
					}
					got := goober.MCPServers[0].Required
					if version != supportmatrix.V31DSLVersion {
						if got != nil {
							t.Fatalf("DSL %s required = %t, want legacy nil", version, *got)
						}
						return
					}
					if got == nil || *got != tc.wantRequired {
						t.Fatalf("compiled required = %v, want %t", got, tc.wantRequired)
					}
					*got = !tc.wantRequired
					if !reflect.DeepEqual(goobers["coder"], *wantSource) {
						t.Fatal("compiled requiredness aliases the shared authoring declaration")
					}
				})
			}
		})
	}
}

func TestCompileDSL31MCPRequirednessRequiresPreview(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required bool
	}{
		{name: "optional"},
		{name: "required", required: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			goobers := map[string]apiv1.GooberSpec{
				"coder": {
					MCPServers: []apiv1.MCPServer{{
						Name:     "context",
						Command:  "context-server",
						Required: &tc.required,
					}},
				},
			}
			_, err := Compile(
				Definition{Name: "mcp-preview", Version: 1, DSLVersion: supportmatrix.V31DSLVersion, Spec: linearSpec()},
				WithGoobers(goobers),
			)
			if err == nil || !strings.Contains(err.Error(), `DSL feature "goober.spec.mcpServers.required" is preview`) {
				t.Fatalf("Compile error = %v, want requiredness preview refusal", err)
			}
		})
	}
}

func TestCompileDSL31MCPRequirednessPreservesCredentials(t *testing.T) {
	required := false
	source := apiv1.GooberSpec{
		Capabilities: []string{"agent:model", "contents:read"},
		MCPServers: []apiv1.MCPServer{{
			Name:     "context",
			URL:      "https://example.invalid/mcp",
			Required: &required,
			CredentialRefs: []apiv1.MCPCredentialRef{
				{Capability: "contents:read", Header: "Authorization"},
				{Kind: apiv1.MCPCredentialKindBYO, Ref: "vendor-api", Header: "X-API-Key"},
			},
		}},
	}
	goobers := map[string]apiv1.GooberSpec{"coder": source}
	wantSource := source.DeepCopy()
	for _, tc := range []struct {
		name         string
		capabilities []string
		wantError    string
	}{
		{
			name:         "missing task credential capability",
			capabilities: []string{"agent:model"},
			wantError:    `task "implement" must declare MCP credential capability "contents:read" required by goober "coder"`,
		},
		{
			name:         "declared task credential capability",
			capabilities: []string{"agent:model", "contents:read"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := linearSpec()
			spec.Tasks[0].Capabilities = tc.capabilities
			machine, err := compileAcknowledged(
				Definition{Name: "mcp-credentials", Version: 1, DSLVersion: supportmatrix.V31DSLVersion, Spec: spec},
				WithGoobers(goobers),
			)
			if !reflect.DeepEqual(goobers["coder"], *wantSource) {
				t.Fatal("compiler mutated the shared authoring declaration")
			}
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("Compile error = %v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, ok := machine.Goober("coder")
			if !ok || !reflect.DeepEqual(got, *wantSource) {
				t.Fatalf("compiled goober = %+v, found=%v, want unmodified requiredness and credential references", got, ok)
			}
		})
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

func TestMCPRequiredDefaultAppliesAtDSL31AndLater(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{version: "3.0"},
		{version: "3.1", want: true},
		{version: "3.2", want: true},
		{version: ""},
		{version: "future"},
	} {
		if got := mcpRequiredDefaultApplies(tc.version); got != tc.want {
			t.Errorf("mcpRequiredDefaultApplies(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}
