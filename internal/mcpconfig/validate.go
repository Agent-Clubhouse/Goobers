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

// Validate checks MCP server shape and ensures every credential reference is
// backed by a capability declared for the current scope.
func Validate(servers []apiv1.MCPServer, declaredCapabilities []string) error {
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
			if !capability.Known(ref.Capability) {
				return fmt.Errorf("%s.capability %q is unknown", refScope, ref.Capability)
			}
			if !capability.StageDeclarable(ref.Capability) {
				return fmt.Errorf("%s.capability %q is runner-owned", refScope, ref.Capability)
			}
			if !declared[ref.Capability] {
				return fmt.Errorf("%s.capability %q is not declared by the goober or invocation", refScope, ref.Capability)
			}
			switch {
			case ref.Env != "" && ref.Header == "" && ref.Scheme == "":
				if server.Command == "" {
					return fmt.Errorf("%s.env is only valid for a stdio server", refScope)
				}
				if !procenv.ValidName(ref.Env) {
					return fmt.Errorf("%s.env %q is not a valid environment variable name", refScope, ref.Env)
				}
				if envNames[ref.Env] {
					return fmt.Errorf("%s.env %q is bound more than once", refScope, ref.Env)
				}
				envNames[ref.Env] = true
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
