package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	grant, label := agentModelGrant(cfg, forHarness)
	if grant == nil {
		return nil, "", nil
	}
	key := agentModelCredentialKey(grant)
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

// agentModelGrant selects the agent:model grant that would back forHarness,
// applying the same scoped-over-unscoped precedence buildGooberCredentialGrants
// uses at run time (#5148). It is shared by the resolver and by the readiness
// report so the two can never disagree about which grant is in play -- a report
// describing a different grant than the one that resolves would be worse than
// no report.
//
// Returns a nil grant when none is configured, with an empty label.
func agentModelGrant(cfg *instance.Config, forHarness apiv1.Harness) (*instance.CredentialGrant, string) {
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
	switch {
	case scoped != nil:
		return scoped, fmt.Sprintf("credentials[] agent:model scoped to harness %q", scoped.Harness)
	case unscoped != nil:
		return unscoped, "credentials[] agent:model (unscoped)"
	default:
		return nil, ""
	}
}

func agentModelCredentialKey(grant *instance.CredentialGrant) string {
	key := string(capability.AgentModel)
	if grant.Harness != "" {
		key = credentials.HarnessScopedCapability(key, grant.Harness)
	}
	return key
}

// agentModelCredentialRef reports the token ref backing forHarness's
// agent:model grant, for describing its SOURCE without resolving its value
// (#5261). ok is false when no grant is configured.
func agentModelCredentialRef(cfg *instance.Config, forHarness apiv1.Harness) (credentials.TokenRef, bool) {
	grant, _ := agentModelGrant(cfg, forHarness)
	if grant == nil {
		return credentials.TokenRef{}, false
	}
	return grant.Token.CredentialTokenRef(agentModelCredentialKey(grant)), true
}

// preflightHarnesses is the seam buildSchedulerSetup calls to preflight agentic
// harnesses at startup (#238). It defaults to the real preflightAgenticHarnesses;
// the cmd/goobers test suite replaces it with a no-op in TestMain, since those
// tests drive `goobers up`/`run` against configs with agentic stages but have no
// real, installed Copilot CLI (LookPath would fail in CI). The real logic is
// tested directly in preflight_test.go.
var preflightHarnesses = preflightAgenticHarnesses

type harnessPreflightInfo map[apiv1.Harness]harness.PreflightInfo

// harnessPreflightFailures is deliberately workflow-scoped. The daemon can
// turn these failures into permanent admission refusals while continuing to
// serve workflows that do not depend on the broken harness. Callers that have
// no sibling-workflow boundary (the engine worker) may still treat the error
// as fatal at their own, narrower scope.
type harnessPreflightFailures struct {
	Refusals map[localscheduler.WorkflowIdentity]string
}

func (e *harnessPreflightFailures) Error() string {
	identities := make([]localscheduler.WorkflowIdentity, 0, len(e.Refusals))
	for identity := range e.Refusals {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Gaggle != identities[j].Gaggle {
			return identities[i].Gaggle < identities[j].Gaggle
		}
		return identities[i].Workflow < identities[j].Workflow
	})
	parts := make([]string, 0, len(identities))
	for _, identity := range identities {
		parts = append(parts, fmt.Sprintf("workflow %q (gaggle %q): %s", identity.Workflow, identity.Gaggle, e.Refusals[identity]))
	}
	return strings.Join(parts, "; ")
}

// preflightAgenticHarnesses preflights every distinct harness an agentic task
// or reviewer gate references. An unusable harness (missing binary,
// non-responsive, signed out, or missing a version) fails closed for every
// workflow that references it, while other harnesses are still checked and
// deterministic-only workflows remain unaffected.
//
// Wired into daemon startup (buildSchedulerSetup, shared by `goobers up` and
// `goobers run`) so a missing/broken harness becomes a visible workflow
// admission refusal before any worktree, claim, or run side effect — not a
// burned attempt with the cause buried in a harness transcript (#238, #5163).
// The adapter (via adapterFor) carries the auth probe and the instance's
// configured environment passthrough, so startup checks the same ambient auth
// environment as a dispatched run and the operator's harnessCommand override
// (#2483); each preflight is bounded by harnessPreflightTimeout so a hung CLI
// or network can't hang startup.
// modelCredentialFor resolves the agent:model credential for ONE harness. The
// preflight takes this instead of a single pre-resolved credential because a
// harness-scoped grant (#5148) is only findable when the harness is known:
// resolving once with an empty harness silently returns the unscoped grant, or
// none, for every harness alike (#5163).
type modelCredentialFor func(apiv1.Harness) (func(ctx context.Context) (string, error), error)

// harnessModelCredentialResolver adapts agentModelCredentialResolver — which
// already understands harness-scoped grants — into the per-harness form the
// preflight needs. Both the daemon and the worker wire it the same way, so
// neither can drift back to resolving once with an empty harness (#5163).
func harnessModelCredentialResolver(cfg *instance.Config, stores credentials.StoreResolver) modelCredentialFor {
	return func(h apiv1.Harness) (func(ctx context.Context) (string, error), error) {
		resolve, _, err := agentModelCredentialResolver(cfg, stores, h)
		return resolve, err
	}
}

