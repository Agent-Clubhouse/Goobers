// Package mcpconfig validates per-goober external MCP server declarations.
package mcpconfig

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/procenv"
)

// ValidateForHarness checks that the selected harness can isolate and
// materialize the declared servers before validating their contents. Both
// supported harnesses (Copilot, claude-code — #1492) share the same
// materialization shape: a workspace/config-scoped file plus one shared
// process environment for every locally-spawned stdio server, so the same
// validation applies to both.
func ValidateForHarness(harness apiv1.Harness, servers []apiv1.MCPServer, declaredCapabilities, tools []string) error {
	if harness == "" {
		harness = apiv1.HarnessCopilot
	}
	if err := Validate(servers, declaredCapabilities, tools); err != nil {
		return err
	}
	return validateLocalCredentialIsolation(harness, servers)
}

// Validate checks MCP server shape, tool policy, and ensures every first-party
// credential reference is backed by a capability declared for the current
// scope. A BYO reference is scoped by its containing server declaration.
func Validate(servers []apiv1.MCPServer, declaredCapabilities, tools []string) error {
	if len(servers) > 0 {
		for i, tool := range tools {
			if strings.Contains(tool, "*") {
				return fmt.Errorf("tools[%d] %q must be an explicit tool name when mcpServers are configured", i, tool)
			}
		}
	}

	declared := make(map[string]bool, len(declaredCapabilities))
	for _, value := range declaredCapabilities {
		declared[value] = true
	}
	names := make(map[string]bool, len(servers))
	for i, server := range servers {
		scope := fmt.Sprintf("mcpServers[%d]", i)
		if !validName(server.Name) {
			return fmt.Errorf("%s.name %q must be a lowercase DNS label", scope, server.Name)
		}
		if names[server.Name] {
			return fmt.Errorf("%s.name %q is declared more than once", scope, server.Name)
		}
		names[server.Name] = true

		switch {
		case server.Command != "" && server.URL == "":
			if strings.TrimSpace(server.Command) != server.Command {
				return fmt.Errorf("%s.command must not have leading or trailing whitespace", scope)
			}
		case server.Command == "" && server.URL != "":
			if len(server.Args) > 0 {
				return fmt.Errorf("%s.args are only valid for a stdio command", scope)
			}
			parsed, err := url.Parse(server.URL)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				return fmt.Errorf("%s.url %q must be an absolute HTTP(S) URL", scope, server.URL)
			}
			if parsed.Scheme != "http" && parsed.Scheme != "https" {
				return fmt.Errorf("%s.url %q must use http or https", scope, server.URL)
			}
			if parsed.User != nil {
				return fmt.Errorf("%s.url must not contain inline credentials; use credentialRefs", scope)
			}
			if parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(server.URL, "#") {
				return fmt.Errorf("%s.url must not contain a query or fragment; use credentialRefs for credentials", scope)
			}
			if parsed.Scheme == "http" && len(server.CredentialRefs) > 0 && !isLoopbackHost(parsed.Hostname()) {
				return fmt.Errorf("%s.url %q must use https when credentialRefs are configured; http is allowed only for a loopback host", scope, server.URL)
			}
		default:
			return fmt.Errorf("%s must set exactly one of command or url", scope)
		}

		envNames := make(map[string]bool, len(server.CredentialRefs))
		headerNames := make(map[string]bool, len(server.CredentialRefs))
		for j, ref := range server.CredentialRefs {
			refScope := fmt.Sprintf("%s.credentialRefs[%d]", scope, j)
			switch {
			case ref.Capability != "" && ref.Kind == "" && ref.Ref == "":
				if !capability.Known(ref.Capability) {
					return fmt.Errorf("%s.capability %q is unknown", refScope, ref.Capability)
				}
				if !capability.StageDeclarable(ref.Capability) {
					return fmt.Errorf("%s.capability %q is runner-owned", refScope, ref.Capability)
				}
				if !declared[ref.Capability] {
					return fmt.Errorf("%s.capability %q is not declared by the goober or invocation", refScope, ref.Capability)
				}
			case ref.Capability == "" && ref.Kind == apiv1.MCPCredentialKindBYO && validName(ref.Ref):
			case ref.Capability == "" && ref.Kind == apiv1.MCPCredentialKindBYO:
				return fmt.Errorf("%s.ref %q must be a lowercase DNS label", refScope, ref.Ref)
			default:
				return fmt.Errorf("%s must set either capability, or kind %q with ref", refScope, apiv1.MCPCredentialKindBYO)
			}
			switch {
			case ref.Env != "" && ref.Header == "" && ref.Scheme == "":
				if server.Command == "" {
					return fmt.Errorf("%s.env is only valid for a stdio server", refScope)
				}
				if !procenv.ValidName(ref.Env) {
					return fmt.Errorf("%s.env %q is not a valid environment variable name", refScope, ref.Env)
				}
				envName := strings.ToUpper(ref.Env)
				if envNames[envName] {
					return fmt.Errorf("%s.env %q is bound more than once", refScope, ref.Env)
				}
				envNames[envName] = true
			case ref.Env == "" && ref.Header != "":
				if server.URL == "" {
					return fmt.Errorf("%s.header is only valid for a remote server", refScope)
				}
				if !validHeaderName(ref.Header) {
					return fmt.Errorf("%s.header %q is not a valid HTTP header name", refScope, ref.Header)
				}
				header := strings.ToLower(ref.Header)
				if headerNames[header] {
					return fmt.Errorf("%s.header %q is bound more than once", refScope, ref.Header)
				}
				headerNames[header] = true
				if ref.Scheme != "" && ref.Scheme != apiv1.MCPHeaderSchemeBearer && ref.Scheme != apiv1.MCPHeaderSchemeBasic {
					return fmt.Errorf("%s.scheme %q is unsupported", refScope, ref.Scheme)
				}
			default:
				return fmt.Errorf("%s must set exactly one of env or header, and scheme is only valid with header", refScope)
			}
		}
	}
	return nil
}

