package workbench

// SourceView describes configured planning scope, never credentials or grants.
// Declared write fields remain an additional allowlist, not user authorization.
type SourceView struct {
	BindingID          string   `json:"bindingId"`
	Kind               string   `json:"kind"`
	Provider           string   `json:"provider"`
	Owner              string   `json:"owner"`
	Project            string   `json:"project,omitempty"`
	Repository         string   `json:"repository,omitempty"`
	Branch             string   `json:"branch,omitempty"`
	Paths              []string `json:"paths,omitempty"`
	WriteFields        []string `json:"writeFields,omitempty"`
	WriteRelationships []string `json:"writeRelationships,omitempty"`
	WriteMetadata      []string `json:"writeMetadata,omitempty"`
}

// SourcePage lists bounded applied source metadata and its configuration digest.
type SourcePage struct {
	Items      []SourceView `json:"items"`
	Generation string       `json:"generation"`
}
