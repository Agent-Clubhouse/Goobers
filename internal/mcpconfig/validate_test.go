package mcpconfig

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestValidate(t *testing.T) {
	valid := []apiv1.MCPServer{
		{
			Name:    "local-context",
			Command: "context-server",
			Args:    []string{"--stdio"},
			CredentialRefs: []apiv1.MCPCredentialRef{{
				Capability: "contents:read",
				Env:        "CONTEXT_TOKEN",
			}},
		},
		{
			Name: "remote-context",
			URL:  "https://mcp.example.test/api",
			CredentialRefs: []apiv1.MCPCredentialRef{{
				Capability: "github:issues:write",
				Header:     "Authorization",
				Scheme:     apiv1.MCPHeaderSchemeBearer,
			}},
		},
		{
			Name: "vendor-context",
			URL:  "https://vendor.example.test/mcp",
			CredentialRefs: []apiv1.MCPCredentialRef{{
				Kind:   apiv1.MCPCredentialKindBYO,
				Ref:    "vendor-api",
				Header: "X-API-Key",
			}},
		},
	}
	capabilities := []string{"contents:read", "github:issues:write"}
	if err := Validate(valid, capabilities, []string{"reachability"}); err != nil {
		t.Fatalf("Validate valid config: %v", err)
	}

	tests := []struct {
		name    string
		servers []apiv1.MCPServer
		caps    []string
		tools   []string
		want    string
	}{
		{
			name:    "wildcard tool selector",
			servers: []apiv1.MCPServer{{Name: "local", Command: "server"}},
			tools:   []string{"reachability", "*"},
			want:    "must be an explicit tool name",
		},
		{
			name:    "duplicate server",
			servers: append(append([]apiv1.MCPServer(nil), valid...), valid[0]),
			caps:    capabilities,
			want:    "declared more than once",
		},
		{
			name:    "both transports",
			servers: []apiv1.MCPServer{{Name: "mixed", Command: "server", URL: "https://example.test"}},
			want:    "exactly one of command or url",
		},
		{
			name:    "remote args",
			servers: []apiv1.MCPServer{{Name: "remote", URL: "https://example.test", Args: []string{"bad"}}},
			want:    "args are only valid",
		},
		{
			name:    "inline URL credentials",
			servers: []apiv1.MCPServer{{Name: "remote", URL: "https://token@example.test"}},
			want:    "must not contain inline credentials",
		},
		{
			name:    "inline query credentials",
			servers: []apiv1.MCPServer{{Name: "remote", URL: "https://example.test/mcp?api_key=secret"}},
			want:    "must not contain a query or fragment",
		},
		{
			name:    "inline fragment credentials",
			servers: []apiv1.MCPServer{{Name: "remote", URL: "https://example.test/mcp#secret"}},
			want:    "must not contain a query or fragment",
		},
		{
			name:    "empty query",
			servers: []apiv1.MCPServer{{Name: "remote", URL: "https://example.test/mcp?"}},
			want:    "must not contain a query or fragment",
		},
		{
			name:    "empty fragment",
			servers: []apiv1.MCPServer{{Name: "remote", URL: "https://example.test/mcp#"}},
			want:    "must not contain a query or fragment",
		},
		{
			name: "plaintext remote credential",
			servers: []apiv1.MCPServer{{
				Name: "remote", URL: "http://mcp.example.test",
				CredentialRefs: []apiv1.MCPCredentialRef{{Capability: "contents:read", Header: "Authorization"}},
			}},
			caps: []string{"contents:read"},
			want: "must use https when credentialRefs are configured",
		},
		{
			name: "undeclared credential",
			servers: []apiv1.MCPServer{{
				Name: "remote", URL: "https://example.test",
				CredentialRefs: []apiv1.MCPCredentialRef{{Capability: "contents:read", Header: "Authorization"}},
			}},
			want: "is not declared",
		},
		{
			name: "unknown credential",
			servers: []apiv1.MCPServer{{
				Name: "local", Command: "server",
				CredentialRefs: []apiv1.MCPCredentialRef{{Capability: "unknown:read", Env: "TOKEN"}},
			}},
			caps: []string{"unknown:read"},
			want: "is unknown",
		},
		{
			name: "mixed first-party and BYO credential",
			servers: []apiv1.MCPServer{{
				Name: "local", Command: "server",
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Capability: "contents:read",
					Kind:       apiv1.MCPCredentialKindBYO,
					Ref:        "vendor-api",
					Env:        "TOKEN",
				}},
			}},
			caps: []string{"contents:read"},
			want: "must set either capability",
		},
		{
			name: "BYO credential missing ref",
			servers: []apiv1.MCPServer{{
				Name: "local", Command: "server",
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Kind: apiv1.MCPCredentialKindBYO,
					Env:  "TOKEN",
				}},
			}},
			want: "must be a lowercase DNS label",
		},
		{
			name: "BYO credential invalid ref",
			servers: []apiv1.MCPServer{{
				Name: "local", Command: "server",
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Kind: apiv1.MCPCredentialKindBYO,
					Ref:  "Vendor API",
					Env:  "TOKEN",
				}},
			}},
			want: "must be a lowercase DNS label",
		},
		{
			name: "wrong binding transport",
			servers: []apiv1.MCPServer{{
				Name: "remote", URL: "https://example.test",
				CredentialRefs: []apiv1.MCPCredentialRef{{Capability: "contents:read", Env: "TOKEN"}},
			}},
			caps: []string{"contents:read"},
			want: "only valid for a stdio server",
		},
		{
			name: "case-insensitive duplicate env binding",
			servers: []apiv1.MCPServer{{
				Name:    "local",
				Command: "server",
				CredentialRefs: []apiv1.MCPCredentialRef{
					{Capability: "contents:read", Env: "TOKEN"},
					{Capability: "github:issues:write", Env: "token"},
				},
			}},
			caps: []string{"contents:read", "github:issues:write"},
			want: "is bound more than once",
		},
		{
			name: "invalid header",
			servers: []apiv1.MCPServer{{
				Name: "remote", URL: "https://example.test",
				CredentialRefs: []apiv1.MCPCredentialRef{{Capability: "contents:read", Header: "Bad Header"}},
			}},
			caps: []string{"contents:read"},
			want: "not a valid HTTP header name",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.servers, tc.caps, tc.tools)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want %q", err, tc.want)
			}
		})
	}

	for _, endpoint := range []string{
		"http://localhost:8080/mcp",
		"http://127.0.0.1:8080/mcp",
		"http://[::1]:8080/mcp",
	} {
		t.Run("plaintext loopback credential "+endpoint, func(t *testing.T) {
			server := apiv1.MCPServer{
				Name: "local-remote",
				URL:  endpoint,
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Capability: "contents:read",
					Header:     "Authorization",
				}},
			}
			if err := Validate([]apiv1.MCPServer{server}, []string{"contents:read"}, nil); err != nil {
				t.Fatalf("Validate loopback credential endpoint: %v", err)
			}
		})
	}

	if err := Validate([]apiv1.MCPServer{{
		Name: "public-remote",
		URL:  "http://mcp.example.test",
	}}, nil, nil); err != nil {
		t.Fatalf("Validate credential-free HTTP endpoint: %v", err)
	}
}

