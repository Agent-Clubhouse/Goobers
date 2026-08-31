package credentials

import "strings"

// A connection is a named, reusable link to an external system that a gaggle
// references by name (apiv1.Connection; GaggleSpec.Project.ConnectionRef and
// GaggleSpec.Backlog.ConnectionRef). At the local instance tier a connection's
// credential is sourced by an instance.yaml credentials: entry, exactly like a
// named BYO MCP credential is — and for the same reason: it is NOT a stage
// capability, so it must be unreachable by capability declaration alone and
// reachable only through an explicit, config-declared reference.
//
// This is what lets one gaggle hold two DISTINCT credentials at once: its
// project repository keeps the ordinary capability-scoped repo token, while its
// backlog resolves a separate connection-scoped token. A private backlog in one
// account and a target repository in another therefore never share a PAT.

// connectionCredentialPrefix namespaces connection credential keys away from
// canonical capability strings ("github:issues:write") and BYO MCP keys, so no
// key space can collide with another.
const connectionCredentialPrefix = "connection:"

// ConnectionCredentialKey returns the credential key a named connection's token
// is granted and injected under.
func ConnectionCredentialKey(name string) string {
	return connectionCredentialPrefix + name
}

// IsConnectionCredentialKey reports whether key names a connection credential.
func IsConnectionCredentialKey(key string) bool {
	return strings.HasPrefix(key, connectionCredentialPrefix)
}

// ConnectionCredentialName returns the connection name behind a credential key.
func ConnectionCredentialName(key string) (string, bool) {
	name, ok := strings.CutPrefix(key, connectionCredentialPrefix)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// ValidConnectionName reports whether name is usable as a connection reference.
// It mirrors the BYO MCP credential name rule (lowercase alphanumerics and
// interior hyphens, at most 63 characters) so connection names, MCP names, and
// Kubernetes-style object names all share one shape.
func ValidConnectionName(name string) bool {
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
