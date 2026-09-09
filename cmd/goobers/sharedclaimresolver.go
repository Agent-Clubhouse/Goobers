package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

// pinnedSharedClaimResolver is the common trusted policy boundary for file and
// daemon claim clients. Provider credentials are supplied by the assembly, not
// by an incoming claim request. Repository routing comes from the run pin.
type pinnedSharedClaimResolver struct {
	layout instance.Layout
	store  func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error)
}

func (r pinnedSharedClaimResolver) Admission(ctx context.Context, key claimsclient.Key, runID, workflow string) (*claimsclient.SharedClaimBinding, error) {
	reader, identity, mode, err := r.claimPolicy(key, runID, workflow)
	if err != nil || mode == "local" {
		return nil, err
	}
	phase, err := reader.PhaseBounded(ctx)
	if err != nil {
		return nil, err
	}
	if terminalRunPhase(phase) {
		return nil, fmt.Errorf("terminal run cannot acquire a shared claim")
	}
	binding, err := r.binding(ctx, key, identity, sharedclaim.Owner{})
	return &binding, err
}

func (r pinnedSharedClaimResolver) Release(ctx context.Context, entry claimsclient.Entry) (claimsclient.SharedClaimBinding, error) {
	key := claimsclient.KeyForEntry(entry)
	_, identity, mode, err := r.claimPolicy(key, entry.RunID, entry.Workflow)
	if err != nil {
		return claimsclient.SharedClaimBinding{}, err
	}
	if mode != "shared" {
		return claimsclient.SharedClaimBinding{}, fmt.Errorf("shared release does not match the pinned workflow policy")
	}
	// Do not require a live phase here: terminal cleanup is an owning release.
	// Recompute the incarnation from independent persisted run evidence; never
	// copy a caller's token into a binding that could authorize provider writes.
	if entry.SharedOwner == (sharedclaim.Owner{}) {
		return claimsclient.SharedClaimBinding{}, fmt.Errorf("shared release requires persisted ownership")
	}
	return r.binding(ctx, key, identity, entry.SharedOwner)
}

func (r pinnedSharedClaimResolver) claimPolicy(key claimsclient.Key, runID, workflow string) (*journal.Reader, journal.RunIdentity, string, error) {
	directory, err := runDirFor(r.layout, runID)
	if err != nil {
		return nil, journal.RunIdentity{}, "", err
	}
	reader, err := journal.OpenReadOnly(directory)
	if err != nil {
		return nil, journal.RunIdentity{}, "", err
	}
	identity, err := reader.Identity()
	if err != nil {
		return nil, identity, "", err
	}
	if identity.RunID != runID || identity.Workflow != workflow || identity.Gaggle != key.Gaggle {
		return nil, identity, "", fmt.Errorf("claim request does not match the pinned run identity")
	}
	mode, err := pinnedClaimVisibility(reader, identity, providers.ProviderKind(key.Provider))
	return reader, identity, mode, err
}

func (r pinnedSharedClaimResolver) binding(ctx context.Context, key claimsclient.Key, identity journal.RunIdentity, expected sharedclaim.Owner) (claimsclient.SharedClaimBinding, error) {
	pinned := identity.WorkspaceRepository
	if pinned == nil || pinned.Owner == "" || pinned.Name == "" || string(pinned.Provider) != key.Provider {
		return claimsclient.SharedClaimBinding{}, fmt.Errorf("shared claim requires a matching pinned repository")
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderKind(pinned.Provider), URL: pinned.BaseURL, Owner: pinned.Owner, Project: pinned.Project, Name: pinned.Name}
	encoded, err := json.Marshal([]string{repo.CanonicalKey(), key.ExternalID})
	if err != nil {
		return claimsclient.SharedClaimBinding{}, err
	}
	instanceID, err := instance.ReadRootIdentity(r.layout.Root)
	if err != nil {
		return claimsclient.SharedClaimBinding{}, err
	}
	owner, err := sharedClaimOwnerFromIdentity(instanceID, identity, string(encoded))
	if err != nil {
		return claimsclient.SharedClaimBinding{}, err
	}
	if expected != (sharedclaim.Owner{}) && owner != expected {
		return claimsclient.SharedClaimBinding{}, fmt.Errorf("shared release incarnation differs from the pinned run")
	}
	if r.store == nil {
		return claimsclient.SharedClaimBinding{}, fmt.Errorf("shared claim provider is not configured")
	}
	store, err := r.store(ctx, repo)
	if err != nil {
		return claimsclient.SharedClaimBinding{}, err
	}
	if store == nil {
		return claimsclient.SharedClaimBinding{}, fmt.Errorf("shared claim provider is unavailable")
	}
	// The store is already repository-scoped. Including the configured API
	// URL in its record key would split one GitHub item into two leases when
	// one instance spells the default host explicitly and another omits it.
	// The owner token still binds the repository as well as the item.
	return claimsclient.SharedClaimBinding{Store: store, RemoteKey: key.ExternalID, Owner: owner}, nil
}
