package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

// publishPodRecovery belongs to the supervisor, not the stage subprocess.
// It uses the existing parent bearer and never exports additional authority.
// Success acknowledges host custody of both dirty and committed work; it does
// not make either eligible as a successful cross-stage workspace delta.
func publishPodRecovery(ctx context.Context, repository string) error {
	if !stageWorkspaceIsWritableRepo() {
		return nil
	}
	runID, gaggle := os.Getenv(dispatcher.EnvRunID), os.Getenv(dispatcher.EnvGaggle)
	endpoint, token := os.Getenv(dispatcher.EnvDaemonAPI), os.Getenv(dispatcher.EnvPodToken)
	client, err := claimsclient.NewHTTP(claimsclient.HTTPConfig{BaseURL: endpoint, Token: token, RunID: runID})
	if err != nil {
		return err
	}
	claims, err := client.ForRunAll(ctx, runID)
	if err != nil {
		return err
	}
	// A review run can legitimately have no claimed issue, and has no item
	// whose recovery namespace could receive an archive.
	if len(claims) == 0 {
		return nil
	}
	if len(claims) != 1 || claims[0].RunID != runID || claims[0].Gaggle != gaggle ||
		claims[0].ItemID == "" || claims[0].ReleasedAt != nil || !claims[0].ExpiresAt.After(time.Now()) {
		return fmt.Errorf("pod recovery requires exactly one current issue claim")
	}
	ctx, cancel := context.WithDeadline(ctx, claims[0].ExpiresAt)
	defer cancel()
	repo := providers.RepositoryRef{
		Provider: providers.ProviderKind(os.Getenv(executor.RepoProviderEnvVar)),
		URL:      os.Getenv(executor.RepoBaseURLEnvVar),
		Owner:    os.Getenv(executor.RepoOwnerEnvVar), Project: os.Getenv(executor.RepoProjectEnvVar),
		Name: os.Getenv(executor.RepoNameEnvVar),
	}
	if repo.Provider == "" || repo.Owner == "" || repo.Name == "" {
		return fmt.Errorf("pod recovery requires a routed repository identity")
	}
	base := os.Getenv(executor.BaseBranchEnvVar)
	if base == "" {
		base = "main"
	}
	base, err = resolveRecoveryBaseRefWithFetch(ctx, repository, base)
	if err != nil {
		return err
	}
	// The in-pod checkout is materialized outside worktree.Manager, so it never
	// received the managed mirror's harness excludes. Register them here, before
	// anything reads the working tree: both podWorkspaceNeedsRecovery's
	// `status --porcelain` and the capture below select untracked files, so
	// without this a stage that only wrote its own result file and mutation
	// sidecar publishes a bookkeeping-only archive and consumes a slot (#5119).
	executor.ExcludeStageArtifacts(ctx, repository, os.Getenv(executor.InputEnvVar(executor.InputResultFile)))
	needed, err := podWorkspaceNeedsRecovery(ctx, repository, base)
	if err != nil {
		return err
	}
	if !needed {
		return nil
	}
	// The temporary inventory is only upload staging. Do not remove it on a
	// failed transfer; only the acknowledged host archive is durable custody.
	root, err := os.MkdirTemp("", "goobers-pod-recovery-")
	if err != nil {
		return err
	}
	inventory, err := prepareRecoveryInventory(root)
	if err != nil {
		return err
	}
	publisher := recovery.HTTPArchivePublisher{BaseURL: endpoint, Token: token, RunID: runID}
	now := time.Now().UTC()
	// The pod has no instance config access (no instance root, only routed
	// env vars), so it falls back to the shared opt-out defaults rather than a
	// raw literal (#4823). The host-side acknowledgement in
	// recoverypublication.go applies the operator's real configured limits.
	request := recovery.RetentionRequest{
		Repository: repository, RepositoryKey: repo.CanonicalKey(), RunID: runID, BaseRef: base,
		IdentityTime: now, RetainUntil: now.Add(instance.DefaultRecoverySnapshotRetainWindow),
		InventoryRoot: inventory, CleanupRoots: []string{repository},
		MaxSnapshots: instance.DefaultRecoverySnapshotMaxCount, MaxArchiveBytes: instance.DefaultRecoverySnapshotMaxArchiveBytes, SkipEmpty: true,
		AcknowledgeArchive: func(ctx context.Context, record recovery.Record, archive string) error {
			return publisher.PublishArchive(ctx, claims[0].ItemID, record, archive)
		},
	}
	publication := recoveryCleanupJournal{directory: filepath.Join(root, "journal"), scrubber: journal.NewRegistryScrubber()}
	if err := recovery.RetainAbandonedPreparation(ctx, request, publication); err != nil {
		return err
	}
	_, _, err = recovery.Retain(ctx, request, publication)
	return err
}

// podWorkspaceNeedsRecovery reports whether a writable workspace has either
// uncommitted changes or commits beyond its recovery base. A clean reviewer
// checkout has neither, so no archive is required to dispose its pod.
func podWorkspaceNeedsRecovery(ctx context.Context, repository, baseRef string) (bool, error) {
	output, err := (podGit{}).Output(ctx, repository, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("inspect workspace changes for recovery custody: %w", err)
	}
	if len(output) != 0 {
		return true, nil
	}
	diff := exec.CommandContext(ctx, "git", "diff", "--quiet", baseRef+"...HEAD")
	diff.Dir = repository
	diff.Env = composeGitEnv(repository, nil)
	if err := diff.Run(); err == nil {
		return false, nil
	} else if exitErr := new(exec.ExitError); errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	} else {
		return false, fmt.Errorf("inspect committed workspace changes for recovery custody: %w", err)
	}
}

