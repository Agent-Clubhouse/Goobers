package terminalwriter

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

// sharedBackend is deliberately separate from Backend: providers without
// revision-CAS coordination must not accidentally emulate it with labels.
type sharedBackend interface {
	claimStore(providers.RepositoryRef) providers.GitHubSharedClaimStore
	claimVisibility(providers.RepositoryRef) sharedclaim.Visibility
}

type githubRESTBackend struct {
	restBackend
	provider *providers.GitHubProvider
}

func (b githubRESTBackend) claimStore(repo providers.RepositoryRef) providers.GitHubSharedClaimStore {
	return providers.GitHubSharedClaimStore{Provider: b.provider, Repository: repo}
}
func (b githubRESTBackend) claimVisibility(repo providers.RepositoryRef) sharedclaim.Visibility {
	return providers.GitHubSharedClaimVisibility{Provider: b.provider, Repository: repo}
}

var _ providerwriter.SharedClaims = (*Writer)(nil)
var _ providerwriter.SharedVisibilityReconciler = (*InstanceWriter)(nil)

func (w *Writer) sharedBackend(ctx context.Context, runID string, repo providers.RepositoryRef, requested capability.Capability) (sharedBackend, error) {
	target, err := w.resolve(ctx, runID, repo)
	if err != nil {
		return nil, w.fail("shared claim binding", err)
	}
	// The general terminal surface accepts omitted hosts. Shared coordination
	// must match the complete identity: different hosts must never share auth.
	if repo.Provider != providers.ProviderGitHub || repo.CanonicalKey() != target.bound.Workspace.CanonicalKey() {
		return nil, w.fail("shared claim binding", providerwriter.ErrRepositoryNotPinned)
	}
	backend, err := w.backends.For(ctx, target.bound, repo, string(requested))
	if err != nil {
		return nil, w.fail("shared claim credentials", err)
	}
	shared, ok := backend.(sharedBackend)
	if !ok {
		return nil, w.fail("shared claim backend", ErrUnsupported)
	}
	return shared, nil
}

// SharedClaimStore binds the run's pinned GitHub repository with repo:push
// credentials; no issue-write credential can mutate coordination authority.
func (w *Writer) SharedClaimStore(ctx context.Context, runID string, repo providers.RepositoryRef) (sharedclaim.Store, error) {
	backend, err := w.sharedBackend(ctx, runID, repo, capability.RepoPush)
	if err != nil {
		return nil, err
	}
	return scrubbedSharedStore{store: backend.claimStore(repo), scrubber: w.scrubber}, nil
}

// SharedClaimVisibility binds only the claimed-label projection, independently
// scoped to issues:write. A failure does not change admission authority.
func (w *Writer) SharedClaimVisibility(ctx context.Context, runID string, repo providers.RepositoryRef) (sharedclaim.Visibility, error) {
	backend, err := w.sharedBackend(ctx, runID, repo, capability.GitHubIssuesWrite)
	if err != nil {
		return nil, err
	}
	return scrubbedSharedVisibility{labels: backend.claimVisibility(repo), scrubber: w.scrubber}, nil
}

type scrubbedSharedStore struct {
	store    sharedclaim.Store
	scrubber journal.Scrubber
}

func (s scrubbedSharedStore) Read(ctx context.Context, key string) (sharedclaim.Observation, error) {
	observed, err := s.store.Read(ctx, key)
	return observed, sharedFailure(s.scrubber, err)
}
func (s scrubbedSharedStore) CompareAndSwap(ctx context.Context, key, revision string, record sharedclaim.Record) error {
	return sharedFailure(s.scrubber, s.store.CompareAndSwap(ctx, key, revision, record))
}
func sharedFailure(scrubber journal.Scrubber, err error) error {
	if err == nil {
		return nil
	}
	// failWith intentionally strips provider error text from its unwrap chain.
	// Preserve protocol identities separately so release can confirm a successor.
	var kinds []error
	for _, kind := range []error{sharedclaim.ErrConflict, sharedclaim.ErrNotOwner, sharedclaim.ErrHeld, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, kind) {
			kinds = append(kinds, kind)
		}
	}
	kinds = append(kinds, failWith(scrubber, "shared claim", err))
	return errors.Join(kinds...)
}

type scrubbedSharedVisibility struct {
	labels   sharedclaim.Visibility
	scrubber journal.Scrubber
	before   func(string, bool) error
}

func (v scrubbedSharedVisibility) ReadClaimed(ctx context.Context, key string) (bool, error) {
	present, err := v.labels.ReadClaimed(ctx, key)
	return present, sharedFailure(v.scrubber, err)
}
func (v scrubbedSharedVisibility) SetClaimed(ctx context.Context, key string, present bool) error {
	if v.before != nil {
		if err := v.before(key, present); err != nil {
			return err
		}
	}
	return sharedFailure(v.scrubber, v.labels.SetClaimed(ctx, key, present))
}

// ReconcileSharedVisibility repairs registered repositories after run history
// expires. Like escalation, every label write first appends to the epoch's
// fenced journal. The only possible mutation is the shared claimed label.
func (w *InstanceWriter) ReconcileSharedVisibility(ctx context.Context, repo providers.RepositoryRef, cursor string, limit int) (string, error) {
	set, digest, ok := w.applied()
	if !ok {
		return "", ErrNoAppliedConfig
	}
	bound, ok := sharedInstanceBinding(set, digest, repo)
	if !ok || repo.Provider != providers.ProviderGitHub || bound.Workspace.CanonicalKey() != repo.CanonicalKey() {
		return "", failWith(w.scrubber, "shared visibility binding", providerwriter.ErrRepositoryNotPinned)
	}
	storeBackend, err := w.backends.For(ctx, bound, repo, string(capability.RepoPush))
	if err != nil {
		return "", sharedFailure(w.scrubber, err)
	}
	labelBackend, err := w.backends.For(ctx, bound, repo, string(capability.GitHubIssuesWrite))
	if err != nil {
		return "", sharedFailure(w.scrubber, err)
	}
	store, storeOK := storeBackend.(sharedBackend)
	labels, labelOK := labelBackend.(sharedBackend)
	if !storeOK || !labelOK {
		return "", fmt.Errorf("shared visibility: %w", ErrUnsupported)
	}
	visible := scrubbedSharedVisibility{labels: labels.claimVisibility(repo), scrubber: w.scrubber, before: func(key string, present bool) error {
		return w.Journal(journal.Event{Type: journal.EventRunnerAnnotation, Reason: "shared claim visibility reconciliation",
			Runner: map[string]any{"kind": "shared_claim_visibility", "repository": repo.CanonicalKey(), "itemId": key, "claimed": present}})
	}}
	next, err := store.claimStore(repo).ReconcileSharedVisibility(ctx, visible, cursor, limit)
	return next, sharedFailure(w.scrubber, err)
}

// sharedInstanceBinding matches workspace identity including host. Shared
// coordination lives in that repository even when its backlog is elsewhere.
func sharedInstanceBinding(set *instance.ConfigSet, digest string, repo providers.RepositoryRef) (Binding, bool) {
	if set == nil {
		return Binding{}, false
	}
	for _, gaggle := range set.Gaggles {
		project := runtimewiring.GaggleProjectRef(set, gaggle.Name)
		workspace := repositoryRef(project)
		if workspace.CanonicalKey() == repo.CanonicalKey() {
			return Binding{Gaggle: gaggle.Name, Generation: digest, Project: project, Workspace: workspace, WorkItems: workspace}, true
		}
	}
	return Binding{}, false
}
