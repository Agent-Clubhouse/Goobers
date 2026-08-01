package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
)

// githubToolPreflighter is implemented by harness adapters that can verify a
// declared capability's tools are actually registered by the live harness
// process, not just requested (#2184/#2194). An optional interface (mirrors
// harness.ConfigValidator) since only the Copilot adapter's github-mcp-server
// integration has this failure mode today.
type githubToolPreflighter interface {
	PreflightGithubTools(ctx context.Context, tools []string, required []string) error
}

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
//
// Also verifies, per distinct (harness, tools, capabilities) combination an
// agentic TASK declares, that any write-capable tool its capabilities require
// is actually registered by the live harness — not just requested (#2184:
// the tool was correctly listed in the allowlist but the harness silently
// never registered it, and the agent correctly never saw it, so it had
// nothing to self-report). This check is per-combination, not per-harness
// like the version/auth check above, since different tasks on the same
// harness kind can declare different tool/capability sets.
func preflightAgenticHarnesses(goobers map[string]apiv1.GooberSpec, workflows []apiv1.Workflow) (harnessPreflightInfo, error) {
	seen := map[apiv1.Harness]bool{}
	seenTools := map[string]bool{}
	info := make(harnessPreflightInfo)
	preflight := func(wfName, stageName, gooberName string, capabilities []string) error {
		spec, ok := goobers[gooberName]
		if !ok {
			return nil
		}
		h := spec.Harness
		if h == "" {
			h = apiv1.HarnessCopilot
		}
		var adapter harness.Adapter
		if !seen[h] {
			seen[h] = true
			var err error
			adapter, err = harnessAdapterFor(h)
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
		}
		required := harness.RequiredGithubTools(capabilities)
		if len(required) == 0 {
			return nil
		}
		signature := string(h) + "|" + strings.Join(spec.Tools, ",") + "|" + strings.Join(required, ",")
		if seenTools[signature] {
			return nil
		}
		seenTools[signature] = true
		if adapter == nil {
			var err error
			adapter, err = harnessAdapterFor(h)
			if err != nil {
				return fmt.Errorf("workflow %q stage %q: %w", wfName, stageName, err)
			}
		}
		preflighter, ok := adapter.(githubToolPreflighter)
		if !ok {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), harnessPreflightTimeout)
		err := preflighter.PreflightGithubTools(ctx, spec.Tools, required)
		cancel()
		if err != nil {
			return fmt.Errorf("workflow %q stage %q: %w", wfName, stageName, err)
		}
		return nil
	}
	for _, wf := range workflows {
		for _, task := range wf.Spec.Tasks {
			if task.Type != apiv1.TaskAgentic {
				continue
			}
			capabilities := append([]string(nil), task.Capabilities...)
			sort.Strings(capabilities)
			if err := preflight(wf.Name, task.Name, task.Goober, capabilities); err != nil {
				return nil, err
			}
		}
		for _, gate := range wf.Spec.Gates {
			if gate.Evaluator != apiv1.EvaluatorAgentic || gate.Agentic == nil {
				continue
			}
			if err := preflight(wf.Name, gate.Name, gate.Agentic.Goober, nil); err != nil {
				return nil, err
			}
		}
	}
	return info, nil
}
