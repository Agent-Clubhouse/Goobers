package main

import (
	"cmp"
	"fmt"
	"os"
	"reflect"
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

func connectRepositoryName(opts connectOptions) string {
	if opts.ado != nil {
		return opts.ado.String()
	}
	return opts.owner + "/" + opts.name
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
	for i, repo := range cfg.Repos {
		if repo.Provider != "ado" {
			continue
		}
		target := connectADOTarget(repo, opts)
		placeholder := repo.Owner == connectPlaceholderOwner && repo.Name == connectPlaceholderName && repo.Project == "your-project"
		current := repo.Owner == target.Owner && repo.Project == target.Project && repo.Name == target.Name
		if current && connectADOSameCredential(repo, target) {
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
		cfg.Repos[0] = connectADORepositoryIdentity(cfg.Repos[0], connectADOTarget(cfg.Repos[0], opts))
		return true, nil
	}
	return false, fmt.Errorf("no matching ADO placeholder found; initialize with --template=standard --provider=ado first; connect never changes an existing repository's provider")
}

// connectADOTarget is the repository entry connect writes over existing: the
// requested identity, and the credential source. An existing source is kept
// unless --token-env was given explicitly: an azure-cli (init's default),
// workload-identity or managed-identity block, or a PAT's own token reference.
// Otherwise, and for an entry with no source at all, connect records PAT
// authentication reading the --token-env variable.
func connectADOTarget(existing instance.RepoRef, opts connectOptions) instance.RepoRef {
	a := opts.ado
	target := instance.RepoRef{Provider: "ado", Owner: a.Organization, Project: a.Project, Name: a.Repository}
	if !opts.explicitTokenEnv && (connectADONonPATAuthKind(existing) != "" || existing.Token != (instance.TokenRef{})) {
		target.Token = existing.Token
		target.Auth = existing.Auth
		return target
	}
	target.Token = instance.TokenRef{Env: opts.tokenEnv}
	target.Auth = &instance.RepoAuthConfig{Kind: instance.ADOAuthPAT}
	return target
}

// connectADONonPATAuthKind returns repo's auth kind when it is an Azure
// DevOps credential source other than a PAT, and "" otherwise (no auth block
// means PAT).
func connectADONonPATAuthKind(repo instance.RepoRef) string {
	if repo.Auth == nil || repo.Auth.Kind == "" || repo.Auth.Kind == instance.ADOAuthPAT {
		return ""
	}
	return repo.Auth.Kind
}

// connectADOAuthLabel names repo's Azure DevOps auth kind; no auth block is PAT.
func connectADOAuthLabel(repo instance.RepoRef) string {
	if kind := connectADONonPATAuthKind(repo); kind != "" {
		return kind
	}
	return instance.ADOAuthPAT
}

// connectADOSameCredential reports, by value, whether existing already
// carries target's credential source, so a repeat connect changes nothing.
func connectADOSameCredential(existing, target instance.RepoRef) bool {
	pat := &instance.RepoAuthConfig{Kind: instance.ADOAuthPAT}
	existingAuth, targetAuth := cmp.Or(existing.Auth, pat), cmp.Or(target.Auth, pat)
	return reflect.DeepEqual(existing.Token, target.Token) && reflect.DeepEqual(*existingAuth, *targetAuth)
}

// connectKeptADOCredential points the steps after the rewrite at the
// credential source connect kept (connectADOTarget), so they authenticate the
// way the instance will: a kept PAT's own token variable becomes opts.tokenEnv,
// and any other kept source (a non-PAT kind, or a PAT read from a file,
// keychain or secret store) is recorded in opts.keptADOAuth, since it names
// no token variable to preflight with.
func connectKeptADOCredential(cfg *instance.Config, opts connectOptions) connectOptions {
	if opts.ado == nil || opts.explicitTokenEnv {
		return opts
	}
	for i := range cfg.Repos {
		repo := cfg.Repos[i]
		if !connectTargetMatches(repo, opts) {
			continue
		}
		if connectADONonPATAuthKind(repo) == "" && repo.Token.Env != "" {
			opts.tokenEnv = repo.Token.Env
		} else {
			opts.keptADOAuth = &repo
		}
		return opts
	}
	return opts
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
	changed := connectRewriteGaggleDisplayName(spec, connectPlaceholderOwner+"/your-project/"+connectPlaceholderName, a.String(), opts.replace)
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
