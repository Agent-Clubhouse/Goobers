package sharedclaims

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/cloud/terminalwriter"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

// Resolver reads the digest-verified workflow snapshot in the PG journal.
// Runs additionally checks the repository against the control-held start pin.
// Credentials and all provider writes belong to Writer, never an HTTP request.
type Resolver struct {
	InstanceID string
	Journal    terminalwriter.RunOpener
	// Verify checks workflow identity against the run's own control-held start pin.
	Verify   func(context.Context, journal.RunIdentity) error
	Runs     terminalwriter.Runs
	Writer   providerwriter.SharedClaims
	Register func(context.Context, providers.RepositoryRef) error
	Report   func(error)
}

// Visibility returns the run's immutable mode for trusted execution snapshots.
func (r Resolver) Visibility(ctx context.Context, runID string) (string, error) {
	_, _, mode, err := r.policy(ctx, runID)
	return mode, err
}

func (r Resolver) policy(ctx context.Context, runID string) (journal.RunReader, journal.RunIdentity, string, error) {
	if r.Journal == nil || r.InstanceID == "" || r.Verify == nil {
		return nil, journal.RunIdentity{}, "", errors.New("shared policy requires a journal, instance and control-held workflow pins")
	}
	reader, err := r.Journal.OpenRun(ctx, journal.RunRef{ID: runID, ReadOnly: true})
	if err != nil {
		return nil, journal.RunIdentity{}, "", err
	}
	identity, err := reader.Identity()
	if err != nil {
		return nil, identity, "", err
	}
	if identity.RunID != runID || identity.InstanceID != r.InstanceID {
		return nil, identity, "", errors.New("claim policy belongs to another run or instance")
	}
	if err := r.Verify(ctx, identity); err != nil {
		return nil, identity, "", err
	}
	machine, err := runsemantics.PinnedWorkflowMachineFrom(reader, identity)
	if err != nil {
		return nil, identity, "", fmt.Errorf("claim visibility requires a verified workflow pin: %w", err)
	}
	if machine.Def.Spec.Gaggle != identity.Gaggle {
		return nil, identity, "", errors.New("claim workflow pin belongs to another gaggle")
	}
	mode := machine.Def.Spec.Readiness.ClaimVisibility
	switch mode {
	case "", "local":
		return reader, identity, "local", nil
	case "shared":
		if identity.WorkspaceRepository == nil || string(identity.WorkspaceRepository.Provider) != string(providers.ProviderGitHub) {
			return nil, identity, "", errors.New("shared claims require a pinned GitHub repository")
		}
		return reader, identity, mode, nil
	default:
		return nil, identity, "", fmt.Errorf("unsupported pinned claim visibility %q", mode)
	}
}

// Admission refuses terminal or mismatched run identities before any remote I/O.
func (r Resolver) Admission(ctx context.Context, key claimsclient.Key, runID, workflow string) (*claimsclient.SharedClaimBinding, error) {
	reader, identity, policyRun, mode, err := r.claimPolicy(ctx, key, runID, workflow)
	if err != nil || mode == "local" {
		return nil, err
	}
	phase, err := reader.PhaseBounded(ctx)
	if err != nil {
		return nil, err
	}
	if phase.IsTerminal() {
		return nil, errors.New("terminal run cannot acquire a shared claim")
	}
	binding, err := r.binding(ctx, key, identity, policyRun)
	return &binding, err
}

// Release binds persisted ownership even after the run becomes terminal.
func (r Resolver) Release(ctx context.Context, entry claimsclient.Entry) (claimsclient.SharedClaimBinding, error) {
	key := claimsclient.KeyForEntry(entry)
	_, identity, policyRun, mode, err := r.claimPolicy(ctx, key, entry.RunID, entry.Workflow)
	if err != nil {
		return claimsclient.SharedClaimBinding{}, err
	}
	if mode != "shared" || entry.SharedOwner == (sharedclaim.Owner{}) {
		return claimsclient.SharedClaimBinding{}, errors.New("shared release requires pinned shared ownership")
	}
	binding, err := r.binding(ctx, key, identity, policyRun)
	if err == nil && binding.Owner != entry.SharedOwner {
		return claimsclient.SharedClaimBinding{}, errors.New("shared release differs from the pinned incarnation")
	}
	return binding, err
}

