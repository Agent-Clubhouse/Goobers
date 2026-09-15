package main

import (
	"context"
	"fmt"
	"io"
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
// ref must already be resolvable locally; neither candidate resolving means
// a checkout-time bug, not something recovery can repair after the fact.
func resolveRecoveryBaseRef(ctx context.Context, repository, base string) (string, error) {
	for _, candidate := range []string{"refs/heads/" + base, "refs/remotes/origin/" + base} {
		probe := exec.CommandContext(ctx, "git", "-C", repository, "rev-parse", "--verify", "--quiet", candidate+"^{commit}")
		probe.Stdout, probe.Stderr = io.Discard, io.Discard
		if probe.Run() == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("recovery base branch %q is not resolvable in this checkout", base)
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
		return "", fmt.Errorf("%w (re-fetching %s from origin for recovery custody also failed: %v)", err, base, fetchErr)
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
