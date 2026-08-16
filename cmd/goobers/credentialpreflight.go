package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/credentials"
)

// credentialPreflightTimeout bounds the startup credential check. Env and file
// refs resolve instantly; the bound exists so a pathological filesystem (a
// stalled network mount holding a token file) cannot hang daemon startup, the
// same guarantee harnessPreflightTimeout gives the harness preflight.
const credentialPreflightTimeout = 10 * time.Second

// preflightCredentials is the seam buildSchedulerDefinitions calls to verify
// configured secrets at startup. It defaults to the real
// preflightCredentialRefs; the cmd/goobers test suite replaces it with a no-op
// in TestMain, since those tests scaffold instances whose token refs point at
// env vars no test process exports. The real logic is tested directly in
// internal/credentials and in credentialpreflight_test.go.
var preflightCredentials = preflightCredentialRefs

// preflightCredentialRefs fails daemon startup closed when a configured env- or
// file-backed secret reference does not resolve (#2954).
//
// Without this, a missing credential is discovered by the first stage that
// needs it: the run has already taken a claim and a worktree, the failure is
// non-retryable, and it still consumes the workflow's attempt and rate budget —
// with the root cause ("env var X is not set") buried in a stage transcript.
// The env case is not hypothetical: a daemon respawned by a service manager or
// by self-update does not inherit an operator shell's exported variables, so
// every run after such a restart fails until someone reads a transcript.
//
// Wired next to preflightHarnesses so both classes of "the instance cannot do
// work at all" are reported before any worktree, claim, or run-journal side
// effect. Resolvers that cannot enumerate their refs are skipped rather than
// treated as failures.
func preflightCredentialRefs(resolver credentials.Resolver) error {
	p, ok := resolver.(credentials.Preflighter)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialPreflightTimeout)
	defer cancel()
	return p.Preflight(ctx)
}
