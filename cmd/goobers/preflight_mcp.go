package main

import (
	"context"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimeplan"
)

type runtimePreflightMCP struct {
	Stage   string                  `json:"stage"`
	Harness string                  `json:"harness"`
	Process runtimeplan.Process     `json:"process"`
	Facts   harness.MCPDiagnostic   `json:"facts"`
	Source  runtimePreflightFactSrc `json:"source"`
}

func runtimePreflightMCPChecks(ctx context.Context, cfg *instance.Config, goobers map[string]apiv1.GooberSpec, stages []runtimePreflightStage, process runtimeplan.Process, probe bool) ([]runtimePreflightMCP, []runtimePreflightCheck) {
	if !probe {
		return nil, nil
	}
	var rows []runtimePreflightMCP
	var checks []runtimePreflightCheck
	self, _ := os.Executable()
	for _, stage := range stages {
		if stage.Harness == "" {
			continue
		}
		goober := goobers[stage.Goober]
		facts := harness.ConfiguredMCPDiagnostics(goober.MCPServers, goober.Tools)
		adapter, err := runtimeReadinessAdapterFor(apiv1.Harness(stage.Harness), harnessEnvironmentPolicy(cfg.Runner), cfg.Runner.HarnessCommand, nil)
		if supported, ok := adapter.(*harness.CopilotAdapter); err == nil && ok {
			supported.SelfBin = self
			facts = supported.ProbeMCPReadiness(ctx, harness.RunRequest{MCPServers: goober.MCPServers, Tools: goober.Tools,
				Envelope: apiv1.InvocationEnvelope{TaskID: stage.Name, Capabilities: stage.CredentialCapabilities}})
		}
		for _, fact := range facts {
			fidelity := "unobservable"
			if fact.Source == "disposable-adapter-session" {
				fidelity = "observed"
			}
			source := runtimePreflightFactSrc{Fidelity: fidelity, Detail: "reporting-process disposable session; target worker identity and environment remain unobservable"}
			rows = append(rows, runtimePreflightMCP{stage.Name, stage.Harness, process, fact, source})
			checks = append(checks, runtimePreflightCheck{Category: "mcp", Process: &process, Stage: stage.Name, Code: fact.Category,
				Outcome: runtimeMCPOutcome(fact.Category), Detail: fact.Server + ": connection=" + fact.Connection + ", inventory=" + fact.Inventory + ", authorization=" + fact.Authorization, Source: source})
		}
	}
	return rows, checks
}

func runtimeMCPOutcome(category string) string {
	switch category {
	case "ready":
		return "ready"
	case "check_unobservable", "authorization_unobservable":
		return "unobservable"
	default:
		return "failed"
	}
}
