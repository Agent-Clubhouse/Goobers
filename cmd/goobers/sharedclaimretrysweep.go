package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

type sharedVisibilitySweep struct {
	layout           instance.Layout
	cursors          map[string]string
	repositoryCursor string
}

// This loop is independent of lease renewal and holds no claims lock. Even an
// empty local ledger must not stop repair of a remote write that outlived it.
func startSharedVisibilityReconciler(ctx context.Context, layout instance.Layout, log *journal.InstanceLog) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		sweep := sharedVisibilitySweep{layout: layout, cursors: make(map[string]string)}
		reporter := newSweepErrorReporter(log, "shared_claim_visibility_reconciliation_failed")
		for {
			if ctx.Err() != nil {
				return
			}
			passCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			reporter.report(sweep.run(passCtx))
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

func (s *sharedVisibilitySweep) run(ctx context.Context) error {
	repos, inventoryErr := sharedVisibilityRepositories(s.layout)
	if len(repos) == 0 {
		return inventoryErr
	}
	cfg, err := instance.LoadConfig(s.layout.ConfigFile())
	if err != nil {
		return errors.Join(inventoryErr, err)
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return errors.Join(inventoryErr, err)
	}
	registry, _ := journal.DefaultScrubber()
	err = s.reconcile(ctx, repos, func(ctx context.Context, repo providers.RepositoryRef, cursor string) (string, error) {
		provider, err := daemonSharedClaimProvider(ctx, cfg, repo, registry, stores, capability.RepoPush)
		if err != nil {
			return cursor, err
		}
		labels, err := daemonSharedClaimVisibility(ctx, cfg, repo, registry, stores)
		if err != nil {
			return cursor, err
		}
		store := providers.GitHubSharedClaimStore{Provider: provider, Repository: repo}
		return store.ReconcileSharedVisibility(ctx, labels, cursor, 16)
	})
	return errors.Join(inventoryErr, scrubTerminalError(registry, err))
}

func (s *sharedVisibilitySweep) reconcile(ctx context.Context, repos []providers.RepositoryRef, visit func(context.Context, providers.RepositoryRef, string) (string, error)) error {
	if s.cursors == nil {
		s.cursors = make(map[string]string)
	}
	slices.SortFunc(repos, func(a, b providers.RepositoryRef) int { return strings.Compare(a.CanonicalKey(), b.CanonicalKey()) })
	var failures error
	attempted := 0
	for _, repo := range repos {
		key := repo.CanonicalKey()
		if key <= s.repositoryCursor {
			continue
		}
		if attempted == 4 {
			return failures
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		attempted++
		s.repositoryCursor = key
		itemCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		next, err := visit(itemCtx, repo, s.cursors[key])
		cancel()
		s.cursors[key] = next
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("shared visibility repository %s/%s: %w", repo.Owner, repo.Name, err))
		}
	}
	s.repositoryCursor = ""
	return failures
}
