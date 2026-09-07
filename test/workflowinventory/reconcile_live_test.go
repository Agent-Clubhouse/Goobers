//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationDormantWorkflowBlockersRemainOpen(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_GITHUB_TOKEN", "GITHUB_TOKEN")
	token := os.Getenv("GOOBERS_GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	repository := os.Getenv("GOOBERS_DESIGN_LEDGER_REPO")
	if repository == "" {
		repository = "Agent-Clubhouse/Goobers"
	}
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		t.Fatalf("invalid repository %q", repository)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	workflows, err := scanWorkflows(filepath.Join(".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("docs", "reference", "workflow-inventory.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseInventory(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if problems := compare(workflows, rows); len(problems) != 0 {
		t.Fatalf("inventory drift: %v", problems)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	provider := providers.NewGitHubProvider(token)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: owner, Name: name}
	cache := map[string]bool{}
	resolve := func(ref string) (bool, bool) {
		if closed, found := cache[ref]; found {
			return closed, true
		}
		item, err := provider.GetWorkItem(ctx, repo, strings.TrimPrefix(ref, "#"))
		if err != nil {
			t.Logf("resolve %s: %v", ref, err)
			return false, false
		}
		closed := strings.EqualFold(item.State, "closed")
		cache[ref] = closed
		return closed, true
	}
	for _, problem := range reconcileDormantBlockers(workflows, rows, resolve) {
		t.Error(problem)
	}
}
