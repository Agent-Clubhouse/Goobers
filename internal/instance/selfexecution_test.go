package instance

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/runnersolve"
)

func TestSelfExecutionPolicyLoadAndSolve(t *testing.T) {
	for _, policy := range []string{"allow", "deny", "invalid"} {
		t.Run(policy, func(t *testing.T) {
			cfg, err := LoadConfig(writeInstanceYAML(t, legacyRunnerBody+"\nplacement:\n  selfExecution: "+policy+"\n"))
			if policy == "invalid" {
				if err == nil || !strings.Contains(err.Error(), "placement.selfExecution") {
					t.Fatalf("invalid policy: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			result := runnersolve.Solve(cfg.PlacementInventory(""), []runnersolve.StageRequirement{{Stage: "build"}})
			if (len(result.Unsatisfiable()) > 0) != (policy == "deny") {
				t.Fatalf("policy %s: %+v", policy, result)
			}
			if policy == "deny" && !strings.Contains(result.Stages[0].Unsat.Diagnostic, `stage "build"`) {
				t.Fatal(result)
			}
		})
	}
	if (*Config)(nil).SelfExecutionPolicy() != "allow" {
		t.Fatal("default changed")
	}
}

func TestSelfExecutionAccounting(t *testing.T) {
	cfg := &Config{Placement: &PlacementConfig{SelfExecution: "deny"}}
	if cfg.SelfExecutionStats().Observed {
		t.Fatal("offline config claims live observation")
	}
	cfg.StartSelfExecutionAccounting()
	cfg.ObserveSelfExecution(true)
	stats := cfg.SelfExecutionStats()
	if !stats.Observed || stats.Policy != "deny" || stats.Placements != 0 || stats.Refusals != 1 {
		t.Fatal(stats)
	}
}
