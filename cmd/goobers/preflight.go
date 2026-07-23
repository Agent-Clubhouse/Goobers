package main

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
)

// preflightHarnesses is the seam buildSchedulerSetup calls to preflight agentic
// harnesses at startup (#238). It defaults to the real preflightAgenticHarnesses;
// the cmd/goobers test suite replaces it with a no-op in TestMain, since those
// tests drive `goobers up`/`run` against configs with agentic stages but have no
// real, installed Copilot CLI (LookPath would fail in CI). The real logic is
// tested directly in preflight_test.go.
var preflightHarnesses = preflightAgenticHarnesses

type harnessPreflightInfo map[apiv1.Harness]harness.PreflightInfo

// preflightAgenticHarnesses preflights every distinct harness an agentic task
// or reviewer gate references, failing closed on the first unusable one
// (missing binary, non-responsive, signed out, or missing a version) with that
// harness's own actionable message. Deterministic-only workflows reference no
// harness and are skipped.
//
// Wired into daemon startup (buildSchedulerSetup, shared by `goobers up` and
// `goobers run`) so a missing/broken harness is caught before any worktree,
// claim, or run-journal side effect — not several stages in, as a burned
// agentic attempt with the root cause buried in a harness transcript (#238).
// The adapter (via adapterFor) carries the auth probe, so a signed-out harness
// is caught here at startup too, not just under `validate --check-harness`
// (#238); each preflight is bounded by harnessPreflightTimeout so a hung CLI or
// network — now that the probe makes a real API round-trip — can't hang startup.
func preflightAgenticHarnesses(goobers map[string]apiv1.GooberSpec, workflows []apiv1.Workflow) (harnessPreflightInfo, error) {
	seen := map[apiv1.Harness]bool{}
	info := make(harnessPreflightInfo)
	preflight := func(wfName, stageName, gooberName string) error {
		spec, ok := goobers[gooberName]
		if !ok {
			return nil
		}
		h := spec.Harness
		if h == "" {
			h = apiv1.HarnessCopilot
		}
		if seen[h] {
			return nil
		}
		seen[h] = true
		adapter, err := harnessAdapterFor(h)
		if err != nil {
			return fmt.Errorf("workflow %q stage %q: %w", wfName, stageName, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), harnessPreflightTimeout)
		result, err := adapter.Preflight(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("workflow %q stage %q harness preflight: %w", wfName, stageName, err)
		}
		if result.Version == "" {
			return fmt.Errorf("workflow %q stage %q harness preflight: %s returned no version", wfName, stageName, adapter.Name())
		}
		info[h] = result
		return nil
	}
	for _, wf := range workflows {
		for _, task := range wf.Spec.Tasks {
			if task.Type != apiv1.TaskAgentic {
				continue
			}
			if err := preflight(wf.Name, task.Name, task.Goober); err != nil {
				return nil, err
			}
		}
		for _, gate := range wf.Spec.Gates {
			if gate.Evaluator != apiv1.EvaluatorAgentic || gate.Agentic == nil {
				continue
			}
			if err := preflight(wf.Name, gate.Name, gate.Agentic.Goober); err != nil {
				return nil, err
			}
		}
	}
	return info, nil
}
