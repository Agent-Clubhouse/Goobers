package main

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
)

// agentModelCredentialResolver builds a resolver for the instance's
// configured agent:model credential (file/keychain/store), for handing to a
// harness's Preflight sign-in probe — which has no RunRequest and so cannot
// reach the normal per-stage credentialEnv resolution path (#3341: without
// this, a file/keychain-sourced agent:model PAT is invisible to the sign-in
// probe, which then silently falls back to whatever the CLI has cached from
// its own prior interactive login — a different, possibly wrong, account).
//
// forHarness selects among multiple configured agent:model grants the same
// way buildGooberCredentialGrants does at run time (#5148): a grant scoped to
// forHarness wins; absent that, an unscoped grant (one with no harness set,
// backing every harness) is used. Pass "" to require an unscoped grant only —
// compiledMachinesWithGooberDigestsAndWarnings' single shared model-discovery
// pass covers every goober's harness at once and has no one harness to prefer,
// so it keeps the pre-#5148 behavior of resolving only the shared grant; a
// mixed-harness instance with no unscoped agent:model grant degrades to no
// online model-name validation there (deferModelDiscovery / the config's own
// declared model still work), which is no worse than before this field
// existed. `validate --check-harness` (checkHarnessesAtSources) calls this
// once per distinct harness instead, so it always gets that harness's actual
// grant.
//
// Returns a nil func and an empty label when no matching agent:model grant is
// configured, leaving preflight to reflect only ambient env or the CLI's own
// cached login, unchanged from before this resolver existed. label names
// which grant would be used (never the resolved value) for --check-harness's
// reporting.
func agentModelCredentialResolver(cfg *instance.Config, stores credentials.StoreResolver, forHarness apiv1.Harness) (resolve func(ctx context.Context) (string, error), label string, err error) {
	var scoped, unscoped *instance.CredentialGrant
	for i := range cfg.Credentials {
		grant := &cfg.Credentials[i]
		if grant.Capability != string(capability.AgentModel) {
			continue
		}
		if forHarness != "" && grant.Harness == string(forHarness) && scoped == nil {
			scoped = grant
		} else if grant.Harness == "" && unscoped == nil {
			unscoped = grant
		}
	}
	grant := scoped
	switch {
	case grant != nil:
		label = fmt.Sprintf("credentials[] agent:model scoped to harness %q", grant.Harness)
	case unscoped != nil:
		grant = unscoped
		label = "credentials[] agent:model (unscoped)"
	default:
		return nil, "", nil
	}
	key := string(capability.AgentModel)
	if grant.Harness != "" {
		key = credentials.HarnessScopedCapability(key, grant.Harness)
	}
	resolver, err := credentials.NewResolverWithStores(
		[]credentials.TokenRef{grant.Token.CredentialTokenRef(key)},
		stores,
	)
	if err != nil {
		return nil, "", err
	}
	return func(ctx context.Context) (string, error) {
		return resolver.Resolve(ctx, key)
	}, label, nil
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
// The adapter (via adapterFor) carries the auth probe and the instance's
// configured environment passthrough, so startup checks the same ambient auth
// environment as a dispatched run and the operator's harnessCommand override
// (#2483); each preflight is bounded by harnessPreflightTimeout so a hung CLI
// or network can't hang startup.
func preflightAgenticHarnesses(goobers map[string]apiv1.GooberSpec, workflows []apiv1.Workflow, envPassthrough []string, harnessCommand map[string][]string, modelCredential func(ctx context.Context) (string, error)) (harnessPreflightInfo, error) {
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
		adapter, err := harnessAdapterFor(h, envPassthrough, harnessCommand, modelCredential)
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
