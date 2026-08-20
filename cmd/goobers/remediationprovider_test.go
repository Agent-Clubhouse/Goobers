package main

import (
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func configureRemediationGitea(t *testing.T, root, baseURL string) {
	t.Helper()
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatalf("load instance config: %v", err)
	}
	cfg.Repos = []instance.RepoRef{{
		Provider: string(providers.ProviderGitea),
		BaseURL:  baseURL,
		Owner:    "your-org",
		Name:     "your-repo",
		Token:    instance.TokenRef{Env: "GITEA_TOKEN"},
	}}
	if err := instance.WriteConfig(layoutFor(root).ConfigFile(), cfg); err != nil {
		t.Fatalf("write Gitea instance config: %v", err)
	}
}
