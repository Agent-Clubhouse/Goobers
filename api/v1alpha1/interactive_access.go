package v1alpha1

// InteractiveAction is an explicitly permitted human operation. Configuration
// grants authority; a route must separately implement the operation before
// advertising it as available.
// +kubebuilder:validation:Enum=session.create;session.message;backlog.read;backlog.edit;backlog.resolve;repository.read;run.intervene;run.restartStage;pr.repair;source.proposeChange;queue.cancel
type InteractiveAction string

// InteractiveAccessPolicy opts one gaggle into authenticated human operations.
// Omission grants no new provider reads, sessions, or writes. Instance roles
// remain necessary, and administrators require explicit gaggle membership.
type InteractiveAccessPolicy struct {
	// Humans grants viewers/operators by verified issuer and subject or group.
	Humans InteractiveHumanGrants `json:"humans" yaml:"humans"`
	// Actions is the closed operation allowlist. Empty permits policy inspection
	// only; it does not enable a provider-backed read.
	// +optional
	// +kubebuilder:validation:MaxItems=11
	// +listType=set
	Actions []InteractiveAction `json:"actions,omitempty" yaml:"actions,omitempty"`
	// Credentials selects named instance interactiveCredentials entries. There
	// is no inheritance from automation, connectionRef, or another target.
	// +optional
	Credentials InteractiveCredentialBindings `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	// SourceWrites always uses pull requests. Omission enforces the same rule.
	// +optional
	SourceWrites *InteractiveSourceWrites `json:"sourceWrites,omitempty" yaml:"sourceWrites,omitempty"`
}

// InteractiveHumanGrants separates visibility from permission to operate.
// Operators also have viewer access within this gaggle.
type InteractiveHumanGrants struct {
	// Viewers may inspect explicitly enabled interactive read surfaces.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	Viewers []InteractiveHumanGrant `json:"viewers,omitempty" yaml:"viewers,omitempty"`
	// Operators may perform explicitly allowed actions and inspect this gaggle.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	Operators []InteractiveHumanGrant `json:"operators,omitempty" yaml:"operators,omitempty"`
}

// InteractiveHumanGrant matches the authenticated issuer plus exactly one
// stable subject or verified group. Names and request bodies are not identity.
// +kubebuilder:validation:XValidation:rule="has(self.subject) != has(self.group)",message="exactly one subject or group is required"
type InteractiveHumanGrant struct {
	// Issuer exactly matches the authenticated trust domain (OIDC issuer URL).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Issuer string `json:"issuer" yaml:"issuer"`
	// Subject exactly matches the authenticated stable subject claim.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Subject string `json:"subject,omitempty" yaml:"subject,omitempty"`
	// Group exactly matches a group from the configured verified token claim.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Group string `json:"group,omitempty" yaml:"group,omitempty"`
}

// InteractiveCredentialBindings keeps the singleton backlog independent from
// each code repository, including when the two use different providers.
type InteractiveCredentialBindings struct {
	// Backlog names the instance interactive credential for this gaggle's
	// configured backlog. Missing means provider-backed backlog access is off.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Backlog string `json:"backlog,omitempty" yaml:"backlog,omitempty"`
	// Repositories explicitly enables configured project/additional repositories.
	// Every qualified identity must already belong to this gaggle.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Repositories []InteractiveRepositoryCredential `json:"repositories,omitempty" yaml:"repositories,omitempty"`
}

// InteractiveRepositoryCredential selects a named credential for one exact
// gaggle repository. It never accepts an arbitrary client-submitted URL.
type InteractiveRepositoryCredential struct {
	// Repository is a qualified configured project or additional repository.
	Repository InteractiveRepositoryIdentity `json:"repository" yaml:"repository"`
	// CredentialRef names one instance interactiveCredentials entry.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	CredentialRef string `json:"credentialRef" yaml:"credentialRef"`
}

// InteractiveRepositoryIdentity excludes branch, checkout and decorative
// connection fields; only the actual provider identity selects a target.
// +kubebuilder:validation:XValidation:rule="self.provider == 'ado' ? has(self.project) : !has(self.project)",message="project is required for ADO and forbidden for GitHub"
type InteractiveRepositoryIdentity struct {
	// Provider is GitHub or Azure DevOps.
	// +kubebuilder:validation:Enum=github;ado
	Provider Provider `json:"provider" yaml:"provider"`
	// Owner is the GitHub owner or Azure DevOps organization.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Owner string `json:"owner" yaml:"owner"`
	// Project is required for Azure DevOps and forbidden for GitHub.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Project string `json:"project,omitempty" yaml:"project,omitempty"`
	// Name is the repository name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Name string `json:"name" yaml:"name"`
}

// InteractiveSourceWrites enforces reviewable repository changes.
type InteractiveSourceWrites struct {
	// Mode requires a pull request; direct repository publication is unsupported.
	// +kubebuilder:validation:Enum=pull-request
	// +kubebuilder:default=pull-request
	Mode string `json:"mode" yaml:"mode"`
}
