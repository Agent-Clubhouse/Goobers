package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// claimMarkerReleaseErrorCode attributes a best-effort provider claim-epoch
// release that did not land, so `cat scheduler/events.jsonl` explains why an
// item is still wearing goobers:claimed while the ledger says released —
// exactly the divergence #3347 was filed about, now observable instead of
// silent until the next curation cycle repairs it.
const claimMarkerReleaseErrorCode = "claim_marker_release_failed"

// terminalClaimMarkerTimeout bounds the whole provider-side claim-epoch
// release for one terminal run (every item it still holds), not one call.
// Deliberately generous — terminal cleanup is off the run's critical path and
// a slow forge must not truncate cleanup into the divergence this exists to
// close — but bounded, because it runs inline in the daemon's terminal
// transition. A var so tests can shrink it.
var terminalClaimMarkerTimeout = 60 * time.Second

// workItemClaimReleaser is the narrow seam buildTerminalClaimMarkerRelease
// needs — just enough of providers.BacklogProvider to swap in a fake in tests,
// mirroring newTerminalBranchDeleter's providers.BranchDeleter seam and
// newRunAbortLabelProvider's workItemUpdater one.
type workItemClaimReleaser interface {
	ReleaseWorkItemClaim(context.Context, providers.ClaimWorkItemRequest) (providers.WorkItem, error)
}

// claimMarkerReleaseFunc ends one item's provider-visible claim epoch: the
// release breadcrumb that reopens the item to future claimants plus the
// goobers:claimed label mirror.
type claimMarkerReleaseFunc func(ctx context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error)

var newTerminalClaimMarkerProvider = func(source providers.TokenSource) workItemClaimReleaser {
	return providers.NewGitHubProvider("", providers.WithTokenSource(source))
}

// buildTerminalClaimMarkerRelease mirrors buildTerminalRunAbortLabeler's shape
// — the same credential/capability wiring and per-gaggle project scoping — but
// github:issues:write (the ordinary label/comment surface, and a daemon-identity
// capability, so the release is attributed to the same bot login that wrote the
// claim) rather than github:pr:write.
//
// Returns a nil func for a repo-less instance (the credential-free demo) and for
// any non-GitHub configured provider: ADO and Gitea claim markers keep relying on
// backlog curation's reconciliation pass exactly as they do today, which is
// unchanged behavior rather than a regression. The returned RepositoryRef is the
// repo the release targets; on GitHub the backlog and the code repo coincide
// (backlogRepoRefForGaggle is an ADO-only rewrite), so no further routing applies.
func buildTerminalClaimMarkerRelease(cfg *instance.Config, project apiv1.RepoRef, registrar terminalSecretRegistry, stores credentials.StoreResolver) (claimMarkerReleaseFunc, providers.RepositoryRef, error) {
	if len(cfg.Repos) == 0 {
		return nil, providers.RepositoryRef{}, nil
	}
	repo := providers.RepositoryRef{
		Provider: providers.ProviderKind(cfg.Repos[0].Provider),
		Owner:    cfg.Repos[0].Owner,
		Name:     cfg.Repos[0].Name,
	}
	if project.Owner != "" && project.Name != "" {
		repo.Owner, repo.Name = project.Owner, project.Name
		// A gaggle project may name its repo without restating the provider
		// (runnerwiring.go's single-repo inference); the configured repo's own
		// provider is the fallback, never an implicit GitHub.
		if project.Provider != "" {
			repo.Provider = providers.ProviderKind(project.Provider)
		}
	}
	if repo.Provider != providers.ProviderGitHub {
		return nil, providers.RepositoryRef{}, nil
	}
	gaggleOwner := project.Owner
	resolver, grants, err := buildCredentials(cfg, stores, gaggleOwner, project.Name, nil, registrar)
	if err != nil {
		return nil, providers.RepositoryRef{}, err
	}
	injector, err := credentials.NewInjector(resolver, grants, registrar)
	if err != nil {
		return nil, providers.RepositoryRef{}, fmt.Errorf("build terminal claim-marker credentials: %w", err)
	}
	release := func(ctx context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
		set, err := injector.Materialize(ctx, []string{string(capability.GitHubIssuesWrite)})
		if err != nil {
			return providers.WorkItem{}, scrubTerminalError(registrar, err)
		}
		item, err := newTerminalClaimMarkerProvider(set.For(string(capability.GitHubIssuesWrite))).ReleaseWorkItemClaim(ctx, req)
		return item, scrubTerminalError(registrar, err)
	}
	return release, repo, nil
}

