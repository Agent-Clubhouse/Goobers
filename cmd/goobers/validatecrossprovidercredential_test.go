package main

import (
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
)

// CFG012: in topology (b) an explicit credentials: entry for a pull-request or
// repository capability replaces the Azure DevOps repository's credential, so
// its token reaches Azure DevOps. It is honoured and warned about, never
// refused, and strict-neutral.
func TestCrossProviderCredentialOverrideWarning(t *testing.T) {
	gaggle := func(name string, project, backlog apiv1.Provider) apiv1.Gaggle {
		g := apiv1.Gaggle{}
		g.Name = name
		g.Spec.Project = apiv1.RepoRef{Provider: project, Owner: "example-org", Project: "example-project", Name: "example-repo"}
		g.Spec.Backlog = apiv1.BacklogRef{Provider: backlog, Project: "example-org/example-backlog"}
		return g
	}
	token := instance.TokenRef{Env: "EXAMPLE_TOKEN"}
	credentials := []instance.CredentialGrant{
		{Capability: "github:issues:write", Token: token},
		{Capability: "github:pr:write", Token: token},
		{Capability: "agent:model", Token: token},
		{Capability: "repo:push", Token: token},
	}
	tests := []struct {
		name      string
		gaggles   []apiv1.Gaggle
		wantPaths []string
	}{
		{
			name:      "topology (b) warns for each project-family entry",
			gaggles:   []apiv1.Gaggle{gaggle("mixed", apiv1.ProviderADO, apiv1.ProviderGitHub), gaggle("plain", apiv1.ProviderGitHub, apiv1.ProviderGitHub)},
			wantPaths: []string{"/credentials/1/capability", "/credentials/3/capability"},
		},
		{
			name:    "pure Azure DevOps never warns",
			gaggles: []apiv1.Gaggle{gaggle("ado", apiv1.ProviderADO, apiv1.ProviderADO)},
		},
		{
			name:    "pure GitHub never warns",
			gaggles: []apiv1.Gaggle{gaggle("gh", apiv1.ProviderGitHub, apiv1.ProviderGitHub)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := &instance.ConfigSet{Gaggles: tc.gaggles}
			cfg := &instance.Config{Credentials: credentials}
			var paths []string
			for _, w := range appendStaticRealityWarnings("", "config", instance.ConfigFileName,
				cfg, set, nil, &validate.Report{}, false, false) {
				if w.warning.Code != validate.WarningCrossProviderCredentialOverride {
					continue
				}
				paths = append(paths, w.path)
				if w.warning.Severity != validate.Warning || !strings.Contains(w.warning.Explanation, "mixed") ||
					!strings.Contains(w.warning.Explanation, "Azure DevOps") {
					t.Errorf("CFG012 = %+v, want a warning naming the gaggle and Azure DevOps", w.warning)
				}
			}
			if !slices.Equal(paths, tc.wantPaths) {
				t.Fatalf("CFG012 paths = %q, want %q", paths, tc.wantPaths)
			}
		})
	}
	if !slices.Contains(strictNeutralWarningCodes, validate.WarningCrossProviderCredentialOverride) {
		t.Fatal("CFG012 must be strict-neutral: the ruling is a warning, not a refusal")
	}
}
