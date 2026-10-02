package harness

import (
	"errors"
	"slices"

	"github.com/goobers/goobers/internal/sandbox"
)

// ErrGuardedCredentialFiles refuses local agentic execution when the instance
// references credential files and the harness cannot confine reads of them.
// Keep this diagnostic independent of both credential paths and their contents.
var ErrGuardedCredentialFiles = errors.New("harness: local agentic execution refused: instance config references credential or controller/private-key files; move private keys off the execution host, use environment, keychain, or secret-store refs where supported, or an isolated worker without daemon credential-file mounts")

// WithGuardedCredentialPaths refuses executor construction when a local
// instance config references credential files. Agent-authored commands cannot
// be checked in advance, and native path masks do not isolate all same-UID
// access, so WithSandboxEnforcement cannot bypass this refusal. Paths are
// retained only for native policy defense in depth, never adapter inputs or
// diagnostics.
// Isolated workers must not pass inaccessible daemon-host paths here.
func WithGuardedCredentialPaths(paths []string) Option {
	hasFiles := len(paths) > 0
	guarded := slices.Clone(paths)
	return func(e *Executor) {
		e.guardedCredentialFiles = hasFiles
		// Keep read masks attached to the sandbox factory as defense in depth;
		// this does not bypass the existing constructor refusal above the adapter.
		original := e.newSandbox
		e.newSandbox = func() (sandbox.Sandbox, error) {
			sb, err := original()
			if err != nil {
				return nil, err
			}
			return sandbox.WithReadDenials(sb, guarded), nil
		}
	}
}
