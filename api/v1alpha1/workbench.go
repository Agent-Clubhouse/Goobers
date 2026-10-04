package v1alpha1

// GaggleWorkbench declares source-owned planning material. It grants neither
// credentials nor human access; interactiveAccess remains the authority boundary.
type GaggleWorkbench struct {
	// SchemaVersion pins this configuration independently of workflow DSL versions.
	// +kubebuilder:validation:Enum=sources/v1
	SchemaVersion string `json:"schemaVersion" yaml:"schemaVersion"`
	// Sources names only this gaggle's backlog and configured repositories.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Sources []WorkbenchSource `json:"sources" yaml:"sources"`
	// RelationshipManifest selects a declared relationships source for new edges
	// that have no faithful native or frontmatter representation. Changing this
	// setting does not migrate existing edge ownership or provide permission fallback.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	RelationshipManifest string `json:"relationshipManifest,omitempty" yaml:"relationshipManifest,omitempty"`
}

// WorkbenchSource names one bounded provider/project or repository-file scope.
type WorkbenchSource struct {
	// Name is a persistent binding identity. Renaming it needs reviewed reference
	// migration; it is not a display title or credential name.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name" yaml:"name"`
	// Kind selects the existing singleton backlog, Markdown files or a manifest.
	// +kubebuilder:validation:Enum=backlog;documents;relationships
	Kind string `json:"kind" yaml:"kind"`
	// Repository must match a configured project/additional repository exactly.
	// Its configured branch is followed; no request can supply a different URL/ref.
	// Forbidden for backlog sources, whose target is gaggle.spec.backlog.
	// +optional
	Repository *InteractiveRepositoryIdentity `json:"repository,omitempty" yaml:"repository,omitempty"`
	// Paths are literal repository-relative files, not globs or crawl roots.
	// Documents allow up to 128 Markdown files; relationships requires one YAML file.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	Paths []string `json:"paths,omitempty" yaml:"paths,omitempty"`
	// Objectives explicitly classifies native backlog items as objectives.
	// Omission infers none. This selector does not narrow ordinary backlog browsing.
	// +optional
	Objectives *WorkbenchObjectiveSelector `json:"objectives,omitempty" yaml:"objectives,omitempty"`
	// Writes is an additional field/relationship allowlist. Omission is read-only.
	// A declared operation also needs current human policy, credentials and actual
	// provider capability; repository proposals always require a policy-governed PR.
	// +optional
	Writes *WorkbenchWrites `json:"writes,omitempty" yaml:"writes,omitempty"`
}

// WorkbenchObjectiveSelector uses exact native IDs, types or labels. Matching any
// listed value classifies the item; title and Markdown mentions never infer it.
type WorkbenchObjectiveSelector struct {
	// IDs are stable provider object identities within this configured backlog.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	IDs []string `json:"ids,omitempty" yaml:"ids,omitempty"`
	// Types names exact provider types such as ADO Epic or Feature.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	Types []string `json:"types,omitempty" yaml:"types,omitempty"`
	// Labels names exact source labels/tags; a match is explicit configuration.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	Labels []string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

// WorkbenchWrites narrows actions granted by the gaggle's interactive policy.
type WorkbenchWrites struct {
	// Fields permits supported edits on this source. Documents support title and
	// description (body) proposals only; manifests have no node fields.
	// +optional
	// +kubebuilder:validation:MaxItems=5
	// +listType=set
	Fields []WorkbenchField `json:"fields,omitempty" yaml:"fields,omitempty"`
	// Relationships permits supported source-owned edge proposals/mutations.
	// +optional
	// +kubebuilder:validation:MaxItems=6
	// +listType=set
	Relationships []WorkbenchRelationship `json:"relationships,omitempty" yaml:"relationships,omitempty"`
}

// WorkbenchField is a provider-neutral editable node field.
// +kubebuilder:validation:Enum=title;description;state;labels;assignees
type WorkbenchField string

// WorkbenchRelationship excludes journal-derived execution observations.
// +kubebuilder:validation:Enum=parent-of;blocked-by;contributes-to;references;milestone-member;implemented-by
type WorkbenchRelationship string
