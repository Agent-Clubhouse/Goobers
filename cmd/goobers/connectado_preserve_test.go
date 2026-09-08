package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestConnectADOReportsFullIdentity(t *testing.T) {
	var out strings.Builder
	printConnectResult(&out, connectOptions{ado: &connectADORepo{Organization: "org", Project: "project", Repository: "web"}}, onboardingActionResult{Path: "instance"})
	if !strings.Contains(out.String(), "connected org/project/web at instance") {
		t.Fatalf("ambiguous ADO identity: %s", out.String())
	}
}

func TestConnectADOPreservesRepositoryTuning(t *testing.T) {
	for _, mode := range []string{"placeholder", "credential", "repository"} {
		t.Run(mode, func(t *testing.T) {
			repo := instance.RepoRef{Provider: "ado", Owner: "org", Project: "boards", Name: "web", Token: instance.TokenRef{Env: "OLD_TOKEN"}, LargeRepo: true, DefaultStageTimeout: "40m", PathLength: &instance.RepoPathLengthConfig{Disabled: true}}
			switch mode {
			case "placeholder":
				repo.Owner, repo.Project, repo.Name = "your-org", "your-project", "your-repo"
			case "repository":
				repo.Name = "previous"
			}
			cfg := &instance.Config{Repos: []instance.RepoRef{repo}}
			changed, err := connectRewriteADOInstanceConfig(cfg, connectOptions{ado: &connectADORepo{Organization: "org", Project: "boards", Repository: "web"}, tokenEnv: "NEW_TOKEN", replace: mode != "placeholder"})
			if err != nil || !changed {
				t.Fatalf("rewrite: changed=%v err=%v", changed, err)
			}
			got := cfg.Repos[0]
			if !got.LargeRepo || got.DefaultStageTimeout != "40m" || got.PathLength != repo.PathLength {
				t.Fatalf("connection discarded operator tuning: %+v", got)
			}
		})
	}
}
