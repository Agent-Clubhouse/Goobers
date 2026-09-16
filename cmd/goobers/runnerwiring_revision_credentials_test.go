package main

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestSelectedRevisionADOCheckoutCannotBorrowBaseCredential(t *testing.T) {
	t.Setenv("REVISION_ADO_BASE_TOKEN", "revision-ado-base-secret-123456")
	base := apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme", Project: "widgets", Name: "web"}
	fork := apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "contributor", Project: "widgets", Name: "web"}
	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider: "ado", Owner: base.Owner, Project: base.Project, Name: base.Name,
		Token: instance.TokenRef{Env: "REVISION_ADO_BASE_TOKEN"},
	}}}
	additional := []apiv1.RepoRef{fork}
	resolver, grants, err := buildCredentials(cfg, nil, "acme/widgets", base.Name, additional, nil)
	if err != nil {
		t.Fatal(err)
	}
	cloneURL := func(repo apiv1.RepoRef) (string, error) {
		return "https://dev.azure.com/" + repo.Owner + "/" + repo.Project + "/_git/" + repo.Name, nil
	}
	gitEnv, err := buildWorktreeGitEnv(cfg, t.TempDir(), base, additional, resolver, grants, cloneURL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gitEnv == nil {
		t.Fatal("configured ADO base credential was not wired")
	}
	baseURL, err := cloneURL(base)
	if err != nil {
		t.Fatal(err)
	}
	env, err := gitEnv(context.Background(), baseURL)
	if err != nil || len(env) == 0 {
		t.Fatalf("base checkout credential was not preserved: %v", err)
	}
	forkURL, err := cloneURL(fork)
	if err != nil {
		t.Fatal(err)
	}
	env, err = gitEnv(context.Background(), forkURL)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 0 {
		t.Fatal("selected source inherited the ADO base credential")
	}
}
