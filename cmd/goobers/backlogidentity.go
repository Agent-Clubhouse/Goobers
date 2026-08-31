package main

import (
	"fmt"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// This file is the single resolution point for "which work-item container does
// this stage operate on, and what is its canonical identity" (personal-gaggle-
// routing §5.1/§5.2). Everything backlog-scoped — the addressing RepositoryRef
// handed to the provider, the authoritative claim-ledger scope, and the
// credential connection — is derived here from ONE resolved apiv1.BacklogRef so
// the three can never disagree about which backlog an item belongs to.
//
// Resolution order, most authoritative first:
//
//  1. the GOOBERS_BACKLOG_* stage env the runner injects from spec.backlog;
//  2. the gaggle's own spec.backlog read from instance config (daemon-side
//     handlers and stages predating the env injection);
//  3. the routed code repository, which is the correct answer for the large
//     majority of gaggles whose project and backlog are the same repository.
//
// Step 3 is a legitimate resolution, not a fallback to GOOBERS_REPO_* for a
// backlog that was declared elsewhere: it is reached only when no distinct
// backlog is declared at all.

// stageBacklogRef resolves the apiv1.BacklogRef this stage's work-item
// operations address, together with the ADO organization that scopes it.
func stageBacklogRef(root string, routed providers.RepositoryRef) (apiv1.BacklogRef, string, error) {
	if provider := os.Getenv(executor.BacklogProviderEnvVar); provider != "" {
		ref := apiv1.BacklogRef{
			Provider:      apiv1.Provider(provider),
			BaseURL:       os.Getenv(executor.BacklogBaseURLEnvVar),
			Project:       os.Getenv(executor.BacklogProjectEnvVar),
			ConnectionRef: os.Getenv(executor.BacklogConnectionRefEnvVar),
		}
		if ref.Project == "" {
			return apiv1.BacklogRef{}, "", fmt.Errorf(
				"%s is set but %s is empty; the backlog stage context is incomplete",
				executor.BacklogProviderEnvVar, executor.BacklogProjectEnvVar)
		}
		return ref, routed.Owner, nil
	}
	if gaggle := providerGaggle(); gaggle != "" && root != "" {
		if ref, ok := configuredBacklogRef(instance.NewLayout(root).ConfigDir(), gaggle); ok {
			return ref, routed.Owner, nil
		}
	}
	return backlogRefFromRepository(routed), routed.Owner, nil
}

// gaggleBacklogRef is stageBacklogRef's daemon-side counterpart: the run's
// failure/park/escalation handlers execute in the daemon process, so the gaggle
// name comes from the gaggle-scoped layout rather than the stage env.
func gaggleBacklogRef(l instance.Layout, routed providers.RepositoryRef) (apiv1.BacklogRef, string) {
	if gaggle := l.Gaggle(); gaggle != "" {
		if ref, ok := configuredBacklogRef(l.ConfigDir(), gaggle); ok {
			return ref, routed.Owner
		}
	}
	return backlogRefFromRepository(routed), routed.Owner
}

// configuredBacklogRef reads the named gaggle's spec.backlog out of the config
// tree. A load failure is reported as "not configured" rather than an error:
// every caller has a correct repository-derived answer to fall back on, and a
// transient config-read problem must not take down an unrelated backlog stage.
func configuredBacklogRef(configDir, gaggle string) (apiv1.BacklogRef, bool) {
	set, report, err := instance.LoadConfigDir(configDir)
	if err != nil || report == nil || set == nil {
		return apiv1.BacklogRef{}, false
	}
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		if g.Name != gaggle {
			continue
		}
		if g.Spec.Backlog.Provider == "" || g.Spec.Backlog.Project == "" {
			return apiv1.BacklogRef{}, false
		}
		return g.Spec.Backlog, true
	}
	return apiv1.BacklogRef{}, false
}

// backlogRefFromRepository expresses "the backlog is this repository" — the
// same-project/same-backlog topology every pre-routing gaggle uses.
func backlogRefFromRepository(routed providers.RepositoryRef) apiv1.BacklogRef {
	ref := apiv1.BacklogRef{Provider: apiv1.Provider(routed.Provider), BaseURL: routed.URL}
	if routed.Provider == providers.ProviderADO {
		ref.Project = routed.Project
		return ref
	}
	ref.Project = routed.Owner + "/" + routed.Name
	return ref
}

// backlogRepositoryRef converts a resolved backlog reference into the
// addressing RepositoryRef the provider layer takes. ADO keeps the routed
// organization and repository name because its provider is organization-scoped
// and only the project tier selects the work-item container; GitHub and Gitea
// address the backlog repository directly.
func backlogRepositoryRef(ref apiv1.BacklogRef, routed providers.RepositoryRef) (providers.RepositoryRef, error) {
	identity, err := apiv1.BacklogIdentityFor(ref, routed.Owner)
	if err != nil {
		return providers.RepositoryRef{}, err
	}
	switch identity.Provider {
	case apiv1.ProviderADO:
		out := routed
		out.Provider = providers.ProviderADO
		out.Project = identity.Project
		return out, nil
	case apiv1.ProviderGitHub, apiv1.ProviderGitea:
		// URL is deliberately not populated from the identity's BaseURL:
		// RepositoryRef.URL is a repository URL, while a Gitea forge root is
		// resolved from instance config by giteaRepoRefForStage. Carrying the
		// base URL here would put a forge root in a repository-URL field.
		return providers.RepositoryRef{
			Provider: providers.ProviderKind(identity.Provider),
			Owner:    identity.Owner,
			Name:     identity.Name,
		}, nil
	default:
		return providers.RepositoryRef{}, fmt.Errorf("unsupported backlog provider %q", identity.Provider)
	}
}

