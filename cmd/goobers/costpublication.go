package main

import (
	"errors"
	"io"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// costPublicationAllowed keeps reporting configuration failures separate from
// merge correctness: suppress optional external cost data, but continue normal
// close-out. Never use the sweep process's gaggle for a delayed record.
func costPublicationAllowed(root, gaggle string, repo providers.RepositoryRef, stderr io.Writer) bool {
	enabled, err := resolveCostPublication(root, gaggle, repo)
	if err != nil {
		pf(stderr, "warning: cost publication suppressed: %v\n", err)
		return false
	}
	return enabled
}

func resolveCostPublication(root, gaggle string, repo providers.RepositoryRef) (bool, error) {
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil && (root != "" || !errors.Is(err, os.ErrNotExist)) {
		return false, errors.New("instance configuration is unreadable or invalid")
	}
	if _, err := os.Stat(layout.ConfigDir()); err != nil {
		if !errors.Is(err, os.ErrNotExist) || (cfg != nil && gaggle != "") {
			return false, errors.New("gaggle configuration is unavailable")
		}
		// Unscoped legacy calls without a definitions tree retain the default.
		return cfg.CostReportingEnabled(nil), nil
	}
	set, _, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		return false, errors.New("gaggle configuration is unreadable or invalid")
	}
	if gaggle != "" {
		for i := range set.Gaggles {
			if set.Gaggles[i].Name == gaggle {
				return cfg.CostReportingEnabled(&set.Gaggles[i]), nil
			}
		}
		return false, errors.New("originating gaggle is no longer configured")
	}
	// Legacy/standalone records lack an originating gaggle. A repository may
	// belong to several gaggles; a disabled matching gaggle forbids guessing
	// that an enabled one originated the report.
	enabled := cfg.CostReportingEnabled(nil)
	matched := false
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		if !costPublicationRepositoryMatches(g.Spec.Project, repo) {
			continue
		}
		if !matched {
			enabled = true
			matched = true
		}
		enabled = enabled && cfg.CostReportingEnabled(g)
	}
	return enabled, nil
}

func costPublicationRepositoryMatches(project apiv1.RepoRef, repo providers.RepositoryRef) bool {
	candidate := providers.RepositoryRef{
		Provider: providers.ProviderKind(project.Provider), Owner: project.Owner,
		Project: project.Project, Name: project.Name, URL: project.BaseURL,
	}
	// Gaggle declarations identify repositories by name, not provider-assigned
	// opaque IDs. GitHub/ADO service locations are already fixed by their
	// configured provider/organization; only Gitea declares a service URL here.
	repo.ID = ""
	if candidate.Provider != providers.ProviderGitea {
		candidate.URL = ""
		repo.URL = ""
	}
	return candidate.CanonicalKey() == repo.CanonicalKey()
}
