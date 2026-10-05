package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func (c prRepairCustodian) runs(ctx context.Context, target providers.RepositoryRef, owners []trackedRun) error {
	roots, err := c.layout.RunDirsContext(ctx)
	if err != nil {
		return err
	}
	if len(roots) > 64 || len(owners) > 4096 {
		return errors.New("PR repair run inventory exceeds bounds")
	}
	live := map[string]bool{}
	for _, owner := range owners {
		live[owner.RunID] = true
	}
	remaining := 4096
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := os.Open(root)
		if err != nil {
			return err
		}
		entries, readErr := file.ReadDir(remaining + 1)
		closeErr := file.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		remaining -= len(entries)
		if remaining < 0 {
			return errors.New("PR repair run inventory exceeds bounds")
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			id, err := c.inspectRun(ctx, filepath.Join(root, entry.Name()), target, live)
			if err != nil {
				return err
			}
			delete(live, id)
		}
	}
	if len(live) > 0 {
		return errors.New("PR repair live owner journal is not yet verifiable")
	}
	return nil
}
func (c prRepairCustodian) inspectRun(ctx context.Context, directory string, target providers.RepositoryRef, owners map[string]bool) (string, error) {
	rd, err := journal.OpenReadOnly(directory)
	if err != nil {
		return "", err
	}
	id, err := rd.Identity()
	if err != nil {
		return "", err
	}
	if id.RunID == c.runID {
		return id.RunID, nil
	}
	// Another session has no arbitrary repository writer. Its host operations
	// serialize through claims.lock and native command physical-target custody.
	if id.Session != nil && id.ValidateSessionLineage() == nil {
		return id.RunID, nil
	}
	phase, err := rd.PhaseBounded(ctx)
	if err != nil {
		return "", err
	}
	if !owners[id.RunID] && (phase == journal.PhaseCompleted || phase == journal.PhaseFailed || phase == journal.PhaseAborted || phase == journal.PhaseEscalated) {
		return id.RunID, nil
	}
	if id.WorkspaceRepository == nil || repairRepoMatches(*id.WorkspaceRepository, target) {
		return "", errors.New("PR repair repository still has a nonterminal automation run")
	}
	// Additional provider/repository grants may name a target outside the main
	// checkout. Verify the retained configuration, never today's renamed catalog.
	if id.ConfigGeneration == "" {
		return "", errors.New("PR repair active run source authority is not pinned")
	}
	defs, release, err := pinnedCredentialDefinitions(ctx, c.layout, id.ConfigGeneration)
	if err != nil {
		return "", err
	}
	defer release()
	scope, ok := defs.Scopes[id.Gaggle]
	if !ok {
		return "", errors.New("PR repair active run source authority is missing")
	}
	if repairRepoMatches(scope.Project, target) {
		return "", errors.New("PR repair repository still has automation authority")
	}
	for _, ref := range scope.AdditionalRepos {
		if repairRepoMatches(ref, target) {
			return "", errors.New("PR repair additional repository still has automation authority")
		}
	}
	return id.RunID, nil
}
