package v1alpha1

import (
	"fmt"
	"net/url"
	"strings"
)

// Provider identifies a backing system vendor. v1 abstracts repo + backlog over
// both GitHub and Azure DevOps (ADO) from the start (see VISION §8 "v1 providers").
type Provider string

const (
	// ProviderGitHub is GitHub (repos + issues/projects backlog).
	ProviderGitHub Provider = "github"
	// ProviderADO is Azure DevOps (repos + boards backlog).
	ProviderADO Provider = "ado"
	// ProviderGitea is a self-hosted Gitea forge (repos + issues backlog). Gitea
	// has no fixed host, so a RepoRef/BacklogRef using it must also set BaseURL.
	ProviderGitea Provider = "gitea"
)

// SecretRef references a secret without storing its value in the repo. Secrets
// are always Key Vault references injected at runtime (CFG-009, SEC-010); they
// are never inlined into config-as-code.
type SecretRef struct {
	// Name of the connection/secret this reference resolves through. For Key
	// Vault-backed secrets this is the Key Vault secret name.
	// +kubebuilder:validation:Required
	Name string `json:"name" yaml:"name"`
	// Key optionally selects a single field within the referenced secret.
	// +optional
	Key string `json:"key,omitempty" yaml:"key,omitempty"`
	// KeyVault optionally names the Key Vault holding the secret; when empty the
	// gaggle's default vault is used.
	// +optional
	KeyVault string `json:"keyVault,omitempty" yaml:"keyVault,omitempty"`
}

// RepoRef points at a git repository through a provider connection. Auth is a
// SecretRef — never an inline token.
type RepoRef struct {
	// +kubebuilder:validation:Enum=github;ado;gitea
	// +kubebuilder:validation:Required
	Provider Provider `json:"provider" yaml:"provider"`
	// BaseURL is the forge root URL (e.g. https://gitea.example.com). It is
	// required when provider=gitea (self-hosted Gitea has no fixed host) and
	// omitted for github/ado.
	// +optional
	BaseURL string `json:"baseUrl,omitempty" yaml:"baseUrl,omitempty"`
	// Owner/organization (GitHub org/user or Azure DevOps organization).
	// +kubebuilder:validation:Required
	Owner string `json:"owner" yaml:"owner"`
	// Project is the Azure DevOps project. It is omitted for GitHub.
	// +optional
	Project string `json:"project,omitempty" yaml:"project,omitempty"`
	// Name of the repository.
	// +kubebuilder:validation:Required
	Name string `json:"name" yaml:"name"`
	// Branch is the default branch goober runs check out and target.
	// +optional
	// +kubebuilder:default=main
	Branch string `json:"branch,omitempty" yaml:"branch,omitempty"`
	// ConnectionRef names the connection (and thus credentials) used to reach
	// this repo. It resolves to a Connection declared in the Manifest.
	// +optional
	ConnectionRef string `json:"connectionRef,omitempty" yaml:"connectionRef,omitempty"`
	// Checkout narrows how much of the repository run workspaces materialize
	// (B2, #649): the local runner provisions a cone-mode sparse checkout
	// (git sparse-checkout set --cone) instead of the full tree. Nil (the
	// default) is a full checkout.
	// +optional
	Checkout *CheckoutSpec `json:"checkout,omitempty" yaml:"checkout,omitempty"`
}

// CheckoutSpec declares partial-checkout behavior for a repository reference.
// A nil *CheckoutSpec (the field is optional on RepoRef) is a full checkout;
// an explicitly-declared CheckoutSpec must name at least one cone — an empty
// Sparse list is a validation error (api/validate), not another way to spell
// "full checkout".
type CheckoutSpec struct {
	// Sparse lists repo-relative path cones a sparse checkout materializes;
	// paths outside every cone are absent from run workspaces, except
	// root-level files (cone-mode semantics). Required non-empty when
	// Checkout is declared at all.
	// +optional
	Sparse []string `json:"sparse,omitempty" yaml:"sparse,omitempty"`
}