func (r Resolver) claimPolicy(ctx context.Context, key claimsclient.Key, runID, workflow string) (journal.RunReader, journal.RunIdentity, string, string, error) {
	policyRun := runID
	auxiliary := false
	if owner, ok := reconciliationRun(runID); ok && workflow == "backlog-reconcile" {
		policyRun, auxiliary = owner, true
	}
	reader, identity, mode, err := r.policy(ctx, policyRun)
	if err != nil {
		return nil, identity, "", "", err
	}
	target := workflow == "decomposition-target-lease" && strings.HasPrefix(key.ExternalID, localscheduler.DecompositionTargetItemIDPrefix) &&
		strings.TrimPrefix(key.ExternalID, localscheduler.DecompositionTargetItemIDPrefix) != ""
	if identity.Gaggle != key.Gaggle || (!auxiliary && !target && identity.Workflow != workflow) ||
		(mode == "shared" && key.Provider != string(providers.ProviderGitHub)) {
		return nil, identity, "", "", errors.New("claim request differs from the pinned run identity")
	}
	identity.RunID = runID
	return reader, identity, policyRun, mode, nil
}

func (r Resolver) binding(ctx context.Context, key claimsclient.Key, identity journal.RunIdentity, policyRun string) (claimsclient.SharedClaimBinding, error) {
	if r.Runs == nil || r.Writer == nil || r.Register == nil {
		return claimsclient.SharedClaimBinding{}, errors.New("shared claims require the control provider writer and repository registry")
	}
	bound, err := r.Runs.Bind(ctx, policyRun)
	if err != nil {
		return claimsclient.SharedClaimBinding{}, err
	}
	pinned := identity.WorkspaceRepository
	repo := providers.RepositoryRef{Provider: providers.ProviderKind(pinned.Provider), URL: pinned.BaseURL, Owner: pinned.Owner, Project: pinned.Project, Name: pinned.Name}
	if repo.CanonicalKey() != bound.Workspace.CanonicalKey() || bound.Gaggle != identity.Gaggle {
		return claimsclient.SharedClaimBinding{}, providerwriter.ErrRepositoryNotPinned
	}
	encoded, err := json.Marshal([]string{repo.CanonicalKey(), key.ExternalID})
	if err != nil {
		return claimsclient.SharedClaimBinding{}, err
	}
	owner, err := ownerFor(r.InstanceID, identity, string(encoded))
	if err != nil {
		return claimsclient.SharedClaimBinding{}, err
	}
	store, err := r.Writer.SharedClaimStore(ctx, policyRun, repo)
	if err != nil {
		return claimsclient.SharedClaimBinding{}, err
	}
	binding := claimsclient.SharedClaimBinding{Store: store, RemoteKey: key.ExternalID, Owner: owner,
		BeforeTransition: func(ctx context.Context) error { return r.Register(ctx, repo) }}
	if number, err := strconv.ParseUint(key.ExternalID, 10, 64); err == nil && number > 0 && strconv.FormatUint(number, 10) == key.ExternalID {
		binding.AfterTransition = func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			labels, err := r.Writer.SharedClaimVisibility(ctx, policyRun, repo)
			if err == nil {
				err = sharedclaim.ReconcileVisibility(ctx, store, labels, key.ExternalID)
			}
			if err != nil && r.Report != nil {
				r.Report(err)
			}
		}
	}
	return binding, nil
}

// ownerFor matches the local resolver's deterministic incarnation encoding.
func ownerFor(instanceID string, identity journal.RunIdentity, remoteKey string) (sharedclaim.Owner, error) {
	if instanceID == "" || identity.StartedAt.IsZero() || identity.WorkflowDigest == "" || identity.Gaggle == "" || identity.Workflow == "" {
		return sharedclaim.Owner{}, errors.New("shared claim requires complete persisted ownership")
	}
	data, err := json.Marshal([]string{instanceID, identity.RunID, identity.Gaggle, identity.Workflow, identity.WorkflowDigest, identity.StartedAt.UTC().Format(time.RFC3339Nano), remoteKey})
	if err != nil {
		return sharedclaim.Owner{}, err
	}
	owner := sharedclaim.Owner{Instance: instanceID, Run: identity.RunID, Token: fmt.Sprintf("%x", sha256.Sum256(data))}
	if _, err := sharedclaim.Encode(remoteKey, sharedclaim.Record{Version: 1, Owner: owner, ExpiresAt: identity.StartedAt}); err != nil {
		return sharedclaim.Owner{}, err
	}
	return owner, nil
}

func reconciliationRun(run string) (string, bool) {
	owner, suffix, ok := strings.Cut(run, "/backlog-reconcile/")
	if !ok || owner == "" {
		return "", false
	}
	parts := strings.Split(suffix, "/")
	if len(parts) != 2 {
		return "", false
	}
	for _, part := range parts {
		if part == "" {
			return "", false
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return "", false
			}
		}
	}
	return owner, true
}
