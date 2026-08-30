package v1alpha1

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
	//
	// It is not a runtime credential selector (#3296): the local runner
	// resolves each access's credential from instance.yaml repos[] by
	// repository identity, never from the named connection, so the connection
	// named here has no effect on which token backs the access — validate
	// reports REF012 wherever it is declared.
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
	//
	// It is not a runtime credential selector (#3296): a gaggle's backlog
	// capabilities are backed by the same repo-identity-selected credential as
	// the rest of its stages, so naming a backlog-specific connection here does
	// not route the backlog through a different credential. validate reports
	// REF012 wherever it is declared.
	// +optional
	ConnectionRef string `json:"connectionRef,omitempty" yaml:"connectionRef,omitempty"`
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
