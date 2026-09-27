package credentials

import (
	"reflect"
	"testing"
)

// TestRoleRunnerGrants_BacklogRoleBindsIssueFamilyToBacklogRepo covers topology
// (b) (docs/design/ado-parity-dsl-2-0.md §7.2 step 2): the backlog-family
// capabilities bind to the backlog repository's credential, and pull-request
// and repository capabilities keep the project repository's.
func TestRoleRunnerGrants_BacklogRoleBindsIssueFamilyToBacklogRepo(t *testing.T) {
	bindings := []RepoBinding{
		{Owner: "example-org/example-project", Name: "service", TokenRef: "example-org/example-project/service"},
		{Owner: "example-org", Name: "backlog", TokenRef: "example-org/backlog"},
	}
	caps := []string{"repo:push", "github:issues:read", "github:issues:write", "github:pr:write", "github:milestones:write"}
	role := &BacklogRole{Owner: "example-org", Name: "backlog", Capabilities: []string{"github:issues:read", "github:issues:write", "github:milestones:write"}}

	got := grantMap(RoleRunnerGrants(bindings, "example-org/example-project", "service", role, caps, nil))
	want := map[string]string{
		"repo:push":               "example-org/example-project/service",
		"github:pr:write":         "example-org/example-project/service",
		"github:issues:read":      "example-org/backlog",
		"github:issues:write":     "example-org/backlog",
		"github:milestones:write": "example-org/backlog",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grants = %v, want %v", got, want)
	}
}

// TestRoleRunnerGrants_UnboundBacklogFailsClosed: with no repos[] credential
// for the backlog repository, the backlog-family capabilities get no grant at
// all. They must never fall back to the project repository's credential, which
// belongs to the other provider.
func TestRoleRunnerGrants_UnboundBacklogFailsClosed(t *testing.T) {
	bindings := []RepoBinding{
		{Owner: "example-org/example-project", Name: "service", TokenRef: "example-org/example-project/service"},
		{Owner: "example-org", Name: "backlog"}, // configured without a credential
	}
	caps := []string{"repo:push", "github:issues:write"}
	role := &BacklogRole{Owner: "example-org", Name: "backlog", Capabilities: []string{"github:issues:write"}}

	got := grantMap(RoleRunnerGrants(bindings, "example-org/example-project", "service", role, caps, nil))
	if ref, ok := got["github:issues:write"]; ok {
		t.Fatalf("github:issues:write granted %q; an unbound backlog role must grant nothing", ref)
	}
	if got["repo:push"] != "example-org/example-project/service" {
		t.Fatalf("repo:push granted %q, want the project credential", got["repo:push"])
	}

	// An explicit credentials: override still sources the capability.
	overrides := []Grant{{Capability: "github:issues:write", Ref: "credential:github:issues:write"}}
	got = grantMap(RoleRunnerGrants(bindings, "example-org/example-project", "service", role, caps, overrides))
	if got["github:issues:write"] != "credential:github:issues:write" {
		t.Fatalf("override not applied: %v", got)
	}
}

// TestRoleRunnerGrants_NilRoleIsRunnerGrants keeps every existing topology
// byte-identical: without a backlog role the grants equal RunnerGrants'.
func TestRoleRunnerGrants_NilRoleIsRunnerGrants(t *testing.T) {
	caps := []string{"repo:push", "github:issues:write", "github:pr:write"}
	overrides := []Grant{{Capability: "agent:model", Ref: "credential:agent:model"}}
	for _, tc := range []struct{ owner, name string }{{"", ""}, {"alpha-org", "site"}, {"bravo-org", "app"}, {"unknown", "repo"}} {
		want := RunnerGrants(twoRepoBindings, tc.owner, tc.name, caps, overrides)
		got := RoleRunnerGrants(twoRepoBindings, tc.owner, tc.name, nil, caps, overrides)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("(%q,%q): RoleRunnerGrants(nil) = %v, want %v", tc.owner, tc.name, got, want)
		}
	}
}
