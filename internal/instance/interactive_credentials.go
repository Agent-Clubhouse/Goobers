package instance

import (
	"fmt"
	"strings"
)

// InteractiveCredential names an execution identity for explicit gaggle human
// operations. Sources use the existing secret-reference and provider-auth
// mechanisms, but are never selected by the automation credential fallback.
type InteractiveCredential struct {
	Name       string          `json:"name" yaml:"name"`
	Provider   string          `json:"provider" yaml:"provider"`
	Owner      string          `json:"owner" yaml:"owner"`
	Project    string          `json:"project,omitempty" yaml:"project,omitempty"`
	Repository string          `json:"repository,omitempty" yaml:"repository,omitempty"`
	Token      TokenRef        `json:"token,omitempty" yaml:"token,omitempty"`
	Auth       *RepoAuthConfig `json:"auth,omitempty" yaml:"auth,omitempty"`
}

// RepositorySource adapts an explicitly selected binding to the existing
// credential-source constructors. Empty repository is valid only for an ADO
// project backlog and never authorizes code access.
func (c InteractiveCredential) RepositorySource() RepoRef {
	return RepoRef{Provider: c.Provider, Owner: c.Owner, Project: c.Project, Name: c.Repository, Token: c.Token, Auth: c.Auth}
}

func (c *Config) validateInteractiveCredentials(stores map[string]bool) error {
	if len(c.InteractiveCredentials) > 128 {
		return fmt.Errorf("interactiveCredentials must contain at most 128 entries")
	}
	seen := map[string]bool{}
	for i, source := range c.InteractiveCredentials {
		if !interactiveIdentifier(source.Name, 128) || seen[source.Name] {
			return fmt.Errorf("interactiveCredentials[%d]: name is invalid or duplicated", i)
		}
		seen[source.Name] = true
		if err := source.validate(i, stores, c.Runner.EnvPassthrough); err != nil {
			return err
		}
	}
	return nil
}

func (c InteractiveCredential) validate(i int, stores map[string]bool, passthrough []string) error {
	if (c.Provider != "github" && c.Provider != "ado") || !interactiveIdentifier(c.Owner, 256) {
		return fmt.Errorf("interactiveCredentials[%d]: an exact GitHub or ADO owner is required", i)
	}
	if c.Repository != "" && !interactiveIdentifier(c.Repository, 256) {
		return fmt.Errorf("interactiveCredentials[%d]: repository identity is invalid", i)
	}
	if c.Provider == "github" && (c.Repository == "" || c.Project != "") {
		return fmt.Errorf("interactiveCredentials[%d]: GitHub requires repository and forbids project", i)
	}
	if c.Provider == "ado" && !interactiveIdentifier(c.Project, 256) {
		return fmt.Errorf("interactiveCredentials[%d]: ADO requires an exact project", i)
	}
	if c.Token.GitHubCLI != nil {
		if c.Provider != "github" || c.Token.GitHubCLI.Hostname != "github.com" {
			return fmt.Errorf("interactiveCredentials[%d]: GitHub CLI source must select github.com for a GitHub target", i)
		}
	}
	repo := c.RepositorySource()
	if err := repo.validateToken(i, stores); err != nil {
		return fmt.Errorf("interactiveCredentials[%d]: %w", i, err)
	}
	if err := repo.validateProvider(i, stores, passthrough); err != nil {
		return fmt.Errorf("interactiveCredentials[%d]: %w", i, err)
	}
	if c.Token.Env != "" && stageEnvironmentAllows(c.Token.Env, passthrough) {
		return fmt.Errorf("interactiveCredentials[%d]: token.env must not be inherited by stage environments", i)
	}
	return nil
}

func interactiveIdentifier(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "/\\\x00\r\n\t")
}