// backlogIdentityForStage resolves the canonical backlog identity a stage's
// claims are scoped by. It fails loudly rather than degrading to an empty or
// repository-derived identity: an unusable identity would silently reopen the
// exact cross-gaggle double-claim this scope exists to prevent.
func backlogIdentityForStage(root string, routed providers.RepositoryRef) (apiv1.BacklogIdentity, error) {
	ref, organization, err := stageBacklogRef(root, routed)
	if err != nil {
		return apiv1.BacklogIdentity{}, err
	}
	identity, err := apiv1.BacklogIdentityFor(ref, organization)
	if err != nil {
		return apiv1.BacklogIdentity{}, err
	}
	return identity, identity.Validate()
}

// backlogIdentityForGaggle is backlogIdentityForStage's daemon-side counterpart.
func backlogIdentityForGaggle(l instance.Layout, routed providers.RepositoryRef) (apiv1.BacklogIdentity, error) {
	ref, organization := gaggleBacklogRef(l, routed)
	identity, err := apiv1.BacklogIdentityFor(ref, organization)
	if err != nil {
		return apiv1.BacklogIdentity{}, err
	}
	return identity, identity.Validate()
}

// backlogRepositoryRefForStage is backlogRepoRefForStage's fail-loud sibling.
// Ownership-mutating paths use it so an unresolvable backlog can never be
// silently substituted with the code repository.
func backlogRepositoryRefForStage(root string, routed providers.RepositoryRef) (providers.RepositoryRef, error) {
	ref, _, err := stageBacklogRef(root, routed)
	if err != nil {
		return providers.RepositoryRef{}, err
	}
	return backlogRepositoryRef(ref, routed)
}

// backlogConnectionRefForStage returns the connection whose credential backlog
// operations must authenticate with, or "" when the backlog shares the
// project's credential.
func backlogConnectionRefForStage(root string, routed providers.RepositoryRef) string {
	ref, _, err := stageBacklogRef(root, routed)
	if err != nil {
		return ""
	}
	return ref.ConnectionRef
}

// backlogStageProviderOptions returns the option prefix EVERY backlog provider
// construction shares, so no stage has to remember it and no two stages can
// disagree about it.
//
// The option that matters is the connection: a provider built for the backlog
// WITHOUT it silently authenticates with the project's capability-scoped token.
// That is invisible in the same-repository majority — the two credentials are
// the same token — and only surfaces as an opaque 404/403 in exactly the
// cross-repository topology this feature exists to support, where the project
// token cannot see the backlog at all. Centralizing the option is what makes
// "built for the backlog" and "authenticated as the backlog" inseparable.
//
// An empty connectionRef is a no-op inside withStageProviderConnection, so a
// gaggle that declares no backlog connection keeps exactly its previous
// credential path.
func backlogStageProviderOptions(root string, routed providers.RepositoryRef, opts ...stageProviderOption) []stageProviderOption {
	return append([]stageProviderOption{
		withStageProviderConnection(backlogConnectionRefForStage(root, routed)),
	}, opts...)
}

// newBacklogProviderForStage is the single constructor for any provider whose
// calls are addressed at the resolved BACKLOG repository. It is built against
// the backlog repository and the backlog connection credential, not the routed
// code repository's — the two may be different repositories, different forges,
// and different accounts.
func newBacklogProviderForStage(root string, routed, backlogRepo providers.RepositoryRef, readOnly bool, opts ...stageProviderOption) (providers.Provider, error) {
	return newProviderForStage(root, backlogRepo, readOnly, backlogStageProviderOptions(root, routed, opts...)...)
}

// newBacklogProviderForStageAs is newBacklogProviderForStage for the stages
// that need a concrete provider type (the GitHub-only and ADO-only backlog
// paths) rather than the provider-neutral interface.
func newBacklogProviderForStageAs[T providers.Provider](root string, routed, backlogRepo providers.RepositoryRef, readOnly bool, opts ...stageProviderOption) (T, error) {
	return newProviderForStageAs[T](root, backlogRepo, readOnly, backlogStageProviderOptions(root, routed, opts...)...)
}

// newBacklogIssueProviderForStage builds the provider that backlog work-item
// MUTATIONS run through — the mutating specialization of
// newBacklogProviderForStage.
func newBacklogIssueProviderForStage(root string, routed, backlogRepo providers.RepositoryRef, opts ...stageProviderOption) (backlogIssueProvider, error) {
	provider, err := newBacklogProviderForStage(root, routed, backlogRepo, false,
		append([]stageProviderOption{withStageProviderMutations("issue")}, opts...)...)
	if err != nil {
		return nil, err
	}
	issueProvider, ok := provider.(backlogIssueProvider)
	if !ok {
		return nil, fmt.Errorf("backlog provider %q does not support work-item operations", backlogRepo.Provider)
	}
	return issueProvider, nil
}

// backlogClaimKey builds the authoritative claim-ledger key for one backlog
// item. Gaggle and provider ride along as descriptive fields; the backlog
// identity is what establishes exclusivity.
func backlogClaimKey(identity apiv1.BacklogIdentity, gaggle, itemID string) localscheduler.ClaimKey {
	return localscheduler.ClaimKey{
		Gaggle:     gaggle,
		Provider:   string(identity.Provider),
		ExternalID: itemID,
		Backlog:    identity,
	}
}
