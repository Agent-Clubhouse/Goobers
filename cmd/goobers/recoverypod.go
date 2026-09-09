package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
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
	request := recovery.RetentionRequest{
		Repository: repository, RepositoryKey: repo.CanonicalKey(), RunID: runID, BaseRef: base,
		IdentityTime: now, RetainUntil: now.Add(30 * 24 * time.Hour),
		InventoryRoot: inventory, CleanupRoots: []string{repository}, MaxSnapshots: 128, MaxArchiveBytes: 512 << 20, SkipEmpty: true,
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
