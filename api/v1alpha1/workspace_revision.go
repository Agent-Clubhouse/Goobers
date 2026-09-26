package v1alpha1

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// RepositoryIdentity is the routing-independent, canonical identity of a
// repository. Branches, credentials, and checkout policy are intentionally
// excluded.
// +kubebuilder:object:generate=false
type RepositoryIdentity struct {
	Provider Provider `json:"provider"`
	URL      string   `json:"url,omitempty"`
	Owner    string   `json:"owner"`
	Project  string   `json:"project,omitempty"`
	Name     string   `json:"name"`
	ID       string   `json:"id,omitempty"`
}

// WorkspaceRevision binds a successful deterministic result to an exact
// repository object. Source fields are provenance only.
// +kubebuilder:object:generate=false
type WorkspaceRevision struct {
	Repository     RepositoryIdentity  `json:"repository"`
	CommitSHA      string              `json:"commitSha"`
	SourceRef      string              `json:"sourceRef,omitempty"`
	SourceID       string              `json:"sourceId,omitempty"`
	BaseRepository *RepositoryIdentity `json:"baseRepository,omitempty"`
	BaseSHA        string              `json:"baseSha,omitempty"`
}

// UnmarshalJSON preserves the closed revision contract at envelope boundaries
// without rejecting unrelated envelope extensions.
func (r *WorkspaceRevision) UnmarshalJSON(data []byte) error {
	if err := validateRevisionJSONMembers(data, false); err != nil {
		return &WorkspaceRevisionDecodeError{Err: err}
	}
	type revision WorkspaceRevision
	var decoded revision
	if err := json.Unmarshal(data, &decoded); err != nil {
		return &WorkspaceRevisionDecodeError{Err: err}
	}
	if err := WorkspaceRevision(decoded).Validate(); err != nil {
		return &WorkspaceRevisionDecodeError{Err: err}
	}
	*r = WorkspaceRevision(decoded)
	return nil
}

// ValidateCommitSHA ensures the SHA is a full lowercase 40- or 64-character
// hexadecimal object ID.
func ValidateCommitSHA(sha string) error {
	if len(sha) != 40 && len(sha) != 64 {
		return fmt.Errorf("commit SHA must contain 40 or 64 lowercase hexadecimal characters")
	}
	for _, ch := range sha {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return fmt.Errorf("commit SHA must contain 40 or 64 lowercase hexadecimal characters")
		}
	}
	return nil
}

// Validate ensures the repository identity is canonical and compatible with the
// configured provider.
func (r RepositoryIdentity) Validate() error {
	switch r.Provider {
	case ProviderGitHub, ProviderADO, ProviderGitea:
	default:
		return fmt.Errorf("repository provider %q is unsupported", r.Provider)
	}
	for _, field := range []struct {
		name, value string
		required    bool
	}{{"owner", r.Owner, true}, {"name", r.Name, true}, {"project", r.Project, false}, {"id", r.ID, false}} {
		if field.required && field.value == "" {
			return fmt.Errorf("repository %s is required", field.name)
		}
		if invalidRepositoryIdentityComponent(field.value) {
			return fmt.Errorf("repository %s is not a canonical identity component", field.name)
		}
	}
	if r.Provider == ProviderADO && r.Project == "" {
		return fmt.Errorf("ADO repository project is required")
	}
	if r.Provider != ProviderADO && r.Project != "" {
		return fmt.Errorf("repository project is only valid for ADO")
	}
	if r.Provider == ProviderGitea && r.URL == "" {
		return fmt.Errorf("gitea repository URL is required")
	}
	if r.URL != "" {
		if !validRepositoryIdentityURL(r.URL) {
			return fmt.Errorf("repository URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
	}
	return nil
}

func invalidRepositoryIdentityComponent(value string) bool {
	return value != strings.TrimSpace(value) ||
		strings.ContainsAny(value, "/\\|") ||
		strings.IndexFunc(value, unicode.IsControl) >= 0 ||
		value == "." || value == ".."
}

func validRepositoryIdentityURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil &&
		u.Hostname() != "" &&
		(u.Scheme == "http" || u.Scheme == "https") &&
		u.User == nil &&
		!u.ForceQuery &&
		u.RawQuery == "" &&
		u.Fragment == "" &&
		!strings.ContainsAny(value, "#\\") &&
		!strings.ContainsAny(u.Hostname(), "[]<>\"`{}^") &&
		strings.IndexFunc(value, func(ch rune) bool { return unicode.IsSpace(ch) || unicode.IsControl(ch) }) < 0
}

// Validate ensures the selected revision carries a canonical repository identity
// and valid object IDs.
func (r WorkspaceRevision) Validate() error {
	if err := r.Repository.Validate(); err != nil {
		return fmt.Errorf("workspace revision repository: %w", err)
	}
	if err := ValidateCommitSHA(r.CommitSHA); err != nil {
		return fmt.Errorf("workspace revision commitSha: %w", err)
	}
	if r.BaseRepository != nil {
		if err := r.BaseRepository.Validate(); err != nil {
			return fmt.Errorf("workspace revision baseRepository: %w", err)
		}
	}
	if r.BaseSHA != "" {
		if err := ValidateCommitSHA(r.BaseSHA); err != nil {
			return fmt.Errorf("workspace revision baseSha: %w", err)
		}
	}
	return nil
}

// DeepCopy returns a deep-copy of the selected revision, preserving any nested
// base repository value while copying the top-level structure.
func (r *WorkspaceRevision) DeepCopy() *WorkspaceRevision {
	if r == nil {
		return nil
	}
	out := *r
	if r.BaseRepository != nil {
		base := *r.BaseRepository
		out.BaseRepository = &base
	}
	return &out
}