// EnvelopeRef is the projection of this reference that rides a stage
// invocation envelope: repository identity and connection fields only.
// Checkout is workspace-materialization config the runner consumes before a
// stage ever runs; it stays off the wire so declaring it never changes the
// closed stage contract (invocation.schema.json's repoRef,
// docs/stage-contract.md).
func (r RepoRef) EnvelopeRef() RepoRef {
	r.Checkout = nil
	return r
}

// BacklogRef points at the singleton backlog a gaggle draws work from.
type BacklogRef struct {
	// +kubebuilder:validation:Enum=github;ado;gitea
	// +kubebuilder:validation:Required
	Provider Provider `json:"provider" yaml:"provider"`
	// BaseURL is the forge root URL (e.g. https://gitea.example.com). It is
	// required when provider=gitea (self-hosted Gitea has no fixed host) and
	// omitted for github/ado.
	// +optional
	BaseURL string `json:"baseUrl,omitempty" yaml:"baseUrl,omitempty"`
	// Project scopes the backlog (GitHub repo "owner/name" or ADO project).
	// +kubebuilder:validation:Required
	Project string `json:"project" yaml:"project"`
	// Labels narrow which items this gaggle considers work. Routing of an
	// item to a specific workflow is handled by workflow selectors (SCH-010).
	// +optional
	Labels []string `json:"labels,omitempty" yaml:"labels,omitempty"`
	// LabelPredicate is a CEL expression over the item's label set. The only
	// supported operations are string membership in `labels` and boolean
	// composition with &&, ||, and !. It is ANDed with Labels.
	// +kubebuilder:validation:MinLength=1
	// +optional
	LabelPredicate string `json:"labelPredicate,omitempty" yaml:"labelPredicate,omitempty"`
	// FieldPredicate is a CEL expression over the provider's typed native-field
	// projection. It is ANDed with Labels and fails when a referenced field is
	// unavailable.
	// +kubebuilder:validation:MinLength=1
	// +optional
	FieldPredicate string `json:"fieldPredicate,omitempty" yaml:"fieldPredicate,omitempty"`
	// ConnectionRef names the connection (credentials) used to reach the backlog.
	// +optional
	ConnectionRef string `json:"connectionRef,omitempty" yaml:"connectionRef,omitempty"`
}

// BacklogIdentity canonically identifies a provider work-item container —
// the stable scope key that authoritative claims, routing validation, and
// journal correlation share. It is derived deterministically from a
// BacklogRef's configuration and is provider-neutral. Normalization rules:
//   - Provider is the canonical enum value.
//   - BaseURL is trimmed, lowered, and stripped of trailing "/".
//   - GitHub/Gitea: Owner and Name are parsed from BacklogRef.Project
//     ("owner/name"); Project is left empty, so the raw "owner/name" string can
//     never produce a second, differently-shaped key for the same container.
//   - ADO: Project is the ADO project and Owner is the ADO organization, which
//     a BacklogRef cannot carry itself (the provider is organization-scoped and
//     the organization comes from instance config) — see BacklogIdentityFor.
//     Name is empty because an ADO backlog is project-scoped, not repo-scoped.
//   - Serialization is "provider|baseUrl|owner|project|name" with each field
//     URL-query-escaped, stable across processes and platforms.
//
// Two backlogs are the same container exactly when their identities are equal,
// so equal external item IDs drawn from different backlogs never contend.
type BacklogIdentity struct {
	Provider Provider `json:"provider"`
	BaseURL  string   `json:"baseUrl,omitempty"`
	Owner    string   `json:"owner,omitempty"`
	Project  string   `json:"project,omitempty"`
	Name     string   `json:"name,omitempty"`
}

// BacklogIdentityFromRef derives a canonical backlog identity from a BacklogRef
// alone. Use BacklogIdentityFor when the ADO organization is known; without it
// an ADO identity is scoped only by project, which is correct for a
// single-organization instance and the historical behavior everywhere else.
func BacklogIdentityFromRef(ref BacklogRef) (BacklogIdentity, error) {
	return BacklogIdentityFor(ref, "")
}

