package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// Shared plumbing for the built-in provider-chain stage kinds (#131/#132):
// backlog-query, open-pr, issue-close-out. Unlike every other `goobers`
// subcommand, these are invoked by the runner as a deterministic stage's
// shell command — their process cwd is the stage's worktree, not the
// instance root — so they read their run context from GOOBERS_* env vars
// the runner injects (internal/executor/env.go's buildStageEnv), falling
// back to an explicit [path] argument for standalone/manual invocation.

// newGitHubProvider constructs the GitHub provider these subcommands talk
// to. A package var (not a plain call to providers.NewGitHubProvider) so a
// CLI-level test can point it at an httptest.Server instead of the real
// api.github.com, mirroring runnerwiring.go's newPRPoller/repoCloneURL seams.
var newGitHubProvider = providers.NewGitHubProvider

// claimLedgerFileName/claimLockFileName are the well-known files under an
// instance's scheduler dir the claim ledger and its cross-process lock
// (withClaimLock) live at — shared by backlog-query (the claimant) and
// `goobers up`'s periodic RecoverExpired sweep (the reaper), both of which
// must agree on the same paths.
const (
	claimLedgerFileName = "claims.json"
	claimLockFileName   = "claims.lock"
)

// layoutFor is instance.NewLayout, named for readability at each provider-
// chain subcommand's call site.
func layoutFor(root string) instance.Layout {
	return instance.NewLayout(root)
}

// providerStageRoot resolves the instance root a provider-chain subcommand
// operates against: an explicit pathArg wins (standalone/manual invocation,
// matching every other subcommand's [path] convention), else
// GOOBERS_INSTANCE_ROOT (the runner-injected env var), else ".".
func providerStageRoot(pathArg string) string {
	if pathArg != "" {
		return pathArg
	}
	if root := os.Getenv("GOOBERS_INSTANCE_ROOT"); root != "" {
		return root
	}
	return "."
}

// providerRepo loads instance.yaml at root and returns its single configured
// target repo as a providers.RepositoryRef — V0's single-target-repo
// simplification (matching runnerwiring.go's buildCredentials/
// buildCIPollExecutor), and V0 ships GitHub only (instance.Config.Repos[].
// Provider is already validated to "github" at load time).
func providerRepo(root string) (providers.RepositoryRef, error) {
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return providers.RepositoryRef{}, err
	}
	if len(cfg.Repos) == 0 {
		return providers.RepositoryRef{}, fmt.Errorf("no repo configured in %s", l.ConfigFile())
	}
	repo := cfg.Repos[0]
	return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}, nil
}

// providerToken reads the capability-scoped credential the runner already
// resolved and injected for this stage process (executor/env.go's
// buildStageEnv, executor.CredentialEnvVar) — it never re-resolves the token
// independently, so it stays covered by the run's registrar-based secret
// scrubbing rather than becoming a second, unregistered copy of the secret.
func providerToken(cap capability.Capability) (string, error) {
	envVar := executor.CredentialEnvVar(string(cap))
	token := os.Getenv(envVar)
	if token == "" {
		return "", fmt.Errorf("no credential in %s env var — this subcommand must run as a stage declaring capabilities: [%q] (or set %s directly for standalone use)", envVar, cap, envVar)
	}
	return token, nil
}

// providerInput reads a declared Task.Inputs value the runner passed through
// as a GOOBERS_INPUT_* env var (executor/env.go's buildStageEnv,
// executor.InputEnvVar), falling back to def when unset.
func providerInput(key, def string) string {
	if v := os.Getenv(executor.InputEnvVar(key)); v != "" {
		return v
	}
	return def
}

// providerRunContext reads the run/workflow identity the runner injects for
// every stage process (GOOBERS_RUN_ID/GOOBERS_WORKFLOW). Both are required —
// a provider-chain subcommand run outside a real stage dispatch (e.g. by
// hand, with no GOOBERS_RUN_ID/GOOBERS_WORKFLOW set) has no meaningful
// claim/PR/branch identity to act under, so this fails closed rather than
// proceeding with an empty runID (which could collide with, or never match,
// a real run's claims) or an empty workflow (which would make
// providers.BranchName(workflow, runID) produce a malformed branch name).
func providerRunContext() (runID, workflow string, err error) {
	runID = os.Getenv("GOOBERS_RUN_ID")
	workflow = os.Getenv("GOOBERS_WORKFLOW")
	if runID == "" {
		return "", "", fmt.Errorf("GOOBERS_RUN_ID is not set — this subcommand must run as a workflow stage")
	}
	if workflow == "" {
		return "", "", fmt.Errorf("GOOBERS_WORKFLOW is not set — this subcommand must run as a workflow stage")
	}
	return runID, workflow, nil
}

// withClaimLock serializes fn against every other process (a concurrent
// `goobers backlog-query` from a racing run, or `goobers up`'s periodic
// RecoverExpired) touching the same instance's claim ledger, via a blocking
// exclusive flock on lockPath. localscheduler.ClaimLedger's own mutex is
// documented as in-process only ("designed for one embedded scheduler per
// instance... not cross-process file locking") — but backlog-query runs as
// its own OS process per stage dispatch, so two concurrent runs'
// backlog-query subprocesses each open an independent ClaimLedger instance
// against the SAME claims.json file: without this lock, both could read the
// file before either persists, and the second write would silently clobber
// the first's claim (a lost update), defeating "one claim wins" (#131's
// acceptance criterion). A blocking (not acquireInstanceLock's non-blocking)
// flock is deliberate: the loser here should wait its turn and get a
// consistent read, not fail outright the way a second `goobers up` should.
func withClaimLock(lockPath string, fn func() error) error {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open claim lock file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire claim lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