func preflightAgenticHarnesses(goobers map[string]apiv1.GooberSpec, workflows []apiv1.Workflow, environment harness.EnvironmentConfig, harnessCommand map[string][]string, credentialFor modelCredentialFor) (harnessPreflightInfo, error) {
	type harnessUse struct {
		identity localscheduler.WorkflowIdentity
		stage    string
		spec     apiv1.GooberSpec
	}
	uses := make(map[apiv1.Harness][]harnessUse)
	order := make([]apiv1.Harness, 0)
	addUse := func(wf apiv1.Workflow, stageName, gooberName string) {
		spec, ok := goobers[gooberName]
		if !ok {
			return
		}
		h := spec.Harness
		if h == "" {
			h = apiv1.HarnessCopilot
		}
		if _, seen := uses[h]; !seen {
			order = append(order, h)
		}
		uses[h] = append(uses[h], harnessUse{
			identity: localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name},
			stage:    stageName,
			spec:     spec,
		})
	}
	for _, wf := range workflows {
		for _, task := range wf.Spec.Tasks {
			if task.Type == apiv1.TaskAgentic {
				addUse(wf, task.Name, task.Goober)
			}
		}
		for _, gate := range wf.Spec.Gates {
			if gate.Evaluator == apiv1.EvaluatorAgentic && gate.Agentic != nil {
				addUse(wf, gate.Name, gate.Agentic.Goober)
			}
		}
	}

	info := make(harnessPreflightInfo)
	failures := &harnessPreflightFailures{Refusals: make(map[localscheduler.WorkflowIdentity]string)}
	for _, h := range order {
		// Different Codex auth modes require distinct checks. In particular, an
		// ambient goober must never resolve the API-key grant merely because a
		// sibling Codex goober uses one.
		groups := map[string][]harnessUse{}
		var groupOrder []string
		for _, use := range uses[h] {
			encoded, _ := json.Marshal(use.spec.HarnessOptions)
			key := string(encoded)
			if _, exists := groups[key]; !exists {
				groupOrder = append(groupOrder, key)
			}
			groups[key] = append(groups[key], use)
		}
		for _, key := range groupOrder {
			group := groups[key]
			spec := group[0].spec
		// Resolve the credential FOR THIS HARNESS. Startup previously resolved
		// once with an empty harness and reused it, so an instance whose
		// agent:model grant was scoped to claude-code preflighted
		// `claude auth status` with no token and reported loggedIn:false —
		// then took the whole daemon down with it (#5163).
		var modelCredential func(ctx context.Context) (string, error)
		if credentialFor != nil && !(h == apiv1.HarnessCodex && harness.CodexUsesAmbientChatGPT(spec.HarnessOptions)) {
			resolved, err := credentialFor(h)
			if err != nil {
				for _, use := range group {
					failures.Refusals[use.identity] = appendHarnessRefusal(failures.Refusals[use.identity], fmt.Sprintf("stage %q requires harness %q, but its agent:model credential could not be resolved: %v", use.stage, h, err))
				}
				continue
			}
			modelCredential = resolved
		}
		adapter, err := harnessAdapterFor(h, environment, harnessCommand, modelCredential)
		if err != nil {
			for _, use := range group {
				failures.Refusals[use.identity] = appendHarnessRefusal(failures.Refusals[use.identity], fmt.Sprintf("stage %q requires harness %q, but its adapter could not be configured: %v", use.stage, h, err))
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), harnessPreflightTimeout)
		var result harness.PreflightInfo
		if configPreflighter, ok := adapter.(interface {
			PreflightConfig(context.Context, string, map[string]apiextensionsv1.JSON) (harness.PreflightInfo, error)
		}); ok {
			result, err = configPreflighter.PreflightConfig(ctx, spec.Model, spec.HarnessOptions)
		} else {
			result, err = adapter.Preflight(ctx)
		}
		cancel()
		if err != nil {
			for _, use := range group {
				failures.Refusals[use.identity] = appendHarnessRefusal(failures.Refusals[use.identity], fmt.Sprintf("stage %q requires harness %q, whose startup preflight failed: %v", use.stage, h, err))
			}
			continue
		}
		if result.Version == "" {
			for _, use := range group {
				failures.Refusals[use.identity] = appendHarnessRefusal(failures.Refusals[use.identity], fmt.Sprintf("stage %q requires harness %q, whose startup preflight returned no version", use.stage, h))
			}
			continue
		}
		info[h] = result
		}
	}
	if len(failures.Refusals) > 0 {
		return info, failures
	}
	return info, nil
}

func appendHarnessRefusal(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}

// preflightDaemonHarnesses converts the shared preflight's structured error
// into daemon admission state. Keeping that policy at this boundary lets the
// worker continue treating the same error as fatal at its own narrower scope.
func preflightDaemonHarnesses(
	goobers map[string]apiv1.GooberSpec,
	workflows []apiv1.Workflow,
	environment harness.EnvironmentConfig,
	harnessCommand map[string][]string,
	credentialFor modelCredentialFor,
	warnings io.Writer,
) (harnessPreflightInfo, map[localscheduler.WorkflowIdentity]string, error) {
	info, err := preflightHarnesses(goobers, workflows, environment, harnessCommand, credentialFor)
	refusals := make(map[localscheduler.WorkflowIdentity]string)
	if err == nil {
		return info, refusals, nil
	}
	var failures *harnessPreflightFailures
	if !errors.As(err, &failures) {
		return nil, nil, err
	}
	for identity, reason := range failures.Refusals {
		refusals[identity] = reason
	}
	for _, identity := range sortedWorkflowIdentities(refusals) {
		_, _ = fmt.Fprintf(warnings, "warning: workflow %q (gaggle %q) is refused because a required harness is unavailable: %s\n",
			identity.Workflow, identity.Gaggle, refusals[identity])
	}
	return info, refusals, nil
}

func preflightSchedulerHarnesses(cfg *instance.Config, set *instance.ConfigSet, goobers map[string]apiv1.GooberSpec, stores credentials.StoreResolver) (harnessPreflightInfo, map[localscheduler.WorkflowIdentity]string, error) {
	return preflightDaemonHarnesses(
		goobers, set.Workflows, harnessEnvironmentPolicy(cfg.Runner), cfg.Runner.HarnessCommand,
		harnessModelCredentialResolver(cfg, stores), os.Stderr,
	)
}