// resolveRecoveryBaseRef picks the git revision recovery custody's merge-base
// resolution (internal/recovery.PrepareRecord) reads the configured base
// branch through. dispatch-checkout.go clones only the branch this stage
// actually needs (see checkoutRepoWorkspace's WRITABLE REPO comment): a
// writable stage dispatched directly onto an already-existing run branch
// clones with `--branch <run branch>` and never gets a local "<base>" branch
// at all — only checkoutRepoWorkspace's now-guaranteed
// refs/remotes/origin/<base> (#5103). A fresh run's fallback clone, by
// contrast, checks out base directly first and so leaves a real
// refs/heads/<base>. Try the local branch first — cheapest, and correct for
// that fallback path and for repo-readonly's single-branch-at-base clone —
// and fall back to the remote-tracking ref checkoutRepoWorkspace fetches for
// exactly this purpose. internal/recovery deliberately runs no git transport
// of its own (see recoveryGitIO's doc comment), so by the time this runs the
// ref must already be resolvable locally.
//
// The probe goes through podGit, not a bare exec.Command, for the reason
// workspaceGitCommand documents: /workspace is not owned by the container
// user, so git without the safe.directory exemption refuses the repository
// outright with "detected dubious ownership" and exit 128. A bare probe
// therefore fails for EVERY candidate no matter which refs exist — and
// discarding its stderr reported that as the base branch being missing.
// MEASURED: reproduced at uid mismatch; #5162 and #5180 both chased a ref
// that was present the whole time.
//
// Hence the exit-status discrimination: only exit 1, rev-parse's "no such
// ref", may advance to the next candidate or fall through to the "not
// resolvable" verdict. Any other status is git failing to answer the
// question, and is surfaced with git's own message rather than being
// silently recast as an absent ref.
func resolveRecoveryBaseRef(ctx context.Context, repository, base string) (string, error) {
	for _, candidate := range []string{"refs/heads/" + base, "refs/remotes/origin/" + base} {
		_, err := (podGit{}).Output(ctx, repository, "rev-parse", "--verify", "--quiet", candidate+"^{commit}")
		if err == nil {
			return candidate, nil
		}
		if !gitRefAbsent(err) {
			return "", fmt.Errorf("probe recovery base ref %s: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("recovery base branch %q is not resolvable in this checkout", base)
}

// gitRefAbsent reports whether a `rev-parse --verify --quiet` failure means
// the ref is simply not there (exit 1) rather than git having refused to run
// at all (exit 128: dubious ownership, not a repository, corrupt index).
func gitRefAbsent(err error) bool {
	exitErr := new(exec.ExitError)
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// resolveRecoveryBaseRefWithFetch is resolveRecoveryBaseRef plus one retry: a
// checkout-time step (checkoutRepoWorkspace's ensureRecoveryBaseRemoteRef, on
// every writable arm as of #5103) is supposed to guarantee base resolves
// before the stage ever runs, but a narrower recurrence of #5103 showed a
// pr-remediation gather-pr-context pod reaching custody with base
// unresolvable anyway — the stage's OWN git operations run between checkout
// and here and are outside checkout's control (gather-pr-context checks out
// its selected PR's head directly, replacing whatever branch checkout left
// HEAD on). internal/recovery deliberately runs no git transport of its own
// (recoveryGitIO's doc comment), so custody has to be able to re-fetch base
// itself, with the same credential the checkout was minted, rather than fail
// closed on a gap checkout can no longer be trusted to have closed for good.
func resolveRecoveryBaseRefWithFetch(ctx context.Context, repository, base string) (string, error) {
	resolved, err := resolveRecoveryBaseRef(ctx, repository, base)
	if err == nil {
		return resolved, nil
	}
	if fetchErr := fetchRecoveryBaseRef(ctx, repository, base); fetchErr != nil {
		return "", fmt.Errorf("%w (re-fetching %s from origin for recovery custody also failed: %w)", err, base, fetchErr)
	}
	return resolveRecoveryBaseRef(ctx, repository, base)
}

// fetchRecoveryBaseRef fetches base from the checkout's own "origin" remote
// into a persistent refs/remotes/origin/<base>, authenticated with whatever
// credential this stage's checkout was minted (anonymous when it was none —
// correct for a public repository, and the same failure mode a credentialed
// fetch would have against a private one).
func fetchRecoveryBaseRef(ctx context.Context, dir, base string) error {
	url, err := originURL(dir)
	if err != nil {
		return err
	}
	var authEnv []string
	if creds, credErr := resolveCheckoutCredential(ctx); credErr == nil {
		if token := gitToken(creds); token != "" {
			authEnv = gitAuthEnv(token)
		}
	}
	var cmd *exec.Cmd
	if authEnv != nil {
		cmd = workspaceGitAuthEnvCommand(dir, authEnv, "fetch", "--quiet", url, base+":refs/remotes/origin/"+base)
	} else {
		cmd = workspaceGitCommand(dir, "fetch", "--quiet", url, base+":refs/remotes/origin/"+base)
	}
	out, err := workspaceGitCombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("fetch base %s: %w: %s", base, err, strings.TrimSpace(string(out)))
	}
	return nil
}
