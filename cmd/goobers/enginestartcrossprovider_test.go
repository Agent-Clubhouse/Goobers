package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestEngineStartSpecPinsRoleRoutedBacklogProvider pins the topology (b)
// marker the engine refuses stage pods on: a GitHub or Gitea backlog for Azure
// DevOps code is pinned, and every other gaggle pins nothing, so its runs
// dispatch exactly as before.
func TestEngineStartSpecPinsRoleRoutedBacklogProvider(t *testing.T) {
	ado := apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: "web"}
	github := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "example-org", Name: "web"}
	cases := []struct {
		name    string
		project apiv1.RepoRef
		backlog apiv1.BacklogRef
		want    apiv1.Provider
	}{
		{name: "ado code, github backlog", project: ado, backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "example-org/backlog"}, want: apiv1.ProviderGitHub},
		{name: "ado code, gitea backlog", project: ado, backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitea, Project: "example-org/backlog"}, want: apiv1.ProviderGitea},
		{name: "ado code, ado backlog project", project: ado, backlog: apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "example-backlog"}},
		{name: "github code, github backlog", project: github, backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "example-org/web"}},
		{name: "github code, gitea backlog", project: github, backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitea, Project: "example-org/web"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, set, _ := runControlsFixture()
			set.Gaggles[0].Spec.Project = tc.project
			set.Gaggles[0].Spec.Backlog = tc.backlog
			spec, err := engineRunSpec(engineRunRequestFor(t, cfg, set, "web", "implementation"))
			if err != nil {
				t.Fatalf("engineRunSpec: %v", err)
			}
			if spec.RoleRoutedBacklogProvider != tc.want {
				t.Fatalf("StartSpec.RoleRoutedBacklogProvider = %q, want %q", spec.RoleRoutedBacklogProvider, tc.want)
			}
		})
	}
}
