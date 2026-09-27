package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
)

// topologyBDaemonIdentityConfig is a topology (b) instance with a daemon
// identity: an ADO code repository, a GitHub backlog repository and a GitHub
// PAT daemon identity.
func topologyBDaemonIdentityConfig(adoToken instance.TokenRef) *instance.Config {
	return &instance.Config{
		Repos: []instance.RepoRef{
			{Provider: "ado", Owner: topologyBOrg, Project: topologyBCodeProject, Name: topologyBCodeRepo, Token: adoToken},
			{Provider: "github", Owner: "example-org", Name: "example-backlog", Token: instance.TokenRef{Env: "TOPOLOGY_B_GITHUB_TOKEN"}},
		},
		DaemonIdentity: &instance.DaemonIdentityConfig{
			Kind:  instance.GitHubAuthPAT,
			Token: &instance.TokenRef{Env: "TOPOLOGY_B_DAEMON_TOKEN"},
		},
	}
}

func topologyBProjectAndBacklog() (apiv1.RepoRef, apiv1.BacklogRef) {
	return apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: topologyBOrg, Project: topologyBCodeProject, Name: topologyBCodeRepo},
		apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: topologyBBacklog}
}

// TestScheduledWorkflowCredentialEnvironmentsTopologyB keeps the scheduled
// workflow preflight in step with the runtime grants: in topology (b) the
// daemon identity backs only the backlog family, so the pull-request and
// repository capabilities are checked against the ADO repository's
// credential, not the identity's.
func TestScheduledWorkflowCredentialEnvironmentsTopologyB(t *testing.T) {
	cfg := topologyBDaemonIdentityConfig(instance.TokenRef{Env: "TOPOLOGY_B_ADO_PAT"})
	project, backlog := topologyBProjectAndBacklog()

	got, err := scheduledWorkflowCredentialEnvironments(cfg, project, backlog)
	if err != nil {
		t.Fatal(err)
	}
	for c, want := range map[capability.Capability]string{
		capability.GitHubIssuesWrite: "TOPOLOGY_B_DAEMON_TOKEN",
		capability.GitHubIssuesRead:  "TOPOLOGY_B_GITHUB_TOKEN",
		capability.GitHubPRWrite:     "TOPOLOGY_B_ADO_PAT",
		capability.GitHubPRMerge:     "TOPOLOGY_B_ADO_PAT",
		capability.RepoPush:          "TOPOLOGY_B_ADO_PAT",
	} {
		if got[string(c)] != want {
			t.Errorf("environment for %q = %q, want %q", c, got[string(c)], want)
		}
	}
}

// TestConfiguredCredentialGrantsTopologyB is the same agreement for the
// Copilot credential preflight: the daemon identity does not grant the ADO
// code capabilities in topology (b), so an ADO repository with no stored
// token grants none of them.
func TestConfiguredCredentialGrantsTopologyB(t *testing.T) {
	cfg := topologyBDaemonIdentityConfig(instance.TokenRef{})
	project, backlog := topologyBProjectAndBacklog()

	got, err := configuredCredentialGrants(cfg, project, backlog)
	if err != nil {
		t.Fatal(err)
	}
	if !got[string(capability.GitHubIssuesWrite)] {
		t.Errorf("github:issues:write not granted; the daemon identity backs it for the backlog")
	}
	for _, c := range []capability.Capability{capability.GitHubPRWrite, capability.GitHubPRMerge, capability.RepoPush} {
		if got[string(c)] {
			t.Errorf("%s granted by the daemon identity; in topology (b) it is backed by the ADO repository only", c)
		}
	}
}
