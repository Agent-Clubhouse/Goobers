package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/platform/proc"
	"github.com/goobers/goobers/internal/runtimeplan"
)

func TestRuntimeLifecycleProbeBoundary(t *testing.T) {
	old := runtimeCleanupProbe
	t.Cleanup(func() { runtimeCleanupProbe = old })
	calls := 0
	runtimeCleanupProbe = func(context.Context) proc.CleanupProbe {
		calls++
		return proc.CleanupProbe{Code: "owned_cleanup_observed", Outcome: "passed"}
	}
	process := runtimeplan.Process{PID: 7, UID: "reporter"}
	stages := []runtimePreflightStage{{Name: "local", Runner: runtimePreflightStageRunner{Selected: &runtimePreflightRunner{Kind: "self"}}}, {Name: "remote", Runner: runtimePreflightStageRunner{Selected: &runtimePreflightRunner{Kind: "image"}}}}
	for _, probe := range []bool{false, true} {
		got, checks := runtimeLifecycleChecks(context.Background(), stages, process, probe)
		if got.Process != process || got.Fixture.AuthorizesWriter || got.Fixture.RequiredGuaranteeSatisfied != probe {
			t.Fatalf("fixture boundary lost: %+v", got)
		}
		if !probe && calls != 0 {
			t.Fatal("metadata-only report spawned fixture")
		}
		for _, target := range got.Targets {
			if target.Cleanup.RequiredGuaranteeSatisfied || target.Cleanup.AuthorizesWriter {
				t.Fatalf("local evidence authorized target: %+v", target)
			}
		}
		if len(checks) != 3 || checks[1].Code != "cleanup_host_owned" || checks[2].Code != "remote_cleanup_unobservable" {
			t.Fatal(checks)
		}
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d", calls)
	}
}

func TestRuntimeLifecycleProbeFailureCannotSatisfyGuarantee(t *testing.T) {
	old := runtimeCleanupProbe
	t.Cleanup(func() { runtimeCleanupProbe = old })
	for _, code := range []string{"cleanup_probe_failed", "cleanup_probe_canceled", "cleanup_guarantee_unavailable"} {
		runtimeCleanupProbe = func(context.Context) proc.CleanupProbe { return proc.CleanupProbe{Code: code, Outcome: "unsupported"} }
		got, checks := runtimeLifecycleChecks(context.Background(), nil, runtimeplan.Process{}, true)
		if got.Fixture.RequiredGuaranteeSatisfied || got.Fixture.AuthorizesWriter || checks[0].Code != code {
			t.Fatal(got)
		}
	}
}

func TestRuntimeTargetCleanupUnavailable(t *testing.T) {
	for _, stage := range []runtimePreflightStage{
		{Runner: runtimePreflightStageRunner{Outcome: "unsupported"}},
		{Runner: runtimePreflightStageRunner{Selected: &runtimePreflightRunner{Kind: "unknown"}}},
	} {
		if got := runtimeTargetCleanup(stage); got.Code != "cleanup_guarantee_unavailable" || got.RequiredGuaranteeSatisfied {
			t.Fatal(got)
		}
	}
	if got := runtimeTargetCleanup(runtimePreflightStage{}); got.Code != "remote_cleanup_unobservable" {
		t.Fatal(got)
	}
}