// releaseTerminalClaimMarkers ends the provider-visible claim epoch for every
// backlog item a terminal run STILL holds in the claim ledger (#3347).
//
// The ledger is the truth and the provider marker only mirrors it (BL-005), but
// until now the mirror was only ever cleared by a stage — issue-close-out, or
// backlog-query --release — and a run that terminates without reaching one never
// cleared it. The first-class `no-work` outcome makes that a certainty rather
// than an edge case: it short-circuits straight to PhaseCompleted from whatever
// stage reported it (taskOutcome, internal/runner/run.go), so close-out never
// executes, and the goobers:claimed label survived until the NEXT
// backlog-curation cycle reconciled it — 3.5 minutes at a 5-minute cadence in
// the pinned live evidence, and indefinitely if curation is disabled, slow, or
// failing. Anything reading labels in that window sees a claimed-but-released
// item and the claimable pool silently shrinks.
//
// Called immediately BEFORE the ledger release (the same order issue-close-out
// and backlog-query --release use), and deliberately not under the claims lock:
// the lock is instance-global and this makes network calls. Holding the ledger
// claim across the provider call is what makes the release safe without it — a
// new owner cannot appear for an item this run still holds, so the epoch being
// closed is still this run's. If a recovery sweep does race in and hand the item
// to a new run first, providers.ReleaseWorkItemClaim's own guard refuses
// (LedgerAuthorized is deliberately left false here, unlike backlog-query
// --release's ledger-authorized path): the new owner's epoch is not ours to end.
//
// Best-effort by construction. Every failure is journaled and swallowed rather
// than returned, because the ledger release below is what actually frees the
// item — the pre-existing curation reconciliation remains the backstop for a
// marker that could not be cleared, so the worst case is exactly today's
// behavior.
func releaseTerminalClaimMarkers(l instance.Layout, log *journal.InstanceLog, runID string, repo providers.RepositoryRef, release claimMarkerReleaseFunc) {
	if release == nil {
		return
	}
	entries, err := terminalClaimMarkerEntries(l, runID)
	if err != nil {
		// A claims-lock timeout is already journaled as such by withClaimLock;
		// the run's ledger release below defers to the recovery sweep on the
		// same timeout, and so does this.
		if !isJournaledClaimsLockTimeout(err) {
			recordClaimMarkerReleaseError(log, localscheduler.ClaimEntry{RunID: runID, Gaggle: l.Gaggle()}, err)
		}
		return
	}
	if len(entries) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminalClaimMarkerTimeout)
	defer cancel()
	for _, entry := range entries {
		if _, err := release(ctx, providers.ClaimWorkItemRequest{
			Repository: repo,
			ID:         claimEntryExternalID(entry),
			RunID:      runID,
		}); err != nil {
			recordClaimMarkerReleaseError(log, entry, err)
		}
	}
}

// terminalClaimMarkerEntries reads the claims a terminal run still holds that
// have a provider-visible marker to retire.
//
// Skips pull-request claims (pullRequestClaimPrefix): pr-claim keys the ledger
// by pr/<number> and never writes a provider claim marker at all, so releasing
// one would post a claim-release breadcrumb onto a PR that never carried a claim.
// Skips claims from a different provider than the one this instance's terminal
// cleanup is credentialed for.
func terminalClaimMarkerEntries(l instance.Layout, runID string) ([]localscheduler.ClaimEntry, error) {
	var entries []localscheduler.ClaimEntry
	err := withClaimLockForRun(
		filepath.Join(l.SchedulerDir(), claimLockFileName),
		claimLockOperationRunLookup,
		l.Gaggle(),
		runID,
		func() error {
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
			if err != nil {
				return fmt.Errorf("open claim ledger: %w", err)
			}
			for _, entry := range ledger.ForRunAll(runID) {
				if strings.HasPrefix(entry.ItemID, pullRequestClaimPrefix) {
					continue
				}
				if entry.Provider != "" && entry.Provider != string(providers.ProviderGitHub) {
					continue
				}
				entries = append(entries, entry)
			}
			return nil
		},
	)
	return entries, err
}

// claimEntryExternalID is the provider-facing item id of a ledger entry: the
// scoped ExternalID when present, the bare ItemID for a legacy unscoped entry
// (the same precedence selectClaimForRelease applies).
func claimEntryExternalID(entry localscheduler.ClaimEntry) string {
	if entry.ExternalID != "" {
		return entry.ExternalID
	}
	return entry.ItemID
}

func recordClaimMarkerReleaseError(log *journal.InstanceLog, entry localscheduler.ClaimEntry, err error) {
	if log == nil {
		return
	}
	_ = log.Append(journal.Event{
		Type:     journal.EventError,
		Name:     entry.ItemID,
		Gaggle:   entry.Gaggle,
		Workflow: entry.Workflow,
		RunID:    entry.RunID,
		Error: &journal.ErrorDetail{
			Code:    claimMarkerReleaseErrorCode,
			Message: err.Error(),
		},
		Runner: map[string]any{"operation": claimLockOperationRunRelease},
	})
}
