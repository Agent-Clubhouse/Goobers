package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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
	expiring, label, err := agentModelCredentialExpiringResolver(cfg, stores, forHarness)
	if err != nil || expiring == nil {
		return nil, label, err
	}
	return expiring.DropExpiry(), label, nil
}

func agentModelCredentialExpiringResolver(cfg *instance.Config, stores credentials.StoreResolver, forHarness apiv1.Harness) (credentials.ExpiringResolveFunc, string, error) {
	grant, label := agentModelGrant(cfg, forHarness)
	if grant == nil {
		return nil, "", nil
	}
	key := agentModelCredentialKey(grant)
	if grant.GitHubApp != nil {
		resolve, err := newAgentModelGitHubAppTokenSource(grant.GitHubApp, nil, stores)
		return resolve, label, err
	}
	resolver, err := credentials.NewResolverWithStores(
		[]credentials.TokenRef{grant.Token.CredentialTokenRef(key)},
		stores,
	)
	if err != nil {
		return nil, "", err
	}
	return func(ctx context.Context) (string, time.Time, error) {
		value, err := resolver.Resolve(ctx, key)
		return value, time.Time{}, err
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

// preflightHarnesses is the seam buildSchedulerSetup calls to preflight agentic
// harnesses at startup (#238). It defaults to the real preflightAgenticHarnesses;
// the cmd/goobers test suite replaces it with a no-op in TestMain, since those
// tests drive `goobers up`/`run` against configs with agentic stages but have no
// real, installed Copilot CLI (LookPath would fail in CI). The real logic is
// tested directly in preflight_test.go.
var preflightHarnesses = preflightAgenticHarnesses

type harnessPreflightInfo map[apiv1.Harness]harness.PreflightInfo

// harnessPreflightFailures is deliberately workflow-scoped. The daemon can
// turn these failures into admission refusals while continuing to serve
// workflows that do not depend on the broken harness; a refusal is permanent
// for the configuration unless every failure behind it was a transient
// provider failure, which is re-probed with backoff (#6017). Callers that have
// no sibling-workflow boundary (the engine worker) may still treat the error
// as fatal at their own, narrower scope.
type harnessPreflightFailures struct {
	Refusals map[localscheduler.WorkflowIdentity]string
	// transient holds, per refused workflow, the transiently failed harness
	// groups behind its refusal (#6017). A workflow with any permanent
	// harness refusal has no entry: its refusal is permanent as a whole.
	transient map[localscheduler.WorkflowIdentity]*transientHarnessRefusal
	permanent map[localscheduler.WorkflowIdentity]bool
}

// transientHarnessRefusal is the transient part of one workflow's refusal.
type transientHarnessRefusal struct {
	retryAt time.Time
	recheck harnessWorkflowRecheck
}

// addRefusal records group's failure against every workflow it refuses.
func (e *harnessPreflightFailures) addRefusal(group []harnessUse, outcome harnessGroupOutcome, recheck *harnessGroupRecheck) {
	if e.transient == nil {
		e.transient = make(map[localscheduler.WorkflowIdentity]*transientHarnessRefusal)
	}
	if e.permanent == nil {
		e.permanent = make(map[localscheduler.WorkflowIdentity]bool)
	}
	for _, use := range group {
		e.Refusals[use.identity] = appendHarnessRefusal(e.Refusals[use.identity], fmt.Sprintf("stage %q %s", use.stage, outcome.reason))
		if outcome.retryAt.IsZero() {
			e.permanent[use.identity] = true
			continue
		}
		refusal := e.transient[use.identity]
		if refusal == nil {
			refusal = &transientHarnessRefusal{}
			e.transient[use.identity] = refusal
		}
		refusal.retryAt = latestTime(refusal.retryAt, outcome.retryAt)
		refusal.recheck.uses = append(refusal.recheck.uses, harnessRecheckUse{stage: use.stage, group: recheck})
	}
}

// transientRefusal returns identity's refusal when every harness failure
// behind it was transient, else nil.
func (e *harnessPreflightFailures) transientRefusal(identity localscheduler.WorkflowIdentity) *transientHarnessRefusal {
	if e == nil || e.permanent[identity] {
		return nil
	}
	return e.transient[identity]
}

func latestTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

type harnessUse struct {
	identity localscheduler.WorkflowIdentity
	stage    string
	spec     apiv1.GooberSpec
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
// modelCredentialSource resolves the agent:model credential for ONE harness.
// The preflight takes this instead of a single pre-resolved credential because
// a harness-scoped grant (#5148) is only findable when the harness is known:
// resolving once with an empty harness silently returns the unscoped grant, or
// none, for every harness alike (#5163).
type modelCredentialSource struct {
	// resolve returns the harness's credential resolver.
	resolve func(apiv1.Harness) (func(ctx context.Context) (string, error), error)
	// identity names the grant that would back the harness, from
	// configuration alone: it never resolves or mints a secret, so the probe
	// backoff can be consulted before any credential work (#6017).
	identity func(apiv1.Harness) string
}

// modelCredentialFor is the per-harness credential source; nil means none.
type modelCredentialFor = *modelCredentialSource

// harnessModelCredentialResolver adapts agentModelCredentialResolver — which
// already understands harness-scoped grants — into the per-harness form the
// preflight needs. Both the daemon and the worker wire it the same way, so
// neither can drift back to resolving once with an empty harness (#5163).
func harnessModelCredentialResolver(cfg *instance.Config, stores credentials.StoreResolver) modelCredentialFor {
	return &modelCredentialSource{
		resolve: func(h apiv1.Harness) (func(ctx context.Context) (string, error), error) {
			resolve, _, err := agentModelCredentialResolver(cfg, stores, h)
			return resolve, err
		},
		identity: func(h apiv1.Harness) string { return agentModelGrantIdentity(cfg, h) },
	}
}

// agentModelGrantIdentity fingerprints the configured grant that backs h: its
// token reference or GitHub App coordinates, never a resolved value. A
// GitHub App grant mints a fresh installation token per resolution, so the
// token value cannot identify it.
func agentModelGrantIdentity(cfg *instance.Config, h apiv1.Harness) string {
	grant, _ := agentModelGrant(cfg, h)
	if grant == nil {
		return "none"
	}
	encoded, _ := json.Marshal(grant)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8])
}

func preflightAgenticHarnesses(goobers map[string]apiv1.GooberSpec, workflows []apiv1.Workflow, environment harness.EnvironmentConfig, harnessCommand map[string][]string, credentialFor modelCredentialFor) (harnessPreflightInfo, error) {
	uses, order := collectHarnessUses(goobers, workflows)
	info := make(harnessPreflightInfo)
	failures := &harnessPreflightFailures{Refusals: make(map[localscheduler.WorkflowIdentity]string)}
	for _, h := range order {
		for _, group := range groupHarnessUses(uses[h]) {
			preflight := harnessGroupPreflight{
				harness: h, spec: group[0].spec, environment: environment, harnessCommand: harnessCommand,
				credentialFor: credentialFor, throttle: harnessProbeThrottle,
			}
			outcome := preflight.run(context.Background())
			if outcome.reason == "" {
				info[h] = outcome.info
				continue
			}
			var recheck *harnessGroupRecheck
			if !outcome.retryAt.IsZero() {
				recheck = newHarnessGroupRecheck(preflight, outcome)
			}
			failures.addRefusal(group, outcome, recheck)
		}
	}
	if len(failures.Refusals) > 0 {
		return info, failures
	}
	return info, nil
}

func collectHarnessUses(goobers map[string]apiv1.GooberSpec, workflows []apiv1.Workflow) (map[apiv1.Harness][]harnessUse, []apiv1.Harness) {
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
	return uses, order
}

// groupHarnessUses keeps authentication options isolated: an ambient Codex
// goober must never resolve an API-key grant because a sibling Codex goober
// uses that different authentication mode.
func groupHarnessUses(uses []harnessUse) [][]harnessUse {
	groups := map[string][]harnessUse{}
	var order []string
	for _, use := range uses {
		encoded, _ := json.Marshal(use.spec.HarnessOptions)
		key := string(encoded)
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], use)
	}
	result := make([][]harnessUse, 0, len(order))
	for _, key := range order {
		result = append(result, groups[key])
	}
	return result
}

