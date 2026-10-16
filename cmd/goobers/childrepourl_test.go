package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestChildRepositoryURLUsesProductionDefaultAndTrustedOverride(t *testing.T) {
	previous := repoCloneURL
	repoCloneURL = nil
	t.Cleanup(func() { repoCloneURL = previous })
	for _, test := range []struct {
		ref apiv1.RepoRef
		url string
	}{
		{apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "own", Name: "repo"}, "https://github.com/own/repo.git"},
		{apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "project", Name: "repo"}, "https://dev.azure.com/org/project/_git/repo"},
	} {
		got, err := childRepoCloneURL(test.ref)
		if err != nil || got != test.url {
			t.Fatal(got, err)
		}
	}
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return "trusted-test-path", nil }
	if got, err := childRepoCloneURL(apiv1.RepoRef{}); err != nil || got != "trusted-test-path" {
		t.Fatal(got, err)
	}
}
