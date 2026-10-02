package harness

import (
	"context"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
)

type mcpMaterializeOptions struct {
	harness              apiv1.Harness
	localEnvRefs         bool
	headerValuesInEnv    bool
	reservedEnv          []string
	defaultReservedLocal map[string]bool
	reservedServerName   string
}

type materializedMCPServer struct {
	Name     string
	Command  string
	Args     []string
	URL      string
	Env      map[string]string
	EnvNames []string
	Headers  map[string]string
}

type envAssignment struct {
	Name  string
	Value string
}

func materializeDeclaredMCP(ctx context.Context, adapter string, req RunRequest, opts mcpMaterializeOptions) ([]materializedMCPServer, []envAssignment, error) {
	if err := mcpconfig.ValidateForHarness(opts.harness, req.MCPServers, req.Envelope.Capabilities, req.Tools); err != nil {
		return nil, nil, fmt.Errorf("harness: %s: invalid MCP configuration: %w", adapter, err)
	}
	if opts.reservedServerName != "" {
		for _, server := range req.MCPServers {
			if server.Name == opts.reservedServerName {
				return nil, nil, fmt.Errorf("harness: %s: MCP server name %q is reserved for automatic goobers-io registration", adapter, server.Name)
			}
		}
	}

	reserved := make(map[string]bool, len(opts.reservedEnv))
	for _, name := range opts.reservedEnv {
		reserved[strings.ToUpper(name)] = true
	}
	localEnvOwners := map[string]string{}
	servers := make([]materializedMCPServer, 0, len(req.MCPServers))
	var assignments []envAssignment
	for serverIndex, server := range req.MCPServers {
		materialized := materializedMCPServer{
			Name:    server.Name,
			Command: server.Command,
			Args:    append([]string(nil), server.Args...),
			URL:     server.URL,
		}
		for refIndex, ref := range server.CredentialRefs {
			key, token, err := resolveMCPCredential(ctx, adapter, req, server.Name, ref)
			if err != nil {
				return nil, nil, err
			}

			envName := fmt.Sprintf("GOOBERS_MCP_CREDENTIAL_%d_%d", serverIndex, refIndex)
			if ref.Env != "" && opts.localEnvRefs {
				normalized := strings.ToUpper(ref.Env)
				if reserved[normalized] || opts.defaultReservedLocal[normalized] || strings.HasPrefix(normalized, "GOOBERS_MCP_CREDENTIAL_") {
					return nil, nil, fmt.Errorf("harness: %s: MCP environment variable %q is reserved by the adapter", adapter, ref.Env)
				}
				if owner, exists := localEnvOwners[normalized]; exists && owner != key {
					return nil, nil, fmt.Errorf("harness: %s: MCP environment variable %q is bound to both %q and %q", adapter, ref.Env, owner, key)
				}
				localEnvOwners[normalized] = key
				envName = ref.Env
			} else if ref.Env == "" && reserved[strings.ToUpper(envName)] {
				return nil, nil, fmt.Errorf("harness: %s: generated MCP environment variable %q is reserved by the adapter", adapter, envName)
			}

			envValue := token
			renderedValue := "${" + envName + "}"
			if ref.Env == "" {
				switch ref.Scheme {
				case apiv1.MCPHeaderSchemeBearer:
					if opts.headerValuesInEnv {
						envValue = "Bearer " + envValue
					} else {
						renderedValue = "Bearer " + renderedValue
					}
				case apiv1.MCPHeaderSchemeBasic:
					if opts.headerValuesInEnv {
						envValue = "Basic " + envValue
					} else {
						renderedValue = "Basic " + renderedValue
					}
				}
			}
			assignments = append(assignments, envAssignment{Name: envName, Value: envValue})

			if ref.Env != "" {
				if materialized.Env == nil {
					materialized.Env = make(map[string]string)
				}
				if opts.localEnvRefs {
					renderedValue = envName
				}
				materialized.Env[ref.Env] = renderedValue
				materialized.EnvNames = append(materialized.EnvNames, envName)
				continue
			}
			if materialized.Headers == nil {
				materialized.Headers = make(map[string]string)
			}
			if opts.headerValuesInEnv {
				renderedValue = envName
			}
			materialized.Headers[ref.Header] = renderedValue
		}
		servers = append(servers, materialized)
	}
	return servers, assignments, nil
}