// harnessGroupPreflight is everything one harness group's preflight needs,
// kept together so a transient failure can re-run the identical check later
// (#6017).
type harnessGroupPreflight struct {
	harness        apiv1.Harness
	spec           apiv1.GooberSpec
	environment    harness.EnvironmentConfig
	harnessCommand map[string][]string
	credentialFor  modelCredentialFor
	throttle       *harness.ProbeBackoff
}

// harnessGroupOutcome is one group's preflight result: info when usable, else
// the refusal reason, with retryAt set exactly when the failure was transient.
// key is the backoff key the outcome was recorded under.
type harnessGroupOutcome struct {
	info    harness.PreflightInfo
	reason  string
	retryAt time.Time
	key     string
}

// key identifies the group to the probe backoff: the same harness, options,
// command, environment policy and agent:model grant make the same sign-in
// request of the same provider. credential is the grant's configured identity,
// so a reload that changes the grant probes at once instead of waiting out the
// old grant's backoff.
func (p harnessGroupPreflight) key(credential string) string {
	options, _ := json.Marshal(p.spec.HarnessOptions)
	parts := []string{string(p.harness), string(options), credential,
		strings.Join(p.environment.ExtraAllowlist, ","), strings.Join(p.environment.Unset, ",")}
	return strings.Join(append(parts, p.harnessCommand[string(p.harness)]...), "\x00")
}

