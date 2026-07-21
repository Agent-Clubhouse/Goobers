package credentials

import (
	"reflect"
	"testing"
)

func grantMap(grants []Grant) map[string]string {
	m := make(map[string]string, len(grants))
	for _, g := range grants {
		m[g.Capability] = g.Ref
	}
	return m
}

var twoRepoBindings = []RepoBinding{
	{Owner: "alpha-org", Name: "site", TokenRef: "alpha-org/site"},
	{Owner: "bravo-org", Name: "app", TokenRef: "bravo-org/app"},
}

// TestRunnerGrants_RoutesToGagglesOwnRepo is the core MGV-5 (#1012) invariant:
// a gaggle's repo capabilities resolve to that gaggle's own repo token, not the
// first/instance-wide one. Gaggle A (alpha-org/site) gets ref alpha-org/site;
// gaggle B (bravo-org/app) gets bravo-org/app — never each other's.
func TestRunnerGrants_RoutesToGagglesOwnRepo(t *testing.T) {
	caps := []string{"repo:push", "github:pr:write"}

	a := grantMap(RunnerGrants(twoRepoBindings, "alpha-org", "site", caps, nil))
	for _, c := range caps {
		if a[c] != "alpha-org/site" {
			t.Errorf("gaggle A: %s granted %q, want alpha-org/site", c, a[c])
		}
	}
	b := grantMap(RunnerGrants(twoRepoBindings, "bravo-org", "app", caps, nil))
	for _, c := range caps {
		if b[c] != "bravo-org/app" {
			t.Errorf("gaggle B: %s granted %q, want bravo-org/app", c, b[c])
		}
	}
	// The whole point: no capability in A's grants routes to B's repo token.
	for c, ref := range a {
		if ref == "bravo-org/app" {
			t.Errorf("gaggle A leaked gaggle B's token ref via %s", c)
		}
	}
}

// TestRunnerGrants_FallsBackToFirstRepo covers the single-repo / legacy /
// instance-level caller: an empty (owner,name), or one matching no binding,
// backs repo capabilities with the FIRST binding — byte-identical to the
// pre-scoping "first repo's token backs every credentialed capability" default.
func TestRunnerGrants_FallsBackToFirstRepo(t *testing.T) {
	caps := []string{"repo:push", "github:pr:write"}

	for _, tc := range []struct{ owner, name string }{
		{"", ""},            // instance-level caller
		{"unknown", "repo"}, // a gaggle whose repo has no configured token
	} {
		got := grantMap(RunnerGrants(twoRepoBindings, tc.owner, tc.name, caps, nil))
		for _, c := range caps {
			if got[c] != "alpha-org/site" {
				t.Errorf("(%q,%q): %s granted %q, want first-repo alpha-org/site", tc.owner, tc.name, c, got[c])
			}
		}
	}
}

// TestRunnerGrants_OverrideBeatsRepoDefault: a per-capability override
// (agent:model from its own shared token, #287) replaces the repo-default grant
// for that capability and adds a new one, while leaving other repo capabilities
// on the gaggle's repo token.
func TestRunnerGrants_OverrideBeatsRepoDefault(t *testing.T) {
	caps := []string{"repo:push", "agent:model"}
	overrides := []Grant{{Capability: "agent:model", Ref: "credential:agent:model"}}

	got := grantMap(RunnerGrants(twoRepoBindings, "bravo-org", "app", caps, overrides))
	if got["repo:push"] != "bravo-org/app" {
		t.Errorf("repo:push granted %q, want bravo-org/app", got["repo:push"])
	}
	if got["agent:model"] != "credential:agent:model" {
		t.Errorf("agent:model granted %q, want the shared override ref", got["agent:model"])
	}
}

// TestRunnerGrants_NoReposLeavesOnlyOverrides: with no repo bindings there is no
// repo-default token; only override-sourced capabilities are granted.
func TestRunnerGrants_NoReposLeavesOnlyOverrides(t *testing.T) {
	overrides := []Grant{{Capability: "agent:model", Ref: "credential:agent:model"}}
	got := RunnerGrants(nil, "", "", []string{"repo:push", "agent:model"}, overrides)
	want := []Grant{{Capability: "agent:model", Ref: "credential:agent:model"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("grants = %+v, want only the override %+v", got, want)
	}
}

// TestRunnerGrants_DeterministicOrder: repo-capability defaults come first in
// input order, then override-only capabilities — stable across builds.
func TestRunnerGrants_DeterministicOrder(t *testing.T) {
	caps := []string{"repo:push", "github:pr:write"}
	overrides := []Grant{{Capability: "agent:model", Ref: "credential:agent:model"}}
	got := RunnerGrants(twoRepoBindings, "alpha-org", "site", caps, overrides)
	wantOrder := []string{"repo:push", "github:pr:write", "agent:model"}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d grants, want %d", len(got), len(wantOrder))
	}
	for i, c := range wantOrder {
		if got[i].Capability != c {
			t.Errorf("grant[%d] = %s, want %s", i, got[i].Capability, c)
		}
	}
}
