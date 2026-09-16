package v1alpha1

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// RepositoryIdentity is the lossless API projection of providers.RepositoryRef.
// It carries identity only, never branch, checkout policy, or credentials.
// +kubebuilder:object:generate=false
type RepositoryIdentity struct {
	Provider Provider `json:"provider"`
	Owner    string   `json:"owner,omitempty"`
	Project  string   `json:"project,omitempty"`
	Name     string   `json:"name"`
	ID       string   `json:"id,omitempty"`
	URL      string   `json:"url,omitempty"`
}

// WorkspaceRevision binds a run to an immutable repository and exact commit.
// SourceRef and SourceID are provenance, not checkout or publication authority.
// The binding is replayed from accepted history, not reselected from moving refs.
// Absence in legacy histories preserves configured-base workspace behavior.
// This identity neither changes the configured writable base nor establishes
// workspaceBranch ownership; workspace deltas remain a separate artifact control.
// +kubebuilder:object:generate=false
type WorkspaceRevision struct {
	Repository     RepositoryIdentity  `json:"repository"`
	CommitSHA      string              `json:"commitSha"`
	SourceRef      string              `json:"sourceRef,omitempty"`
	SourceID       string              `json:"sourceId,omitempty"`
	BaseRepository *RepositoryIdentity `json:"baseRepository,omitempty"`
	BaseSHA        string              `json:"baseSha,omitempty"`
}

// ValidateCommitSHA accepts full, lowercase SHA-1 and SHA-256 Git object IDs.
// Abbreviations, symbolic refs, and revision expressions are never accepted.
func ValidateCommitSHA(sha string) error {
	if len(sha) != 40 && len(sha) != 64 {
		return fmt.Errorf("commit SHA must contain 40 or 64 lowercase hexadecimal characters")
	}
	for _, ch := range sha {
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') {
			return fmt.Errorf("commit SHA must contain 40 or 64 lowercase hexadecimal characters")
		}
	}
	return nil
}

// Validate checks identity shape; configuration authorization is a separate step.
func (r RepositoryIdentity) Validate() error {
	switch r.Provider {
	case ProviderGitHub, ProviderADO, ProviderGitea:
	default:
		return fmt.Errorf("repository provider %q is unsupported", r.Provider)
	}
	for _, field := range []struct{ name, value string }{
		{"owner", r.Owner}, {"name", r.Name}, {"project", r.Project}, {"id", r.ID},
	} {
		if (field.name == "owner" || field.name == "name") && field.value == "" {
			return fmt.Errorf("repository %s is required", field.name)
		}
		if field.value != strings.TrimSpace(field.value) ||
			strings.ContainsAny(field.value, "/\\|") ||
			strings.IndexFunc(field.value, unicode.IsControl) >= 0 ||
			field.value == "." || field.value == ".." {
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
		return fmt.Errorf("Gitea repository URL is required to identify the service")
	}
	if r.URL != "" {
		u, err := url.Parse(r.URL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") ||
			u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
			r.URL != strings.TrimSpace(r.URL) {
			return fmt.Errorf("repository URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
	}
	return nil
}

// CanonicalKey mirrors providers.RepositoryRef.CanonicalKey, including its
// service host, ADO project, and provider-native ID discrimination.
func (r RepositoryIdentity) CanonicalKey() string {
	host := strings.TrimSpace(r.URL)
	if parsed, err := url.Parse(host); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	parts := []string{string(r.Provider), host, r.Project, r.Owner, r.Name, r.ID}
	for i := range parts {
		parts[i] = strings.ToLower(strings.TrimSpace(parts[i]))
	}
	return strings.Join(parts, "|")
}

// Validate checks the binding without resolving a moving ref or contacting a provider.
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

// DeepCopy returns an independently owned binding, including base provenance.
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