// run preflights the group unless a transient failure is still being waited
// out, in which case it refuses with that failure without probing — and
// without resolving or minting any credential, since the backoff is keyed by
// the grant's configured identity. A transient failure
// (credentials.ErrTransientProvider: a rate-limited, failing or unreachable
// provider, including while resolving the credential) is backed off; any
// other outcome clears the backoff, and a non-transient failure stays the
// permanent refusal it always was.
func (p harnessGroupPreflight) run(ctx context.Context) harnessGroupOutcome {
	key := p.key(p.credentialIdentity())
	if wait, waiting := p.throttle.Pending(key); waiting {
		return harnessGroupOutcome{reason: transientHarnessReason(wait.Cause, wait.RetryAt), retryAt: wait.RetryAt, key: key}
	}
	modelCredential, err := p.modelCredential()
	if err != nil {
		return p.finish(key, harness.PreflightInfo{}, fmt.Errorf("requires harness %q, but its agent:model credential could not be resolved: %w", p.harness, err))
	}
	info, err := p.check(ctx, modelCredential)
	return p.finish(key, info, err)
}

// finish records a probe's result in the backoff and shapes the outcome.
func (p harnessGroupPreflight) finish(key string, info harness.PreflightInfo, err error) harnessGroupOutcome {
	if err == nil {
		p.throttle.Clear(key)
		return harnessGroupOutcome{info: info, key: key}
	}
	if errors.Is(err, credentials.ErrTransientProvider) {
		retryAt := p.throttle.Record(key, err)
		return harnessGroupOutcome{reason: transientHarnessReason(err, retryAt), retryAt: retryAt, key: key}
	}
	p.throttle.Clear(key)
	return harnessGroupOutcome{reason: err.Error(), key: key}
}

// usesModelCredential reports whether the group's preflight carries the
// configured agent:model credential: not without a source, and not for
// Codex's ambient ChatGPT session.
func (p harnessGroupPreflight) usesModelCredential() bool {
	return p.credentialFor != nil && (p.harness != apiv1.HarnessCodex || !harness.CodexUsesAmbientChatGPT(p.spec.HarnessOptions))
}

// credentialIdentity is the backoff key's credential component.
func (p harnessGroupPreflight) credentialIdentity() string {
	if !p.usesModelCredential() || p.credentialFor.identity == nil {
		return "ambient"
	}
	return p.credentialFor.identity(p.harness)
}

// modelCredential returns the group's agent:model credential resolver; the
// adapter resolves it during the probe.
func (p harnessGroupPreflight) modelCredential() (func(context.Context) (string, error), error) {
	if !p.usesModelCredential() || p.credentialFor.resolve == nil {
		return nil, nil
	}
	return p.credentialFor.resolve(p.harness)
}

func (p harnessGroupPreflight) check(ctx context.Context, modelCredential func(context.Context) (string, error)) (harness.PreflightInfo, error) {
	h := p.harness
	adapter, err := harnessAdapterFor(h, p.environment, p.harnessCommand, modelCredential)
	if err != nil {
		return harness.PreflightInfo{}, fmt.Errorf("requires harness %q, but its adapter could not be configured: %w", h, err)
	}
	ctx, cancel := context.WithTimeout(ctx, harnessPreflightTimeout)
	defer cancel()
	result, err := preflightAdapterConfig(ctx, adapter, p.spec)
	if err != nil {
		return harness.PreflightInfo{}, fmt.Errorf("requires harness %q, whose startup preflight failed: %w", h, err)
	}
	if result.Version == "" {
		return harness.PreflightInfo{}, fmt.Errorf("requires harness %q, whose startup preflight returned no version", h)
	}
	return result, nil
}

func preflightAdapterConfig(ctx context.Context, adapter harness.Adapter, spec apiv1.GooberSpec) (harness.PreflightInfo, error) {
	if configPreflighter, ok := adapter.(interface {
		PreflightConfig(context.Context, string, map[string]apiextensionsv1.JSON) (harness.PreflightInfo, error)
	}); ok {
		return configPreflighter.PreflightConfig(ctx, spec.Model, spec.HarnessOptions)
	}
	return adapter.Preflight(ctx)
}

func appendHarnessRefusal(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}
