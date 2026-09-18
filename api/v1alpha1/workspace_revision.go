package v1alpha1

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// RepositoryIdentity is the routing-independent, canonical identity of a
// repository. Branches, credentials, and checkout policy are intentionally
// excluded.
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
type WorkspaceRevision struct {
	Repository     RepositoryIdentity  `json:"repository"`
	CommitSHA      string              `json:"commitSha"`
	SourceRef      string              `json:"sourceRef,omitempty"`
	SourceID       string              `json:"sourceId,omitempty"`
	BaseRepository *RepositoryIdentity `json:"baseRepository,omitempty"`
	BaseSHA        string              `json:"baseSha,omitempty"`
}

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
		return fmt.Errorf("gitea repository URL is required")
	}
	if r.URL != "" {
		u, err := url.Parse(r.URL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" || r.URL != strings.TrimSpace(r.URL) {
			return fmt.Errorf("repository URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
	}
	return nil
}

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
