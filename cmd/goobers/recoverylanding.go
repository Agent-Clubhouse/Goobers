package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

func recoveryLandingRoute(repo apiv1.RepoRef) (string, error) {
	base := strings.TrimRight(repo.BaseURL, "/")
	var parts []string
	switch repo.Provider {
	case apiv1.ProviderGitHub:
		if base == "" {
			base = "https://api.github.com"
		}
		parts = []string{"repos", strings.ToLower(repo.Owner), strings.ToLower(repo.Name)}
	case apiv1.ProviderGitea:
		if !strings.HasSuffix(base, "/api/v1") {
			base += "/api/v1"
		}
		parts = []string{"repos", strings.ToLower(repo.Owner), strings.ToLower(repo.Name)}
	case apiv1.ProviderADO:
		if base == "" {
			base = "https://dev.azure.com"
		}
		parts = []string{repo.Owner, repo.Project, "_apis", "git", "repositories", repo.Name}
	default:
		return "", fmt.Errorf("unsupported recovery landing provider")
	}
	return url.JoinPath(base, parts...)
}

// recoveryLandingScanBudget bounds how many run journals ONE landing scan
// opens. It was a hard ceiling on the runs directory itself (#4672): any
// instance that had ever held more than 256 runs failed every landing scan
// with "recovery landing run scan exceeds budget", once per recovery snapshot
// per sweep, so landing-proof retirement never ran again on exactly the
// long-lived instances that accumulate snapshots (#5943). The bound still caps
// the per-scan journal work; it no longer caps the instance's history.
const recoveryLandingScanBudget = 256

// recoveryLandingCursors makes an over-budget scan resumable (#5943). Each
// (runs root, recovery record) pair scans the next budget-sized window of the
// name-sorted run directories and advances, so successive sweeps cover every
// run while no single scan opens more than the budget. Partial coverage is
// safe by construction: a scan only ever PRODUCES landed heads, each of which
// must still pass VerifyLandedRestoration before anything is retired, so an
// unscanned run can delay a retirement but never cause one. The cursor is
// process memory: a restart (or a one-shot CLI sweep) starts from the first
// window again, which only delays coverage.
var recoveryLandingCursors = struct {
	sync.Mutex
	next map[string]int
}{next: map[string]int{}}

// recoveryLandingCursorLimit bounds the cursor map. Keys are per snapshot, so
// the map grows only with snapshots seen over the process lifetime; clearing
// it merely restarts every rotation.
const recoveryLandingCursorLimit = 4096

// recoveryLandingWindow returns the run directories this scan should read:
// all of them when they fit the budget, otherwise the next rotating window.
func recoveryLandingWindow(runsRoot, recordRunID string, candidates []string) []string {
	if len(candidates) <= recoveryLandingScanBudget {
		return candidates
	}
	key := runsRoot + "\x00" + recordRunID
	recoveryLandingCursors.Lock()
	if len(recoveryLandingCursors.next) >= recoveryLandingCursorLimit {
		clear(recoveryLandingCursors.next)
	}
	start := recoveryLandingCursors.next[key] % len(candidates)
	recoveryLandingCursors.next[key] = (start + recoveryLandingScanBudget) % len(candidates)
	recoveryLandingCursors.Unlock()
	window := make([]string, 0, recoveryLandingScanBudget)
	for i := 0; i < recoveryLandingScanBudget; i++ {
		window = append(window, candidates[(start+i)%len(candidates)])
	}
	return window
}

// recoveryLandingHeads scans the instance's run journals for a landing receipt
// of record's content. os.ReadDir sorts by name, so a rotating window is
// stable across passes apart from runs created or removed in between, which
// can shift one window's edge and are picked up on a later rotation.
func recoveryLandingHeads(ctx context.Context, runsRoot string, record recovery.Record, route string) ([]recovery.LandedHead, error) {
	entries, err := os.ReadDir(runsRoot)
	if err != nil {
		return nil, err
	}
	candidates := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != record.RunID {
			candidates = append(candidates, entry.Name())
		}
	}
	var heads []recovery.LandedHead
	for _, name := range recoveryLandingWindow(runsRoot, record.RunID, candidates) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Join(runsRoot, name)
		reader, err := journal.OpenReadOnly(path)
		if err != nil {
			return nil, err
		}
		identity, err := reader.Identity()
		if err != nil {
			return nil, err
		}
		if identity.RunID != name || identity.WorkspaceRepository == nil {
			continue
		}
		repo := identity.WorkspaceRepository
		key := providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), URL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}.CanonicalKey()
		if key != record.RepositoryKey {
			continue
		}
		// A receiving writer may still be projecting a conflicting receipt.
		_, err = journal.WithIdleRunReader(ctx, path, func(idle *journal.Reader) error {
			matched, err := readRecoveryLandingJournal(ctx, idle, path, string(repo.Provider), route)
			heads = append(heads, matched...)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return heads, nil
}

func readRecoveryLandingJournal(ctx context.Context, reader *journal.Reader, path, provider, route string) ([]recovery.LandedHead, error) {
	phase, err := reader.PhaseBounded(ctx)
	if err != nil || !terminalRunPhase(phase) {
		return nil, err
	}
	info, err := os.Lstat(filepath.Join(path, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return nil, fmt.Errorf("recovery landing requires a regular journal within the evidence budget")
	}
	events, err := reader.Events()
	if err != nil {
		return nil, err
	}
	var intents []providers.LandingIntent
	var confirmations []providers.MergeConfirmation
	for _, event := range events {
		if !event.IsReferenceTouch() || event.Error != nil || event.ExternalRef.Provider != provider || event.ExternalRef.Kind != "pr" {
			continue
		}
		data, err := json.Marshal(event.Runner)
		if err != nil {
			return nil, err
		}
		var receipt struct {
			Intent       *providers.LandingIntent     `json:"landingIntent"`
			Confirmation *providers.MergeConfirmation `json:"mergeConfirmation"`
		}
		if err := json.Unmarshal(data, &receipt); err != nil {
			return nil, err
		}
		if receipt.Intent != nil && receipt.Intent.PullID == event.ExternalRef.ID {
			intents = append(intents, *receipt.Intent)
		}
		if receipt.Confirmation != nil && receipt.Confirmation.PullID == event.ExternalRef.ID {
			confirmations = append(confirmations, *receipt.Confirmation)
		}
	}
	return recovery.MatchLandedHeads(route, intents, confirmations)
}
