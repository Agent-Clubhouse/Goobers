package instance

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/runnersolve"
)

func TestIsolationMandatesValidateUnionAndClosedVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mandates []IsolationMandate
		want     string
	}{
		{"empty", nil, "at least one mandate"},
		{"unknown class", []IsolationMandate{{Match: IsolationMatch{StageClass: "*"}}}, "stageClass"},
		{"empty effects", []IsolationMandate{{Match: IsolationMatch{StageClass: "agentic"}}}, "at least one effect"},
		{"unknown effect", []IsolationMandate{{Match: IsolationMatch{StageClass: "agentic"}, Restrictions: []RunnerRestriction{"network:any"}}}, "unknown"},
		{"duplicate effect", []IsolationMandate{{Match: IsolationMatch{StageClass: "agentic"}, Restrictions: []RunnerRestriction{RunnerRestrictionTmpEphemeral, RunnerRestrictionTmpEphemeral}}}, "duplicate"},
		{"union impossible", []IsolationMandate{
			{Match: IsolationMatch{StageClass: "agentic"}, Restrictions: []RunnerRestriction{RunnerRestrictionTmpEphemeral}},
			{Match: IsolationMatch{StageClass: "agentic"}, Restrictions: []RunnerRestriction{RunnerRestrictionNetworkAllowlist}},
		}, "isolation.mandates for agentic"},
		{"independent classes", []IsolationMandate{
			{Match: IsolationMatch{StageClass: "agentic"}, Restrictions: []RunnerRestriction{RunnerRestrictionTmpEphemeral}},
			{Match: IsolationMatch{StageClass: "deterministic"}, Restrictions: []RunnerRestriction{RunnerRestrictionNetworkAllowlist}},
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Isolation: &IsolationConfig{Mandates: tc.mandates}, Runners: []RunnerEntry{
				{Name: "temp", Host: "image:temp", Restrictions: []RunnerRestriction{RunnerRestrictionTmpEphemeral}},
				{Name: "network", Host: "image:network", Restrictions: []RunnerRestriction{RunnerRestrictionNetworkAllowlist}},
			}}
			err := cfg.validateIsolation()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %q", err, tc.want)
			}
		})
	}
}

func TestIsolationMandatesStrictLoadAndLegacySelfRefusal(t *testing.T) {
	_, err := LoadConfig(writeInstanceYAML(t, legacyRunnerBody+`
isolation:
  mandates:
    - match: {stageClass: agentic}
      restrictions: [network:allowlist]
`))
	if err == nil || !strings.Contains(err.Error(), "network:allowlist") || !strings.Contains(err.Error(), "self") {
		t.Fatalf("legacy self escaped isolation floor: %v", err)
	}
	_, err = LoadConfig(writeInstanceYAML(t, legacyRunnerBody+`
isolation:
  mandates:
    - match: {stageClas: agentic}
      restrictions: [network:allowlist]
`))
	if err == nil {
		t.Fatal("unknown selector accepted")
	}
}

func TestIsolationInventoryClassFloorIsStrengthenOnly(t *testing.T) {
	cfg := &Config{Isolation: &IsolationConfig{Mandates: []IsolationMandate{{Match: IsolationMatch{StageClass: "agentic"}, Restrictions: []RunnerRestriction{RunnerRestrictionTmpEphemeral}}}}, Runners: []RunnerEntry{
		{Name: "self", Host: "self"},
		{Name: "safe", Host: "image:safe", Restrictions: []RunnerRestriction{RunnerRestrictionTmpEphemeral, RunnerRestrictionNetworkAllowlist}},
	}}
	inv := cfg.PlacementInventory("")
	cfg.Isolation.Mandates = append(cfg.Isolation.Mandates, cfg.Isolation.Mandates[0])
	if got := cfg.PlacementInventory("").ClassMandates["agentic"]; len(got) != 1 {
		t.Fatalf("effective union duplicated a repeated mandate: %v", got)
	}
	rows := []runnersolve.StageRequirement{
		{Stage: "agent", StageClass: "agentic", Restrictions: []string{"network:allowlist"}},
		{Stage: "script", StageClass: "deterministic"},
		{Stage: "gate", StageClass: "agentic", ControlPlane: true},
	}
	result := runnersolve.Solve(inv, rows)
	if len(result.Stages[0].Eligible) != 1 || result.Stages[0].Eligible[0] != "safe" {
		t.Fatalf("agent: %+v", result.Stages[0])
	}
	if len(result.Stages[1].Eligible) != 2 {
		t.Fatalf("deterministic was strengthened: %+v", result.Stages[1])
	}
	if result.Stages[2].Unsat == nil {
		t.Fatal("control-plane gate borrowed remote protections")
	}
	local := runnersolve.SolveExecutable(inv, rows)
	if local.Stages[0].Unsat == nil {
		t.Fatal("local fallback dropped the class floor")
	}
}
