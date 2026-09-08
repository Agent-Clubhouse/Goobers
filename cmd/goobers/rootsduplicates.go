package main

import (
	"context"
	"fmt"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// Only lock-verified daemons participate in active-scope warnings. On-disk
// config is evidence of configured scope, not proof of a currently running job.
func inspectDiscoveryScopes(ctx context.Context, output *rootsDiscoveryOutput) {
	for i := range output.Roots {
		root := &output.Roots[i]
		if root.OwningPID == 0 {
			continue
		}
		set, validation, err := instance.LoadConfigSource(ctx, instance.LocalDirSource{Path: instance.NewLayout(root.Path).ConfigDir()})
		if err != nil || validation == nil || set == nil {
			root.ScopeProblem = "Local configuration could not be validated; active scope comparison is incomplete."
			output.Partial = true
			continue
		}
		for _, gaggle := range set.Gaggles {
			root.Gaggles = append(root.Gaggles, gaggle.Name)
		}
		root.Repositories = discoveryRepositoryKeys(set)
		sort.Strings(root.Gaggles)
	}
	output.Warnings = duplicateRootWarnings(output.Roots)
}

func duplicateRootWarnings(roots []discoveredRoot) []string {
	warnings := []string{}
	identities := make(map[string][]string)
	gaggles := make(map[string][]string)
	repositories := make(map[string][]string)
	for _, root := range roots {
		if root.ID != "" {
			identities[root.ID] = append(identities[root.ID], root.Path)
		}
		if root.OwningPID == 0 {
			continue
		}
		for _, name := range root.Gaggles {
			gaggles[name] = append(gaggles[name], root.Path)
		}
		for _, key := range root.Repositories {
			repositories[key] = append(repositories[key], root.Path)
		}
	}
	for id, paths := range identities {
		if len(paths) > 1 {
			warnings = append(warnings, fmt.Sprintf("Instance ID %q is shared by multiple roots %q; copied identity does not establish which root is authoritative.", id, paths))
		}
	}
	for name, paths := range gaggles {
		if len(paths) > 1 {
			warnings = append(warnings, fmt.Sprintf("Gaggle %q is configured in multiple roots with owning daemons: %q. Inspect both before mutating either root.", name, paths))
		}
	}
	for key, paths := range repositories {
		if len(paths) > 1 {
			warnings = append(warnings, fmt.Sprintf("Repository %q is configured in multiple roots with owning daemons: %q. Inspect both before mutating either root.", key, paths))
		}
	}
	sort.Strings(warnings)
	return warnings
}

func discoveryRepositoryKeys(set *instance.ConfigSet) []string {
	keys := make(map[string]bool)
	add := func(repo apiv1.RepoRef) {
		key := providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), URL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}.CanonicalKey()
		keys[key] = true
	}
	for _, gaggle := range set.Gaggles {
		add(gaggle.Spec.Project)
		for _, repo := range gaggle.Spec.AdditionalRepos {
			add(repo)
		}
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
