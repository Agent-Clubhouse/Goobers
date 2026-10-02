package harness

import "errors"

// ErrGuardedCredentialFiles refuses local agentic execution when the instance
// references credential files and the harness cannot confine reads of them.
// Keep this diagnostic independent of both credential paths and their contents.
var ErrGuardedCredentialFiles = errors.New("harness: local agentic execution refused: instance config references credential files and the agentic sandbox does not confine reads; use environment, keychain, or secret-store credential refs, or an isolated worker without daemon credential-file mounts")

// WithGuardedCredentialPaths refuses executor construction when a local
// instance config references credential files. Agent-authored commands cannot
// be checked in advance, and the current sandbox confines writes only, so even
// WithSandboxEnforcement cannot bypass this refusal. Only presence is retained;
// paths are never opened, passed to adapters, or included in diagnostics.
// Isolated workers must not pass inaccessible daemon-host paths here.
func WithGuardedCredentialPaths(paths []string) Option {
	hasFiles := len(paths) > 0
	return func(e *Executor) { e.guardedCredentialFiles = hasFiles }
}
