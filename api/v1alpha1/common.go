package v1alpha1

// Provider identifies a backing system vendor. v1 abstracts repo + backlog over
// both GitHub and Azure DevOps (ADO) from the start (see VISION §8 "v1 providers").
type Provider string

const (
	// ProviderGitHub is GitHub (repos + issues/projects backlog).
	ProviderGitHub Provider = "github"
	// ProviderADO is Azure DevOps (repos + boards backlog).
	ProviderADO Provider = "ado"
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
	// +kubebuilder:validation:Enum=github;ado
	// +kubebuilder:validation:Required
	Provider Provider `json:"provider" yaml:"provider"`
	// Owner/organization (GitHub org/user, or ADO organization/project owner).
	// +kubebuilder:validation:Required
	Owner string `json:"owner" yaml:"owner"`
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
}

// BacklogRef points at the singleton backlog a gaggle draws work from.
type BacklogRef struct {
	// +kubebuilder:validation:Enum=github;ado
	// +kubebuilder:validation:Required
	Provider Provider `json:"provider" yaml:"provider"`
	// Project scopes the backlog (GitHub repo "owner/name" or ADO project).
	// +kubebuilder:validation:Required
	Project string `json:"project" yaml:"project"`
	// Query/labels narrow which items this gaggle considers work. Routing of an
	// item to a specific workflow is handled by workflow selectors (SCH-010).
	// +optional
	Labels []string `json:"labels,omitempty" yaml:"labels,omitempty"`
	// +optional
	Query string `json:"query,omitempty" yaml:"query,omitempty"`
	// ConnectionRef names the connection (credentials) used to reach the backlog.
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
