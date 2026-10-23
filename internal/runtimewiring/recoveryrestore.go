package runtimewiring

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// RecoveryConfiguredProject finds the configured repository for a recovery key.
func RecoveryConfiguredProject(cfg *instance.Config, key string) (apiv1.RepoRef, error) {
	var matches []apiv1.RepoRef
	for _, repo := range cfg.Repos {
		identity := providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), URL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
		if identity.CanonicalKey() == key {
			matches = append(matches, apiv1.RepoRef{Provider: apiv1.Provider(repo.Provider), BaseURL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name})
		}
	}
	if len(matches) != 1 {
		return apiv1.RepoRef{}, fmt.Errorf("recovery record must match exactly one configured repository")
	}
	return matches[0], nil
}
