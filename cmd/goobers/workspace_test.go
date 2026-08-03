package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestResolvePinnedWorkspaceProject(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{
			Provider:  "github",
			Owner:     "acme",
			Name:      "web",
			Workspace: &instance.RepoWorkspaceConfig{Pinned: true},
		},
	}}
	for _, selector := range []string{"web", "acme/web", "ACME/WEB"} {
		got, err := resolvePinnedWorkspaceProject(cfg, selector)
		if err != nil {
			t.Fatalf("selector %q: %v", selector, err)
		}
		if got.Name != "web" {
			t.Fatalf("selector %q resolved %q", selector, got.Name)
		}
	}
}

func TestResolvePinnedWorkspaceProjectRejectsDisposableRepo(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{
			Provider: "github",
			Owner:    "acme",
			Name:     "web",
		},
	}}
	if _, err := resolvePinnedWorkspaceProject(cfg, "web"); err == nil ||
		!strings.Contains(err.Error(), "not configured for pinned workspace") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolvePinnedWorkspaceProjectRejectsAmbiguousShortName(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{
			Provider:  "github",
			Owner:     "acme",
			Name:      "web",
			Workspace: &instance.RepoWorkspaceConfig{Pinned: true},
		},
		{
			Provider:  "github",
			Owner:     "contoso",
			Name:      "web",
			Workspace: &instance.RepoWorkspaceConfig{Pinned: true},
		},
	}}

	if _, err := resolvePinnedWorkspaceProject(cfg, "web"); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v", err)
	}

	got, err := resolvePinnedWorkspaceProject(cfg, "contoso/web")
	if err != nil {
		t.Fatalf("qualified selector: %v", err)
	}
	if got.Owner != "contoso" {
		t.Fatalf("qualified selector resolved owner %q", got.Owner)
	}
}

func TestResolvePinnedWorkspaceProjectNoMatch(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{
			Provider:  "github",
			Owner:     "acme",
			Name:      "web",
			Workspace: &instance.RepoWorkspaceConfig{Pinned: true},
		},
	}}
	if _, err := resolvePinnedWorkspaceProject(cfg, "does-not-exist"); err == nil ||
		!strings.Contains(err.Error(), "no configured pinned repository matches") {
		t.Fatalf("error = %v", err)
	}
}