// CredentialKey returns the invocation-internal key used to materialize ref.
// BYO keys are deliberately absent from internal/capability's public registry.
func CredentialKey(ref apiv1.MCPCredentialRef) string {
	if ref.Kind == apiv1.MCPCredentialKindBYO {
		return BYOCredentialKey(ref.Ref)
	}
	return ref.Capability
}

// BYOCredentialKey namespaces a named BYO credential away from first-party
// capability grants.
func BYOCredentialKey(name string) string {
	return "mcp:" + name
}

// IsBYOCredentialKey reports whether key is a well-formed internal BYO key.
func IsBYOCredentialKey(key string) bool {
	name, ok := strings.CutPrefix(key, "mcp:")
	return ok && validName(name)
}

// BYOCredentialKeys returns the unique BYO keys explicitly referenced by
// servers, preserving declaration order.
func BYOCredentialKeys(servers []apiv1.MCPServer) []string {
	var keys []string
	seen := make(map[string]bool)
	for _, server := range servers {
		for _, ref := range server.CredentialRefs {
			if ref.Kind != apiv1.MCPCredentialKindBYO {
				continue
			}
			key := BYOCredentialKey(ref.Ref)
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	return keys
}

// validateLocalCredentialIsolation rejects a declaration the given harness
// cannot materialize safely. Every supported harness launches each
// locally-spawned MCP server sharing that invocation's own process
// environment, not an isolated one per server — confirmed live for
// claude-code (#1492) the same way it was already established for Copilot —
// so a credential granted to one local stdio server is reachable by every
// other local stdio server in the same invocation unless that server
// explicitly claims it too.
func validateLocalCredentialIsolation(harness apiv1.Harness, servers []apiv1.MCPServer) error {
	keys := make([]string, 0)
	seen := make(map[string]bool)
	serverKeys := make([]map[string]bool, len(servers))
	for i, server := range servers {
		serverKeys[i] = make(map[string]bool, len(server.CredentialRefs))
		for _, ref := range server.CredentialRefs {
			key := CredentialKey(ref)
			serverKeys[i][key] = true
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	for i, server := range servers {
		if server.Command == "" {
			continue
		}
		for _, key := range keys {
			if !serverKeys[i][key] {
				return fmt.Errorf(
					"mcpServers[%d] local stdio server %q cannot isolate credential %q granted to another server because harness %q uses one shared process environment; add an explicit credentialRef for that credential or use a remote server",
					i, server.Name, key, harness,
				)
			}
		}
	}
	return nil
}

// ValidBYOCredentialName reports whether name is valid for a named BYO grant.
func ValidBYOCredentialName(name string) bool {
	return validName(name)
}

func validName(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(name)-1:
		default:
			return false
		}
	}
	return true
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}
