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

// harnessPreflightFailures maps a harness to why its live preflight probe
// could not be confirmed usable (bad/expired credential, exhausted quota,
// non-responsive CLI). Distinct from the error preflightAgenticHarnesses
// itself returns, which is reserved for structural problems (an unregistered
// harness, adapter construction failure) — those indicate a config/code
// defect and still fail startup outright. A live-probe failure is scoped to
// that one harness: other, healthy harnesses and the workflows that use them
// are unaffected.
type harnessPreflightFailures map[apiv1.Harness]error

// preflightAgenticHarnesses preflights every distinct harness an agentic task
// or reviewer gate references. Deterministic-only workflows reference no
// harness and are skipped.
//
// Wired into daemon startup (buildSchedulerSetup, shared by `goobers up` and
// `goobers run`) so a missing/broken harness is caught before any worktree,
// claim, or run-journal side effect — not several stages in, as a burned
// agentic attempt with the root cause buried in a harness transcript (#238).
// The adapter (via adapterFor) carries the auth probe and the instance's
// configured environment passthrough, so startup checks the same ambient auth
// environment as a dispatched run and the operator's harnessCommand override
// (#2483); each preflight is bounded by harnessPreflightTimeout so a hung CLI
// or network can't hang startup.
//
// A harness's live probe failing (missing binary, non-responsive, signed
// out, over quota, or a version check that returns no version) is recorded
// in the returned harnessPreflightFailures map, not treated as fatal here —
// #2812: one broken harness (e.g. Copilot over quota) must not block startup
// for every other, healthy harness's workflows. The caller is responsible
// for surfacing these as warnings and for any run that actually dispatches
// through the broken harness failing on its own, the same way it always has.
// Only a structural failure — harnessAdapterFor itself erroring, which means
// the harness isn't wired up at all rather than merely unreachable right
// now — still fails this call outright.
func preflightAgenticHarnesses(goobers map[string]apiv1.GooberSpec, workflows []apiv1.Workflow, envPassthrough []string, harnessCommand map[string][]string) (harnessPreflightInfo, harnessPreflightFailures, error) {
	seen := map[apiv1.Harness]bool{}
	info := make(harnessPreflightInfo)
	failures := make(harnessPreflightFailures)
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
		adapter, err := harnessAdapterFor(h, envPassthrough, harnessCommand)
		if err != nil {
			return fmt.Errorf("workflow %q stage %q: %w", wfName, stageName, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), harnessPreflightTimeout)
		result, err := adapter.Preflight(ctx)
		cancel()
		if err != nil {
			failures[h] = fmt.Errorf("workflow %q stage %q harness preflight: %w", wfName, stageName, err)
			return nil
		}
		if result.Version == "" {
			failures[h] = fmt.Errorf("workflow %q stage %q harness preflight: %s returned no version", wfName, stageName, adapter.Name())
			return nil
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
				return nil, nil, err
			}
		}
		for _, gate := range wf.Spec.Gates {
			if gate.Evaluator != apiv1.EvaluatorAgentic || gate.Agentic == nil {
				continue
			}
			if err := preflight(wf.Name, gate.Name, gate.Agentic.Goober); err != nil {
				return nil, nil, err
			}
		}
	}
	return info, failures, nil
}