// BacklogIdentityFor derives a canonical backlog identity, supplying the
// organization context a BacklogRef cannot carry. organization is used only by
// ADO (where it is the organization the project lives under); GitHub and Gitea
// parse their owner out of BacklogRef.Project and ignore it.
func BacklogIdentityFor(ref BacklogRef, organization string) (BacklogIdentity, error) {
	id := BacklogIdentity{
		Provider: ref.Provider,
		BaseURL:  normalizeBaseURL(ref.BaseURL),
	}
	switch ref.Provider {
	case ProviderGitHub, ProviderGitea:
		owner, name, ok := strings.Cut(ref.Project, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return BacklogIdentity{}, fmt.Errorf("backlog identity: %s project must be %q, got %q", ref.Provider, "owner/name", ref.Project)
		}
		id.Owner = owner
		id.Name = name
	case ProviderADO:
		if ref.Project == "" {
			return BacklogIdentity{}, fmt.Errorf("backlog identity: ado backlog requires a project")
		}
		id.Owner = organization
		id.Project = ref.Project
	default:
		return BacklogIdentity{}, fmt.Errorf("backlog identity: unsupported provider %q", ref.Provider)
	}
	return id, nil
}

// Validate rejects an identity that is incomplete for its provider. Callers
// that build an identity by hand (deserialized ledger entries, tests) use it so
// a half-populated value can never become an ownership key that silently
// collides with another backlog's.
func (b BacklogIdentity) Validate() error {
	switch b.Provider {
	case ProviderGitHub, ProviderGitea:
		if b.Owner == "" || b.Name == "" {
			return fmt.Errorf("backlog identity: %s requires owner and name", b.Provider)
		}
		if b.Project != "" {
			return fmt.Errorf("backlog identity: %s must not set project", b.Provider)
		}
	case ProviderADO:
		if b.Project == "" {
			return fmt.Errorf("backlog identity: ado requires a project")
		}
	default:
		return fmt.Errorf("backlog identity: unsupported provider %q", b.Provider)
	}
	return nil
}

// String returns the stable serialization used as a claim ledger scope key.
func (b BacklogIdentity) String() string {
	return url.QueryEscape(string(b.Provider)) + "|" +
		url.QueryEscape(b.BaseURL) + "|" +
		url.QueryEscape(b.Owner) + "|" +
		url.QueryEscape(b.Project) + "|" +
		url.QueryEscape(b.Name)
}

// IsZero reports whether the identity is empty/unset.
func (b BacklogIdentity) IsZero() bool {
	return b.Provider == "" && b.BaseURL == "" && b.Owner == "" && b.Project == "" && b.Name == ""
}

// Equal reports whether two identities refer to the same backlog container.
func (b BacklogIdentity) Equal(other BacklogIdentity) bool {
	return b == other
}

func normalizeBaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), "/")
}

// Connection declares a named, reusable link to an external system. Manifests
// declare connections once; gaggles/goobers reference them by name. Credentials
// are always SecretRefs.
type Connection struct {
	// +kubebuilder:validation:Required
	Name string `json:"name" yaml:"name"`
	// Type categorizes what the connection links to.
	// +kubebuilder:validation:Enum=repo;backlog;telemetry;identity;harness
	// +kubebuilder:validation:Required
	Type string `json:"type" yaml:"type"`
	// Provider is the backing vendor/service (e.g. github, ado, azure-adx, entra).
	// +kubebuilder:validation:Required
	Provider string `json:"provider" yaml:"provider"`
	// SecretRef holds the credentials for this connection (Key Vault reference).
	// +kubebuilder:validation:Required
	SecretRef SecretRef `json:"secretRef" yaml:"secretRef"`
	// Endpoint optionally overrides the default service endpoint/host.
	// +optional
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
}
