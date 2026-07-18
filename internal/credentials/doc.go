// Package credentials is the local-tier secret-resolver and capability-scoped
// credential injection seam (issue #14, docs/ARCHITECTURE.md §9, security.md
// SEC-041/042/045/046).
//
// It resolves named token refs (env var or file, per instance.yaml) into
// secret values, and hands them out only for the capabilities a stage
// declared — nothing is materialized for a capability that was not declared,
// so undeclared use fails closed because there is simply no credential to
// use. Every resolved value is registered with a SecretRegistrar (the run
// journal's scrubber, wired in by the caller) before it is handed out, so it
// can never land at rest in a journal, span, snapshot, or artifact.
//
// This package deliberately does not import providers or journal: it defines
// the shape it needs (SecretRegistrar) and returns values that satisfy the
// shapes other packages need (a Token(ctx) (string, error) method matches
// providers.TokenSource structurally) so build order across concurrent
// missions does not gate this one. Resolver is the SEC-010 drop-in point: a
// Key Vault implementation replaces the local env/file implementation at V2
// (ARCHITECTURE.md §10) without caller changes.
package credentials
