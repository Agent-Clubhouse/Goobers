package workbench

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// BoundSource contains a copied source declaration and its existing configured
// target. Branch and endpoint come from the gaggle, never from a read request.
// It contains no credentials and cannot authorize provider access.
type BoundSource struct {
	Spec       apiv1.WorkbenchSource
	Repository apiv1.RepoRef
	Backlog    apiv1.BacklogRef
	// BacklogIdentity pins the exact configured provider target. ADO has an
	// organization/project and no repository name; GitHub has owner/name.
	BacklogIdentity apiv1.InteractiveRepositoryIdentity
}

// SourceSet resolves a gaggle's explicit declarations before provider access.
type SourceSet struct {
	Scope         Scope
	Sources       []BoundSource
	ManifestOwner *Owner
}

// BindSources validates and copies source scope, without resolving secrets,
// reading a repository or consulting another gaggle. An absent configuration is
// inert. The caller must use current interactive policy for every actual read.
func BindSources(gaggle apiv1.Gaggle) (SourceSet, error) {
	if gaggle.Spec.Workbench == nil {
		return SourceSet{}, nil
	}
	g := gaggle.DeepCopy()
	config := g.Spec.Workbench
	if config.SchemaVersion != "sources/v1" || len(config.Sources) == 0 || len(config.Sources) > 32 {
		return SourceSet{}, errors.New("workbench: invalid source version or binding count")
	}
	result := SourceSet{Scope: Scope{GaggleID: g.Name, Bindings: map[string]bool{}}}
	locations := map[string]bool{}
	backlog := false
	for _, source := range config.Sources {
		if !bindingID.MatchString(source.Name) || result.Scope.Bindings[source.Name] {
			return SourceSet{}, errors.New("workbench: source names must be valid and unique")
		}
		bound, err := bindSource(*g, source)
		if err != nil {
			return SourceSet{}, fmt.Errorf("workbench source %s: %w", source.Name, err)
		}
		if source.Kind == "backlog" {
			if backlog {
				return SourceSet{}, errors.New("workbench: singleton backlog has multiple source owners")
			}
			backlog = true
		}
		if err := addSourceLocations(locations, bound); err != nil {
			return SourceSet{}, err
		}
		result.Scope.Bindings[source.Name] = true
		result.Sources = append(result.Sources, bound)
		if config.RelationshipManifest == source.Name {
			if source.Kind != "relationships" {
				return SourceSet{}, errors.New("workbench: relationshipManifest must name a relationships source")
			}
			result.ManifestOwner = &Owner{Kind: "manifest", SourceBindingID: source.Name, Path: source.Paths[0]}
		}
	}
	if config.RelationshipManifest != "" && result.ManifestOwner == nil {
		return SourceSet{}, errors.New("workbench: relationshipManifest is not a declared source")
	}
	return result, result.Scope.Validate()
}

func bindSource(g apiv1.Gaggle, source apiv1.WorkbenchSource) (BoundSource, error) {
	bound := BoundSource{Spec: source}
	if source.Kind == "backlog" {
		if source.Repository != nil || len(source.Paths) != 0 {
			return BoundSource{}, errors.New("backlog target must come only from the gaggle backlog")
		}
		if g.Spec.Backlog.Provider != apiv1.ProviderGitHub && g.Spec.Backlog.Provider != apiv1.ProviderADO {
			return BoundSource{}, errors.New("interactive backlog source requires GitHub or ADO")
		}
		if err := validateObjectiveSelector(source.Objectives); err != nil {
			return BoundSource{}, err
		}
		bound.Backlog = g.Spec.Backlog
		identity, err := sourceBacklogIdentity(g)
		if err != nil {
			return BoundSource{}, err
		}
		bound.BacklogIdentity = identity
	} else {
		if source.Objectives != nil {
			return BoundSource{}, errors.New("native objective selectors apply only to backlog sources")
		}
		repository, err := sourceRepository(g, source.Repository)
		if err != nil {
			return BoundSource{}, err
		}
		if err := validateSourcePaths(source.Kind, source.Paths); err != nil {
			return BoundSource{}, err
		}
		bound.Repository = repository
	}
	return bound, validateSourceWrites(source.Kind, source.Writes)
}

func sourceRepository(g apiv1.Gaggle, identity *apiv1.InteractiveRepositoryIdentity) (apiv1.RepoRef, error) {
	if !validRepositoryIdentity(identity) {
		return apiv1.RepoRef{}, errors.New("document source requires a configured GitHub or ADO repository")
	}
	repositories := append([]apiv1.RepoRef{g.Spec.Project}, g.Spec.AdditionalRepos...)
	var matched *apiv1.RepoRef
	for _, repository := range repositories {
		actual := apiv1.InteractiveRepositoryIdentity{Provider: repository.Provider, Owner: repository.Owner, Project: repository.Project, Name: repository.Name}
		if actual == *identity {
			if repository.BaseURL != "" {
				return apiv1.RepoRef{}, errors.New("interactive source does not support a custom provider endpoint")
			}
			if repository.Branch == "" {
				repository.Branch = "main"
			}
			if !textValue(repository.Branch, 1024) {
				return apiv1.RepoRef{}, errors.New("repository branch exceeds the source reference bound")
			}
			if matched != nil {
				return apiv1.RepoRef{}, errors.New("repository identity has multiple configured targets")
			}
			matched = &repository
		}
	}
	if matched != nil {
		return *matched, nil
	}
	return apiv1.RepoRef{}, errors.New("repository is not configured in this gaggle")
}

func validateSourcePaths(kind string, paths []string) error {
	if len(paths) == 0 || len(paths) > 128 || (kind == "relationships" && len(paths) != 1) {
		return errors.New("documents require 1–128 files; relationships requires exactly one file")
	}
	if kind != "documents" && kind != "relationships" {
		return errors.New("unknown source kind")
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if !validSourcePath(path) || seen[path] {
			return errors.New("source paths must be unique literal repository-relative files")
		}
		lower := strings.ToLower(path)
		if kind == "documents" && !strings.HasSuffix(lower, ".md") {
			return errors.New("document source paths must end in .md")
		}
		if kind == "relationships" && !strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".yml") {
			return errors.New("relationship manifest path must end in .yaml or .yml")
		}
		seen[path] = true
	}
	return nil
}

func addSourceLocations(seen map[string]bool, source BoundSource) error {
	for _, path := range source.Spec.Paths {
		raw, _ := json.Marshal(struct {
			Provider                        apiv1.Provider
			Owner, Project, Name, Ref, Path string
		}{source.Repository.Provider, source.Repository.Owner, source.Repository.Project, source.Repository.Name, source.Repository.Branch, path})
		key := string(raw)
		if seen[key] {
			return errors.New("workbench: a repository file has multiple configured source owners")
		}
		seen[key] = true
	}
	return nil
}
