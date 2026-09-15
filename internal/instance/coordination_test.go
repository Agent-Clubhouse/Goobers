package instance

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/coordination"
)

func TestCoordinationConfigRequiresExactExplicitTargets(t *testing.T) {
	parent := coordination.Repository{Provider: "github", Owner: "acme", Name: "planning"}
	target := coordination.Repository{Provider: "github", Owner: "acme", Name: "core"}
	for _, mode := range []string{"valid", "missing repo", "wrong host", "ambiguous", "implicit approval", "duplicate owner", "bad digest"} {
		t.Run(mode, func(t *testing.T) {
			a := coordination.Authority{Name: "coordinator", ParentRepo: parent, Targets: []coordination.Target{{Repository: target, Approval: "reviewed-plan"}}}
			cfg := Config{Repos: []RepoRef{{Provider: "github", Owner: "acme", Name: "planning"}, {Provider: "github", Owner: "acme", Name: "core"}}, Coordination: &coordination.Configuration{Gaggles: []coordination.Authority{a}}}
			switch mode {
			case "missing repo":
				cfg.Repos = cfg.Repos[:1]
			case "wrong host":
				cfg.Repos[1].BaseURL = "https://other.invalid"
			case "ambiguous":
				cfg.Repos = append(cfg.Repos, cfg.Repos[1])
			case "implicit approval":
				cfg.Coordination.Gaggles[0].Targets[0].Approval = ""
			case "duplicate owner":
				cfg.Coordination.Gaggles = append(cfg.Coordination.Gaggles, a)
			case "bad digest":
				cfg.Coordination.Gaggles[0].ApprovedPlans = map[string]string{"plan": "approved"}
			}
			err := cfg.validateCoordination()
			if (err == nil) != (mode == "valid") {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}

func TestCoordinationCredentialCannotBeStageGranted(t *testing.T) {
	path := writeInstanceYAML(t, `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - {provider: github, owner: acme, name: core, token: {env: UNUSED_COORDINATION_TOKEN}}
credentials:
  - capability: coordination:write
    token: {env: NEVER_RESOLVE_THIS}
`)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "runner-owned") {
		t.Fatalf("stage grant accepted: %v", err)
	}
}
