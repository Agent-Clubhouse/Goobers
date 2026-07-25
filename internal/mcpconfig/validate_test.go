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
	}
	capabilities := []string{"contents:read", "github:issues:write"}
	if err := Validate(valid, capabilities); err != nil {
		t.Fatalf("Validate valid config: %v", err)
	}

	tests := []struct {
		name    string
		servers []apiv1.MCPServer
		caps    []string
		want    string
	}{
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
			err := Validate(tc.servers, tc.caps)
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
			if err := Validate([]apiv1.MCPServer{server}, []string{"contents:read"}); err != nil {
				t.Fatalf("Validate loopback credential endpoint: %v", err)
			}
		})
	}

	if err := Validate([]apiv1.MCPServer{{
		Name: "public-remote",
		URL:  "http://mcp.example.test",
	}}, nil); err != nil {
		t.Fatalf("Validate credential-free HTTP endpoint: %v", err)
	}
}
