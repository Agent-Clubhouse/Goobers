package main

import (
	"context"
	"flag"
	"io"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// runPreflightRepoWrite implements `goobers preflight-repo-write` (#4414): a
// non-mutating check of whether the configured repository credential can
// push the run's branch namespace, run as the workflow's first stage so a
// bad credential or a blocking branch ruleset refuses the claim before any
// implementation/review/CI resources are spent — the failure mode MDB5
// observed eight times over (issue #4414): only discovering the same
// problem at `push-branch`, after the work was already done.
//
// Exit codes: 0 = pushable, 1 = a known preflight failure (repository
// unreachable/unauthorized, authenticated without push permission, a
// branch ruleset denies the namespace, or ruleset introspection is
// unavailable — each printed with its distinct failure capability so the
// run's status/diagnose output names exactly what is broken), 2 =
// usage/IO error.
const preflightRepoWriteHelp = "Usage: goobers preflight-repo-write [path]\n\n" +
	"Check, without mutating any repository state, whether the configured\n" +
	"repository credential can push this run's branch namespace. Reads two\n" +
	"provider endpoints (repository permissions, branch ruleset policy) and\n" +
	"reports one of four distinct outcomes: unreachable/unauthorized,\n" +
	"authenticated without push permission, a branch ruleset denying the\n" +
	"namespace, or ruleset introspection unavailable for this credential —\n" +
	"never inferred as a pass.\n\n" +
	"The `branch` input overrides the checked branch name; it defaults to\n" +
	"the gaggle's configured branch namespace plus a representative suffix,\n" +
	"since the exact per-run branch name is not generated until later\n" +
	"(worktree checkout) — a namespace-prefix ruleset, the common case,\n" +
	"still evaluates correctly against this default.\n" +
	"[path] defaults to the current directory (the stage's worktree).\n" +
	"Exit codes: 0 = pushable, 1 = a known preflight failure, 2 = usage/IO error.\n"

func runPreflightRepoWrite(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("preflight-repo-write", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "preflight-repo-write")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	branch := providerInput("branch", providerBranchNamespace()+"preflight-check")

	provider, err := newProviderForStage(root, repo, true, withStageProviderCapability(capability.RepoPush))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	dispatcher := providers.NewDispatcher(provider)

	ctx, cancel := context.WithTimeout(context.Background(), stageTimeout())
	defer cancel()
	result, err := dispatcher.PreflightRepositoryWrite(ctx, repo, branch)
	if err != nil {
		pf(stderr, "error: repository-write preflight for %s/%s could not run: %v\n", repo.Owner, repo.Name, err)
		return 1
	}
	if !result.OK {
		pf(stderr, "error: repository-write preflight failed: repo=%s/%s capability=%s: %s\n",
			repo.Owner, repo.Name, result.FailureCapability, result.Detail)
		return 1
	}
	pf(stdout, "repository-write preflight: %s/%s can push %q\n", repo.Owner, repo.Name, branch)
	return 0
}
