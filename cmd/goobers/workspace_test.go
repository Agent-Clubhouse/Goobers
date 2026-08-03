package main

import (
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestPinnedWorkspaceLayoutUsesGaggleOverride(t *testing.T) {
	root := t.TempDir()
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	override := filepath.Join(t.TempDir(), "short")
	cfg := &instance.Config{
		Workcopies: &instance.WorkcopiesConfig{Root: filepath.Join(t.TempDir(), "instance")},
		Repos: []instance.RepoRef{{
			Provider: "github",
			Owner:    "acme",
			Name:     "web",
			Workspace: &instance.RepoWorkspaceConfig{
				Pinned: true,
			},
		}},
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets"},
		Spec: apiv1.GaggleSpec{
			Project:    project,
			Workcopies: &apiv1.GaggleWorkcopies{Root: override},
		},
	}}}

	layout, err := pinnedWorkspaceLayout(instance.NewLayout(root), cfg, set, project)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := layout.WorkcopiesBaseDir(), filepath.Join(override, "widgets"); got != want {
		t.Fatalf("WorkcopiesBaseDir = %q, want %q", got, want)
	}
}

func TestPinnedWorkspaceLayoutRejectsDifferentGaggleRoots(t *testing.T) {
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider:  "github",
		Owner:     "acme",
		Name:      "web",
		Workspace: &instance.RepoWorkspaceConfig{Pinned: true},
	}}}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "alpha"},
			Spec:       apiv1.GaggleSpec{Project: project, Workcopies: &apiv1.GaggleWorkcopies{Root: filepath.Join(t.TempDir(), "alpha")}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "beta"},
			Spec:       apiv1.GaggleSpec{Project: project, Workcopies: &apiv1.GaggleWorkcopies{Root: filepath.Join(t.TempDir(), "beta")}},
		},
	}}

	_, err := pinnedWorkspaceLayout(instance.NewLayout(t.TempDir()), cfg, set, project)
	if err == nil || !strings.Contains(err.Error(), "different workcopies roots") {
		t.Fatalf("error = %v", err)
	}
}
