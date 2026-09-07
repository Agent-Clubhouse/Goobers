package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func TestCostPublicationRepositoryIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project apiv1.RepoRef
		repo    providers.RepositoryRef
		want    bool
	}{
		{"github URL and ID", apiv1.RepoRef{Provider: "github", Owner: "ORG", Name: "Repo"}, providers.RepositoryRef{Provider: "github", Owner: "org", Name: "repo", URL: "https://github.com/org/repo", ID: "123"}, true},
		{"ADO project differs", apiv1.RepoRef{Provider: "ado", Owner: "org", Project: "one", Name: "repo"}, providers.RepositoryRef{Provider: "ado", Owner: "org", Project: "two", Name: "repo"}, false},
		{"provider differs", apiv1.RepoRef{Provider: "github", Owner: "org", Name: "repo"}, providers.RepositoryRef{Provider: "ado", Owner: "org", Name: "repo"}, false},
		{"Gitea equivalent host", apiv1.RepoRef{Provider: "gitea", Owner: "org", Name: "repo", BaseURL: "https://forge.example/"}, providers.RepositoryRef{Provider: "gitea", Owner: "org", Name: "repo", URL: "https://forge.example/org/repo", ID: "123"}, true},
		{"Gitea distinct host", apiv1.RepoRef{Provider: "gitea", Owner: "org", Name: "repo", BaseURL: "https://one.example"}, providers.RepositoryRef{Provider: "gitea", Owner: "org", Name: "repo", URL: "https://two.example"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := costPublicationRepositoryMatches(tc.project, tc.repo); got != tc.want {
				t.Fatalf("matched=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestCostPublicationMissingExplicitInstanceFailsClosed(t *testing.T) {
	for _, gaggle := range []string{"", "origin"} {
		if enabled, err := resolveCostPublication(t.TempDir(), gaggle, postMergeTestRepo()); enabled || err == nil {
			t.Fatalf("missing instance, gaggle=%q: enabled=%v error=%v", gaggle, enabled, err)
		}
	}
}

func TestCostPublicationLoadsGaggleOverride(t *testing.T) {
	for _, tc := range []struct {
		name          string
		instanceValue string
		gaggleValue   any
		want          bool
	}{
		{"disable inherited", "false", nil, false},
		{"enable override", "false", true, true},
		{"disable override", "true", false, false},
		{"enable inherited", "true", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			layout := instance.NewLayout(root)
			set, _, err := instance.LoadConfigDir(layout.ConfigDir())
			if err != nil || len(set.Gaggles) != 1 {
				t.Fatalf("load demo: %v", err)
			}
			gaggle := set.Gaggles[0].Name
			source, ok := set.GaggleSource(gaggle)
			if !ok {
				t.Fatal("missing gaggle source")
			}
			path := filepath.Join(layout.ConfigDir(), source)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := yaml.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			document["spec"].(map[string]any)["cost"] = map[string]any{"enabled": tc.gaggleValue}
			raw, err = yaml.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			raw, err = os.ReadFile(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, []byte("\ncost:\n  enabled: "+tc.instanceValue+"\n")...)
			if err := os.WriteFile(layout.ConfigFile(), raw, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := resolveCostPublication(root, gaggle, postMergeTestRepo())
			if err != nil || got != tc.want {
				t.Fatalf("publication = %v, %v; want %v", got, err, tc.want)
			}
			if got, err := resolveCostPublication(root, "not-configured", postMergeTestRepo()); err == nil || got {
				t.Fatalf("unknown gaggle must fail closed: %v, %v", got, err)
			}
		})
	}
}

func TestPostMergeTimeoutPreservesOriginatingGaggle(t *testing.T) {
	for _, origin := range []string{"", "origin"} {
		t.Run("origin="+origin, func(t *testing.T) {
			root := t.TempDir()
			repo := postMergeTestRepo()
			t.Setenv(executor.GaggleEnvVar, origin)
			if err := recordPostMergeTimeout(root, repo, "20", time.Now()); err != nil {
				t.Fatal(err)
			}
			t.Setenv(executor.GaggleEnvVar, "other-gaggle")
			if err := recordPostMergeTimeout(root, repo, "20", time.Now()); err != nil {
				t.Fatal(err)
			}
			entry := loadPostMergeReconcileEntry(t, root, repo, "20")
			if entry.Gaggle != origin {
				t.Fatalf("originating gaggle = %q, want %q", entry.Gaggle, origin)
			}
		})
	}
}
