package workflow

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	v20 "github.com/goobers/goobers/internal/workflow/v_2_0"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
)

func TestBYOMCPCredentialDoesNotBecomeTaskCapability(t *testing.T) {
	goobers := map[string]apiv1.GooberSpec{
		"coder": {
			MCPServers: []apiv1.MCPServer{{
				Name: "vendor",
				URL:  "https://mcp.example.test",
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Kind:   apiv1.MCPCredentialKindBYO,
					Ref:    "vendor-api",
					Header: "Authorization",
				}},
			}},
		},
	}
	for _, version := range []string{v20.DSLVersion, v30.DSLVersion} {
		t.Run(version, func(t *testing.T) {
			def := Definition{Name: "mcp-byo", Version: 1, DSLVersion: version, Spec: linearSpec()}
			if _, err := compileAcknowledged(def, WithGoobers(goobers)); err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if problems := CheckWorkflowAdmission(def, goobers); len(problems) != 0 {
				t.Fatalf("CheckWorkflowAdmission problems = %q", problems)
			}
		})
	}
}

func TestCapabilityAdmissionKeepsFirstPartyMCPCredentials(t *testing.T) {
	goobers := map[string]apiv1.GooberSpec{
		"coder": {
			Capabilities: []string{"contents:read"},
			MCPServers: []apiv1.MCPServer{{
				Name: "vendor",
				URL:  "https://mcp.example.test",
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Capability: "contents:read",
					Header:     "Authorization",
				}},
			}},
		},
	}
	def := Definition{Name: "mcp-first-party", Version: 1, Spec: linearSpec()}
	if _, err := compileAcknowledged(def, WithGoobers(goobers)); err == nil {
		t.Fatal("Compile: want missing task capability error")
	}
}
