package main

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func (s *daemonClaimService) releaseProviderClaim(ctx context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	if s.markerRelease == nil {
		return httpapi.ClaimResponse{}, fmt.Errorf("provider cleanup is unavailable")
	}
	if request.ItemID == "" || request.ClaimedAt.IsZero() {
		return httpapi.ClaimResponse{}, fmt.Errorf("provider cleanup requires an item and claim incarnation")
	}
	client, err := fileClaimLedger(s.layout, withClaimJournal(s.log)...)
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	entry := localscheduler.ClaimEntry{
		Gaggle: request.Gaggle, Provider: request.Provider, ExternalID: request.ItemID,
		ItemID: request.ItemID, RunID: request.RunID, ClaimedAt: request.ClaimedAt,
	}
	ok, err := client.(claimsclient.ProviderReleaser).ReleaseWithProvider(ctx, entry, request.Reason, func(ctx context.Context) error {
		return s.markerRelease(ctx, entry, request.Reason)
	})
	return httpapi.ClaimResponse{Ok: ok}, err
}

// Provider credentials and repository routing are resolved from the daemon's
// trusted run identity, never from repository data supplied by the stage.
func (u *upSession) releaseProviderClaimMarker(ctx context.Context, entry localscheduler.ClaimEntry, reason claimrelease.Reason) error {
	dir, err := u.l.FindRunDir(entry.RunID)
	if err != nil {
		return err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return err
	}
	identity, err := reader.Identity()
	if err != nil {
		return err
	}
	if identity.RunID != entry.RunID || identity.Gaggle != entry.Gaggle || identity.WorkspaceRepository == nil {
		return fmt.Errorf("provider cleanup requires a matching pinned run repository")
	}
	project := *identity.WorkspaceRepository
	if project.Provider == "gitea" {
		baseURL, err := terminalGiteaBaseURLForProject(u.setup.Config, project)
		if err != nil {
			return err
		}
		resolver, grants, err := buildRoleCredentials(u.setup.Config, u.setup.SecretStores, project.Owner, project.Name, nil, nil, u.setup.SharedRegistry)
		if err != nil {
			return err
		}
		injector, err := credentials.NewInjector(resolver, grants, u.setup.SharedRegistry)
		if err != nil {
			return err
		}
		set, err := injector.Materialize(ctx, []string{string(capability.GitHubIssuesWrite)})
		if err != nil {
			return scrubTerminalError(u.setup.SharedRegistry, err)
		}
		repo := terminalRepositoryRefForProject(u.setup.Config, project)
		if string(repo.Provider) != entry.Provider {
			return fmt.Errorf("provider cleanup repository does not match the claim")
		}
		provider := providers.NewGiteaProvider(baseURL, "", providers.WithGiteaTokenSource(set.For(string(capability.GitHubIssuesWrite))))
		_, err = provider.ReleaseWorkItemClaim(ctx, providers.ClaimWorkItemRequest{
			Repository: repo, ID: entry.ItemID, RunID: entry.RunID, LedgerAuthorized: true, Reason: reason,
		})
		return scrubTerminalError(u.setup.SharedRegistry, err)
	}
	release, repo, err := buildTerminalClaimMarkerRelease(u.l.ForGaggle(identity.Gaggle), u.setup.Config, *identity.WorkspaceRepository, u.setup.SharedRegistry, u.setup.SecretStores)
	if err != nil {
		return err
	}
	if release == nil || string(repo.Provider) != entry.Provider {
		return fmt.Errorf("provider cleanup is unavailable for the claim repository")
	}
	_, err = release(ctx, providers.ClaimWorkItemRequest{
		Repository: repo, ID: entry.ItemID, RunID: entry.RunID,
		LedgerAuthorized: true, Reason: reason,
	})
	return err
}
