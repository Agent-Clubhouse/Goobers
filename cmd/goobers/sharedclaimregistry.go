package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const sharedRepositoryPrefix = "shared-claim-repo-"

type sharedVisibilityRegistration struct {
	Version    int                     `json:"version"`
	Repository providers.RepositoryRef `json:"repository"`
}

func sharedRepositoryFilename(repo providers.RepositoryRef) string {
	digest := sha256.Sum256([]byte(repo.CanonicalKey()))
	return sharedRepositoryPrefix + hex.EncodeToString(digest[:]) + ".json"
}

// Registrations contain repository identity, never credentials or ownership.
// They survive release, run retention, and configuration changes. Concurrent
// identical registrations use independent staging files and atomic replacement.
func registerSharedVisibilityRepository(ctx context.Context, layout instance.Layout, repo providers.RepositoryRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validSharedVisibilityRepository(repo) {
		return fmt.Errorf("invalid shared visibility repository")
	}
	data, err := json.Marshal(sharedVisibilityRegistration{Version: 1, Repository: repo})
	if err != nil {
		return err
	}
	if len(data) > 4096 {
		return fmt.Errorf("shared visibility registration exceeds bound")
	}
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		return err
	}
	return journal.WriteFileAtomic(filepath.Join(layout.SchedulerDir(), sharedRepositoryFilename(repo)), data, 0o600)
}

func sharedVisibilityRepositories(layout instance.Layout) ([]providers.RepositoryRef, error) {
	directory, err := os.Open(layout.SchedulerDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(4097)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 4096 {
		return nil, fmt.Errorf("shared visibility registry directory exceeds bound")
	}
	var repos []providers.RepositoryRef
	var failures error
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), sharedRepositoryPrefix) {
			continue
		}
		repo, err := readSharedVisibilityRegistration(layout, entry)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		repos = append(repos, repo)
	}
	return repos, failures
}

func readSharedVisibilityRegistration(layout instance.Layout, entry os.DirEntry) (providers.RepositoryRef, error) {
	var registration sharedVisibilityRegistration
	if !entry.Type().IsRegular() {
		return registration.Repository, fmt.Errorf("shared visibility registration is not a regular file")
	}
	f, err := os.Open(filepath.Join(layout.SchedulerDir(), entry.Name()))
	if err != nil {
		return registration.Repository, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return registration.Repository, err
	}
	if len(data) > 4096 {
		return registration.Repository, fmt.Errorf("shared visibility registration exceeds bound")
	}
	if err := json.Unmarshal(data, &registration); err != nil {
		return registration.Repository, fmt.Errorf("invalid shared visibility registration")
	}
	canonical, err := json.Marshal(registration)
	repo := registration.Repository
	if err != nil || !bytes.Equal(canonical, data) || registration.Version != 1 || !validSharedVisibilityRepository(repo) || sharedRepositoryFilename(repo) != entry.Name() {
		return providers.RepositoryRef{}, fmt.Errorf("shared visibility registration identity mismatch")
	}
	return repo, nil
}

func validSharedVisibilityRepository(repo providers.RepositoryRef) bool {
	if repo.Provider != providers.ProviderGitHub || repo.Owner == "" || repo.Name == "" {
		return false
	}
	if repo.URL == "" {
		return true
	}
	u, err := url.Parse(repo.URL)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
