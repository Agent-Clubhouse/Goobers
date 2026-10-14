package capability

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestFitsProviderFollowsTheCapabilityNamespace(t *testing.T) {
	providers := []apiv1.Provider{apiv1.ProviderGitHub, "", apiv1.ProviderADO, apiv1.ProviderGitea}
	cases := []struct {
		capability string
		fits       map[apiv1.Provider]bool
	}{
		{capability: "github:issues:write", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true, apiv1.ProviderGitea: true}},
		{capability: "github:issues:approve", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true, apiv1.ProviderGitea: true}},
		{capability: "github:milestones:write", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true, apiv1.ProviderGitea: true}},
		{capability: "ado:pr:complete", fits: map[apiv1.Provider]bool{apiv1.ProviderADO: true}},
		{capability: "ado:packaging:read", fits: map[apiv1.Provider]bool{apiv1.ProviderADO: true}},
		{capability: "repo:push", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true, apiv1.ProviderADO: true, apiv1.ProviderGitea: true}},
		{capability: "provider:pr:write", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true, apiv1.ProviderADO: true, apiv1.ProviderGitea: true}},
		{capability: "contents:read", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true, apiv1.ProviderADO: true, apiv1.ProviderGitea: true}},
		{capability: "agent:model", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true, apiv1.ProviderADO: true, apiv1.ProviderGitea: true}},
	}
	for _, tc := range cases {
		for _, provider := range providers {
			if got := FitsProvider(tc.capability, provider); got != tc.fits[provider] {
				t.Errorf("FitsProvider(%q, %q) = %v, want %v", tc.capability, provider, got, tc.fits[provider])
			}
		}
	}
}