func TestBYOCredentialKeys(t *testing.T) {
	servers := []apiv1.MCPServer{
		{CredentialRefs: []apiv1.MCPCredentialRef{
			{Kind: apiv1.MCPCredentialKindBYO, Ref: "vendor-api"},
			{Capability: "contents:read"},
		}},
		{CredentialRefs: []apiv1.MCPCredentialRef{
			{Kind: apiv1.MCPCredentialKindBYO, Ref: "vendor-api"},
			{Kind: apiv1.MCPCredentialKindBYO, Ref: "jira"},
		}},
	}
	keys := BYOCredentialKeys(servers)
	if len(keys) != 2 || keys[0] != "mcp:vendor-api" || keys[1] != "mcp:jira" {
		t.Fatalf("BYOCredentialKeys = %v, want [mcp:vendor-api mcp:jira]", keys)
	}
	if !IsBYOCredentialKey(keys[0]) || IsBYOCredentialKey("mcp:Vendor") || IsBYOCredentialKey("contents:read") {
		t.Fatalf("IsBYOCredentialKey accepted an invalid key")
	}
	if got := CredentialKey(apiv1.MCPCredentialRef{Capability: "contents:read"}); got != "contents:read" {
		t.Fatalf("first-party CredentialKey = %q", got)
	}
}

// TestValidateForHarness pins #1492: mcpServers are adapter-neutral —
// declaring them is no longer rejected for claude-code, matching Copilot.
func TestValidateForHarness(t *testing.T) {
	servers := []apiv1.MCPServer{{Name: "context", Command: "context-server"}}
	for _, harness := range []apiv1.Harness{"", apiv1.HarnessCopilot, apiv1.HarnessClaudeCode} {
		if err := ValidateForHarness(harness, servers, nil, []string{"read-context"}); err != nil {
			t.Fatalf("ValidateForHarness(%q): %v", harness, err)
		}
	}
}

// TestValidateForHarnessRejectsCredentialExposureToLocalSibling pins #1492's
// AC3: the shared-process-environment credential-isolation rejection applies
// identically to every harness — confirmed live that claude-code's stdio MCP
// servers inherit the full parent process environment exactly like
// Copilot's, so the same risk (and the same rejection) applies there too.
func TestValidateForHarnessRejectsCredentialExposureToLocalSibling(t *testing.T) {
	for _, harness := range []apiv1.Harness{apiv1.HarnessCopilot, apiv1.HarnessClaudeCode} {
		t.Run(string(harness), func(t *testing.T) {
			credential := apiv1.MCPCredentialRef{
				Kind:   apiv1.MCPCredentialKindBYO,
				Ref:    "vendor-api",
				Header: "Authorization",
			}
			servers := []apiv1.MCPServer{
				{Name: "local-context", Command: "context-server"},
				{Name: "vendor-context", URL: "https://vendor.example.test/mcp", CredentialRefs: []apiv1.MCPCredentialRef{credential}},
			}
			err := ValidateForHarness(harness, servers, nil, nil)
			if err == nil ||
				!strings.Contains(err.Error(), `local stdio server "local-context" cannot isolate credential "mcp:vendor-api"`) ||
				!strings.Contains(err.Error(), string(harness)) {
				t.Fatalf("ValidateForHarness error = %v, want local credential-isolation rejection naming %q", err, harness)
			}

			servers[0].CredentialRefs = []apiv1.MCPCredentialRef{{
				Kind: apiv1.MCPCredentialKindBYO,
				Ref:  "vendor-api",
				Env:  "VENDOR_TOKEN",
			}}
			if err := ValidateForHarness(harness, servers, nil, nil); err != nil {
				t.Fatalf("ValidateForHarness explicitly shared credential: %v", err)
			}
		})
	}
}
