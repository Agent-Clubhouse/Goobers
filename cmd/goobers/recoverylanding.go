package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

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

func recoveryLandingHeads(ctx context.Context, runsRoot string, record recovery.Record, route string) ([]recovery.LandedHead, error) {
	directory, err := os.Open(runsRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(257)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > 256 {
		return nil, fmt.Errorf("recovery landing run scan exceeds budget")
	}
	var heads []recovery.LandedHead
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() || entry.Name() == record.RunID {
			continue
		}
		path := filepath.Join(runsRoot, entry.Name())
		reader, err := journal.OpenReadOnly(path)
		if err != nil {
			return nil, err
		}
		identity, err := reader.Identity()
		if err != nil {
			return nil, err
		}
		if identity.RunID != entry.Name() || identity.WorkspaceRepository == nil {
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
