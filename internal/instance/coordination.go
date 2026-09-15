package instance

import (
	"fmt"

	"github.com/goobers/goobers/internal/coordination"
	"github.com/goobers/goobers/providers"
)

func (c *Config) validateCoordination() error {
	if c.Coordination == nil {
		return nil
	}
	seen := map[string]bool{}
	parents := map[string]bool{}
	for _, authority := range c.Coordination.Gaggles {
		if err := authority.Validate(); err != nil {
			return fmt.Errorf("coordination: %w", err)
		}
		if seen[authority.Name] || parents[authority.ParentRepo.Key()] {
			return fmt.Errorf("coordination requires unique gaggle and parent repository ownership")
		}
		seen[authority.Name], parents[authority.ParentRepo.Key()] = true, true
		repos := []coordination.Repository{authority.ParentRepo}
		for _, target := range authority.Targets {
			repos = append(repos, target.Repository)
		}
		for _, repo := range repos {
			matches := 0
			for _, configured := range c.Repos {
				candidate := providers.RepositoryRef{Provider: providers.ProviderKind(configured.Provider), Owner: configured.Owner, Project: configured.Project, Name: configured.Name, URL: configured.BaseURL}
				if candidate.CanonicalKey() == repo.Key() {
					matches++
				}
			}
			if matches != 1 {
				return fmt.Errorf("coordination repository %s/%s must match exactly one configured repo identity", repo.Owner, repo.Name)
			}
		}
	}
	return nil
}
