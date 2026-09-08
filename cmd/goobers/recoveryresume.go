package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

const recoveryResumeHelp = "Usage: goobers recovery-resume [instance]\n\n" +
	"Restore the current run's single claimed issue onto freshly fetched main,\n" +
	"then fast-forward its clean receiving worktree to the restored commit.\n" +
	"Requires workflow run context and repository credentials. Refuses another\n" +
	"branch, changed or dirty work, and expired claims. Verified retries resume\n" +
	"the prepared result; completed adoption removes its preparation branch.\n" +
	"Does not push, open a PR, release the claim, or remove retained state.\n"

func runRecoveryResume(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("recovery-resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "recovery-resume")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	registry, scrubber := journal.DefaultScrubber()
	commit, err := resumeClaimedRecovery(ctx, instance.NewLayout(root), registry)
	if err != nil {
		pf(stderr, "error: %s\n", scrubber.Scrub([]byte(err.Error())))
		return 1
	}
	pf(stdout, "adopted retained implementation at %s\n", commit)
	return 0
}

func resumeClaimedRecovery(ctx context.Context, layout instance.Layout, registry *journal.RegistryScrubber) (string, error) {
	runID, workflow, err := providerRunContext()
	if err != nil {
		return "", err
	}
	ledger, err := openStageClaimLedger(layout)
	if err != nil {
		return "", err
	}
	claim, err := currentRecoveryClaim(ctx, ledger, runID, workflow)
	if err != nil {
		return "", err
	}
	key, err := recoveryStageRepositoryKey(layout)
	if err != nil {
		return "", err
	}
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	branch, err := currentBranch(directory)
	if err != nil {
		return "", err
	}
	if branch != providers.BranchNameIn(providerBranchNamespace(), workflow, runID) {
		return "", fmt.Errorf("recovery resume requires this run's original receiving branch")
	}
	command := workspaceGitCommand(directory, "rev-parse", "--verify", "HEAD^{commit}")
	head, err := workspaceGitCombinedOutput(command)
	if err != nil {
		return "", fmt.Errorf("read receiving branch head: %w", err)
	}
	commit, err := withIssueRecoveryRecord(ctx, layout, key, claim.ItemID, func(path string) (string, error) {
		return prepareClaimedRecovery(ctx, layout, directory, runID, path, registry)
	})
	if err != nil {
		return "", err
	}
	current, err := currentRecoveryClaim(ctx, ledger, runID, workflow)
	if err != nil {
		return "", err
	}
	if claimsclient.KeyForEntry(current) != claimsclient.KeyForEntry(claim) {
		return "", fmt.Errorf("receiving issue claim changed during recovery preparation")
	}
	if err := recovery.AdoptRestoredCommit(ctx, directory, branch, strings.TrimSpace(string(head)), commit); err != nil {
		return "", err
	}
	if err := recovery.DeletePreparedRestore(ctx, directory, runID, commit); err != nil {
		return "", err
	}
	return commit, nil
}

func currentRecoveryClaim(ctx context.Context, ledger claimsclient.Ledger, runID, workflow string) (claimsclient.Entry, error) {
	claims, err := ledger.ForRunAll(ctx, runID)
	if err != nil {
		return claimsclient.Entry{}, err
	}
	if len(claims) != 1 || claims[0].ReleasedAt != nil || !claims[0].ExpiresAt.After(time.Now()) || claims[0].Workflow != workflow {
		return claimsclient.Entry{}, fmt.Errorf("recovery resume requires exactly one live claim for this workflow run")
	}
	return claims[0], nil
}

func prepareClaimedRecovery(ctx context.Context, layout instance.Layout, directory, runID, path string, registry *journal.RegistryScrubber) (string, error) {
	record, err := recovery.ReadRetainedRecord(path)
	if err != nil {
		return "", err
	}
	if !time.Now().Before(record.RetainUntil) {
		return "", fmt.Errorf("recovery retention deadline has expired")
	}
	archive := filepath.Join(filepath.Dir(path), recovery.BundleFileName)
	if err := recovery.ImportSnapshotBundle(ctx, directory, archive, record, 512<<20); err != nil {
		return "", err
	}
	prepared, err := recovery.FindPreparedRestore(ctx, directory, record, runID, 512<<20)
	if err != nil || prepared != "" {
		return prepared, err
	}
	branch, err := recovery.PreparedRestoreBranch(runID)
	if err != nil {
		return "", err
	}
	return restoreConfiguredRecovery(ctx, layout, path, directory, branch, registry)
}

func recoveryStageRepositoryKey(layout instance.Layout) (string, error) {
	routed, err := providerRepo(layout.Root)
	if err != nil {
		return "", err
	}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return "", err
	}
	var matches []string
	for _, repo := range cfg.Repos {
		if providers.ProviderKind(repo.Provider) == routed.Provider && repo.Owner == routed.Owner && repo.Project == routed.Project && repo.Name == routed.Name {
			identity := providers.RepositoryRef{Provider: routed.Provider, URL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
			matches = append(matches, identity.CanonicalKey())
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("recovery stage must resolve exactly one configured repository")
	}
	return matches[0], nil
}
