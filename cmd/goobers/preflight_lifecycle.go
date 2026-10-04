package main

import (
	"context"
	"io"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/platform/proc"
	"github.com/goobers/goobers/internal/runtimeplan"
	"github.com/goobers/goobers/internal/workerhost"
)

type runtimePreflightLifecycle struct {
	Process            runtimeplan.Process             `json:"process"`
	CancellationOwner  string                          `json:"cancellationOwner"`
	Cancellation       string                          `json:"cancellation"`
	Drain              string                          `json:"drain"`
	WorkerDrainDefault string                          `json:"workerDrainDefault"`
	DisposalTimeout    string                          `json:"disposalTimeout"`
	Fixture            runtimeplan.Cleanup             `json:"fixture"`
	Probe              proc.CleanupProbe               `json:"probe"`
	Targets            []runtimePreflightCleanupTarget `json:"targets"`
	Source             runtimeplan.Source              `json:"source"`
}

type runtimePreflightCleanupTarget struct {
	Stage   string              `json:"stage"`
	Cleanup runtimeplan.Cleanup `json:"cleanup"`
}

var runtimeCleanupProbe = proc.ProbeCleanup

func runtimeLifecycleChecks(ctx context.Context, stages []runtimePreflightStage, process runtimeplan.Process, probe bool) (runtimePreflightLifecycle, []runtimePreflightCheck) {
	result := runtimePreflightLifecycle{
		Process:            process,
		CancellationOwner:  "attempt runner and its proc.Tree supervisor; remote execution host owns remote processes",
		Cancellation:       "graceful daemon drain preserves in-flight attempts; hard stop cancels owned attempts; harness timeout/cancellation kills its proc.Tree; configured timeout inputs are in execution.plan.timeouts and stages[].settings; effective live deadlines remain unobservable",
		Drain:              "daemon --drain-timeout defaults to unbounded; repeated shutdown signal or configured drain expiry requests hard stop; actual daemon/worker launch flags are unobservable; checkpoint recovery is unchanged",
		WorkerDrainDefault: workerhost.DefaultDrainTimeout.String(),
		DisposalTimeout:    dispatcher.DefaultDisposalTimeout.String(),
		Fixture:            runtimeplan.CleanupObservation("owned", "reporting process proc.Tree supervisor", false),
		Probe:              proc.CleanupProbe{Code: "cleanup_probe_not_requested", Outcome: "unobservable", Detail: "metadata only; use --check-readiness for a controlled local force-stop fixture"},
		Source:             runtimeplan.Source{Fidelity: "static", Detail: "supervisor contracts and defaults; local fixture evidence cannot attest daemon/worker identity, remote cleanup or authorize another writer"},
	}
	if probe {
		result.Probe = runtimeCleanupProbe(ctx)
		result.Fixture = runtimeplan.CleanupObservation("owned", result.Fixture.Owner, result.Probe.Outcome == "passed")
	}
	probeFidelity := "unobservable"
	if probe {
		probeFidelity = "observed"
	}
	checks := []runtimePreflightCheck{{Category: "cleanup_guarantee", Code: result.Probe.Code, Outcome: result.Probe.Outcome, Detail: result.Probe.Detail, Source: runtimePreflightFactSrc{Fidelity: probeFidelity, Detail: "controlled reporting-process fixture only"}}}
	for _, stage := range stages {
		cleanup := runtimeTargetCleanup(stage)
		result.Targets = append(result.Targets, runtimePreflightCleanupTarget{stage.Name, cleanup})
		checks = append(checks, runtimePreflightCheck{Category: "remote_cleanup", Stage: stage.Name, Code: cleanup.Code, Outcome: cleanup.Outcome, Detail: cleanup.Detail, Source: runtimePreflightFactSrc{Fidelity: "unobservable", Detail: "no target cleanup observation or provider mutation"}})
	}
	return result, checks
}

func runtimeTargetCleanup(stage runtimePreflightStage) runtimeplan.Cleanup {
	if stage.Runner.Outcome == "unsupported" {
		return runtimeplan.CleanupObservation("unsupported", "unavailable runner", false)
	}
	if stage.Runner.Selected == nil {
		return runtimeplan.CleanupObservation("unobservable", "target execution owner unobserved", false)
	}
	switch stage.Runner.Selected.Kind {
	case "self":
		return runtimeplan.CleanupObservation("host-owned", "daemon attempt runner and process supervisor", false)
	case "image", "deployment":
		return runtimeplan.CleanupObservation("unobservable", "dispatcher disposal and remote worker host; remote completion unobserved", false)
	default:
		return runtimeplan.CleanupObservation("unsupported", "unsupported execution host", false)
	}
}

func printRuntimeLifecycle(w io.Writer, lifecycle runtimePreflightLifecycle) {
	pf(w, "  lifecycle cancellation owner: %s\n", lifecycle.CancellationOwner)
	pf(w, "    cancellation: %s\n", lifecycle.Cancellation)
	pf(w, "    drain: %s\n", lifecycle.Drain)
	pf(w, "    worker drain default=%s remote disposal timeout=%s (target launch settings unobservable)\n", lifecycle.WorkerDrainDefault, lifecycle.DisposalTimeout)
	pf(w, "    fixture: %s ownership=%s requiredGuaranteeSatisfied=%t authorizesWriter=%t\n", lifecycle.Probe.Code, lifecycle.Fixture.Ownership, lifecycle.Fixture.RequiredGuaranteeSatisfied, lifecycle.Fixture.AuthorizesWriter)
}
