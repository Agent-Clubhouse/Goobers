package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func connectTargetProject(opts connectOptions) apiv1.RepoRef {
	ref := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: opts.owner, Name: opts.name}
	if opts.ado != nil {
		ref.Provider = apiv1.ProviderADO
		ref.Project = opts.ado.Project
	}
	return ref
}

func connectTargetMatches(repo instance.RepoRef, opts connectOptions) bool {
	target := connectTargetProject(opts)
	return repo.Provider == string(target.Provider) && repo.Owner == target.Owner && repo.Project == target.Project && repo.Name == target.Name
}

func connectRewriteTargetGaggle(path string, opts connectOptions) (bool, error) {
	if opts.ado != nil {
		return connectRewriteADOGaggleFile(path, opts)
	}
	return connectRewriteGaggleFile(path, opts.owner, opts.name, opts.replace)
}

func connectRewriteADOInstanceConfig(cfg *instance.Config, opts connectOptions) (bool, error) {
	a := opts.ado
	target := instance.RepoRef{Provider: "ado", Owner: a.Organization, Project: a.Project, Name: a.Repository, Token: instance.TokenRef{Env: opts.tokenEnv}, Auth: &instance.RepoAuthConfig{Kind: instance.ADOAuthPAT}}
	for i, repo := range cfg.Repos {
		if repo.Provider != "ado" {
			continue
		}
		placeholder := repo.Owner == connectPlaceholderOwner && repo.Name == connectPlaceholderName && repo.Project == "your-project"
		current := repo.Owner == target.Owner && repo.Project == target.Project && repo.Name == target.Name
		if current && repo.Token.Env == opts.tokenEnv && (repo.Auth == nil || repo.Auth.Kind == instance.ADOAuthPAT) {
			return false, nil
		}
		if placeholder || (current && opts.replace) {
			cfg.Repos[i] = connectADORepositoryIdentity(repo, target)
			return true, nil
		}
		if current {
			return false, fmt.Errorf("ADO repository %s already has a different credential source; use --replace to change it", a.String())
		}
	}
	if opts.replace && len(cfg.Repos) > 0 && cfg.Repos[0].Provider == "ado" {
		cfg.Repos[0] = connectADORepositoryIdentity(cfg.Repos[0], target)
		return true, nil
	}
	return false, fmt.Errorf("no matching ADO placeholder found; initialize with --template=standard --provider=ado first; connect never changes an existing repository's provider")
}

// Change only the requested identity and credential; repository execution
// policy belongs to the operator and is not reset by connecting a repository.
func connectADORepositoryIdentity(existing, target instance.RepoRef) instance.RepoRef {
	existing.Owner = target.Owner
	existing.Project = target.Project
	existing.Name = target.Name
	existing.Token = target.Token
	existing.Auth = target.Auth
	return existing
}

func connectRewriteADOGaggleFile(path string, opts connectOptions) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return false, fmt.Errorf("not a YAML document")
	}
	spec := yamlMapValue(doc.Content[0], "spec")
	project := yamlMapValue(spec, "project")
	if project == nil || yamlScalarValue(project, "provider") != "ado" {
		return false, nil
	}
	a := opts.ado
	placeholder := yamlScalarValue(project, "owner") == connectPlaceholderOwner && yamlScalarValue(project, "name") == connectPlaceholderName && yamlScalarValue(project, "project") == "your-project"
	current := yamlScalarValue(project, "owner") == a.Organization && yamlScalarValue(project, "name") == a.Repository && yamlScalarValue(project, "project") == a.Project
	if !placeholder && !current && !opts.replace {
		return false, nil
	}
	changed := false
	for key, value := range map[string]string{"owner": a.Organization, "project": a.Project, "name": a.Repository} {
		node := yamlMapValue(project, key)
		if node == nil {
			return false, fmt.Errorf("ADO spec.project.%s is missing", key)
		}
		if node.Value != value {
			node.Value = value
			changed = true
		}
	}
	if connectRewriteADOBacklog(yamlMapValue(spec, "backlog"), a.Project, opts.replace) {
		changed = true
	}
	if !changed {
		return false, nil
	}
	var out strings.Builder
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return false, err
	}
	if err := encoder.Close(); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func connectRewriteADOBacklog(backlog *yaml.Node, project string, replace bool) bool {
	if yamlScalarValue(backlog, "provider") != "ado" {
		return false
	}
	node := yamlMapValue(backlog, "project")
	if node == nil || node.Value == project || (node.Value != "your-project" && !replace) {
		return false
	}
	node.Value = project
	return true
}

func yamlScalarValue(node *yaml.Node, key string) string {
	value := yamlMapValue(node, key)
	if value == nil {
		return ""
	}
	return value.Value
}
