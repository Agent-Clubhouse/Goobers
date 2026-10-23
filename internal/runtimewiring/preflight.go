package runtimewiring

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

// AgentModelCredentialResolver builds a resolver for the instance's
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
func AgentModelCredentialResolver(cfg *instance.Config, stores credentials.StoreResolver, forHarness apiv1.Harness) (resolve func(ctx context.Context) (string, error), label string, err error) {
	expiring, label, err := AgentModelCredentialExpiringResolver(cfg, stores, forHarness)
	if err != nil || expiring == nil {
		return nil, label, err
	}
	return expiring.DropExpiry(), label, nil
}

// AgentModelCredentialExpiringResolver selects a harness model credential resolver that preserves expiry.
func AgentModelCredentialExpiringResolver(cfg *instance.Config, stores credentials.StoreResolver, forHarness apiv1.Harness) (credentials.ExpiringResolveFunc, string, error) {
	grant, label := AgentModelGrant(cfg, forHarness)
	if grant == nil {
		return nil, "", nil
	}
	key := agentModelCredentialKey(grant)
	if grant.GitHubApp != nil {
		resolve, err := NewAgentModelGitHubAppTokenSource(grant.GitHubApp, nil, stores)
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

// AgentModelGrant selects the agent:model grant that would back forHarness,
// applying the same scoped-over-unscoped precedence buildGooberCredentialGrants
// uses at run time (#5148). It is shared by the resolver and by the readiness
// report so the two can never disagree about which grant is in play -- a report
// describing a different grant than the one that resolves would be worse than
// no report.
//
// Returns a nil grant when none is configured, with an empty label.
func AgentModelGrant(cfg *instance.Config, forHarness apiv1.Harness) (*instance.CredentialGrant, string) {
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

// AgentModelCredentialRef reports the token ref backing forHarness's
// agent:model grant, for describing its SOURCE without resolving its value
// (#5261). ok is false when no grant is configured.
func AgentModelCredentialRef(cfg *instance.Config, forHarness apiv1.Harness) (credentials.TokenRef, bool) {
	grant, _ := AgentModelGrant(cfg, forHarness)
	if grant == nil || grant.GitHubApp != nil {
		return credentials.TokenRef{}, false
	}
	return grant.Token.CredentialTokenRef(agentModelCredentialKey(grant)), true
}

// PreflightHarnesses is the seam buildSchedulerSetup calls to preflight agentic
// harnesses at startup (#238). It defaults to the real preflightAgenticHarnesses;
// the cmd/goobers test suite replaces it with a no-op in TestMain, since those
// tests drive `goobers up`/`run` against configs with agentic stages but have no
// real, installed Copilot CLI (LookPath would fail in CI). The real logic is
// tested directly in preflight_test.go.
var PreflightHarnesses = PreflightAgenticHarnesses

// HarnessPreflightInfo indexes successful preflight results by harness.
type HarnessPreflightInfo map[apiv1.Harness]harness.PreflightInfo

// HarnessPreflightFailures is deliberately workflow-scoped. The daemon can
// turn these failures into admission refusals while continuing to serve
// workflows that do not depend on the broken harness; a refusal is permanent
// for the configuration unless every failure behind it was a transient
// provider failure, which is re-probed with backoff (#6017). Callers that have
// no sibling-workflow boundary (the engine worker) may still treat the error
// as fatal at their own, narrower scope.
type HarnessPreflightFailures struct {
	Refusals map[localscheduler.WorkflowIdentity]string
	// transient holds, per refused workflow, the transiently failed harness
	// groups behind its refusal (#6017). A workflow with any permanent
	// harness refusal has no entry: its refusal is permanent as a whole.
	Transient map[localscheduler.WorkflowIdentity]*transientHarnessRefusal
	Permanent map[localscheduler.WorkflowIdentity]bool
}

// transientHarnessRefusal is the transient part of one workflow's refusal.
type transientHarnessRefusal struct {
	RetryAt time.Time
	Recheck HarnessWorkflowRecheck
}

// AddRefusal records group's failure against every workflow it refuses.
func (e *HarnessPreflightFailures) AddRefusal(group []harnessUse, outcome harnessGroupOutcome, recheck *harnessGroupRecheck) {
	if e.Transient == nil {
		e.Transient = make(map[localscheduler.WorkflowIdentity]*transientHarnessRefusal)
	}
	if e.Permanent == nil {
		e.Permanent = make(map[localscheduler.WorkflowIdentity]bool)
	}
	for _, use := range group {
		e.Refusals[use.Identity] = appendHarnessRefusal(e.Refusals[use.Identity], fmt.Sprintf("stage %q %s", use.Stage, outcome.Reason))
		if outcome.RetryAt.IsZero() {
			e.Permanent[use.Identity] = true
			continue
		}
		refusal := e.Transient[use.Identity]
		if refusal == nil {
			refusal = &transientHarnessRefusal{}
			e.Transient[use.Identity] = refusal
		}
		refusal.RetryAt = latestTime(refusal.RetryAt, outcome.RetryAt)
		refusal.Recheck.Uses = append(refusal.Recheck.Uses, harnessRecheckUse{Stage: use.Stage, Group: recheck})
	}
}

// TransientRefusal returns identity's refusal when every harness failure
// behind it was transient, else nil.
func (e *HarnessPreflightFailures) TransientRefusal(identity localscheduler.WorkflowIdentity) *transientHarnessRefusal {
	if e == nil || e.Permanent[identity] {
		return nil
	}
	return e.Transient[identity]
}

// Recheck is identity's localscheduler.HarnessRecheck: nil unless its
// refusal is wholly transient.
func (e *HarnessPreflightFailures) Recheck(identity localscheduler.WorkflowIdentity) localscheduler.HarnessRecheck {
	if refusal := e.TransientRefusal(identity); refusal != nil {
		return &refusal.Recheck
	}
	return nil
}

// Reason is identity's refusal text; "" when it is not refused.
func (e *HarnessPreflightFailures) Reason(identity localscheduler.WorkflowIdentity) string {
	if e == nil {
		return ""
	}
	return e.Refusals[identity]
}

func latestTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

type harnessUse struct {
	Identity localscheduler.WorkflowIdentity
	Stage    string
	Spec     apiv1.GooberSpec
}

func (e *HarnessPreflightFailures) Error() string {
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

// ModelCredentialSource resolves the agent:model credential for ONE harness.
// The preflight takes this instead of a single pre-resolved credential because
// a harness-scoped grant (#5148) is only findable when the harness is known:
// resolving once with an empty harness silently returns the unscoped grant, or
// none, for every harness alike (#5163).
type ModelCredentialSource struct {
	// resolve returns the harness's credential resolver.
	Resolve func(apiv1.Harness) (func(ctx context.Context) (string, error), error)
	// identity names the grant that would back the harness, from
	// configuration alone: it never resolves or mints a secret, so the probe
	// backoff can be consulted before any credential work (#6017).
	Identity func(apiv1.Harness) string
}

// ModelCredentialFor is the per-harness credential source; nil means none.
type ModelCredentialFor = *ModelCredentialSource

// HarnessModelCredentialResolver adapts agentModelCredentialResolver — which
// already understands harness-scoped grants — into the per-harness form the
// preflight needs. Both the daemon and the worker wire it the same way, so
// neither can drift back to resolving once with an empty harness (#5163).
func HarnessModelCredentialResolver(cfg *instance.Config, stores credentials.StoreResolver) ModelCredentialFor {
	return &ModelCredentialSource{
		Resolve: func(h apiv1.Harness) (func(ctx context.Context) (string, error), error) {
			resolve, _, err := AgentModelCredentialResolver(cfg, stores, h)
			return resolve, err
		},
		Identity: func(h apiv1.Harness) string { return agentModelGrantIdentity(cfg, h) },
	}
}

// agentModelGrantIdentity fingerprints the configured grant that backs h: its
// token reference or GitHub App coordinates, never a resolved value. A
// GitHub App grant mints a fresh installation token per resolution, so the
// token value cannot identify it.
func agentModelGrantIdentity(cfg *instance.Config, h apiv1.Harness) string {
	grant, _ := AgentModelGrant(cfg, h)
	if grant == nil {
		return "none"
	}
	encoded, _ := json.Marshal(grant)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8])
}

func PreflightAgenticHarnesses(goobers map[string]apiv1.GooberSpec, workflows []apiv1.Workflow, environment harness.EnvironmentConfig, harnessCommand map[string][]string, credentialFor ModelCredentialFor) (HarnessPreflightInfo, error) {
	uses, order := collectHarnessUses(goobers, workflows)
	info := make(HarnessPreflightInfo)
	failures := &HarnessPreflightFailures{Refusals: make(map[localscheduler.WorkflowIdentity]string)}
	for _, h := range order {
		for _, group := range groupHarnessUses(uses[h]) {
			preflight := harnessGroupPreflight{
				Harness: h, Spec: group[0].Spec, Environment: environment, HarnessCommand: harnessCommand,
				CredentialFor: credentialFor, Throttle: HarnessProbeThrottle,
			}
			outcome := preflight.Run(context.Background())
			if outcome.Reason == "" {
				info[h] = outcome.Info
				continue
			}
			// PreflightAgenticHarnesses checks every referenced harness and scopes refusals to affected workflows.
			var recheck *harnessGroupRecheck
			if !outcome.RetryAt.IsZero() {
				recheck = newHarnessGroupRecheck(preflight, outcome)
			}
			failures.AddRefusal(group, outcome, recheck)
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
			Identity: localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name},
			Stage:    stageName,
			Spec:     spec,
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
		encoded, _ := json.Marshal(use.Spec.HarnessOptions)
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
	Harness        apiv1.Harness
	Spec           apiv1.GooberSpec
	Environment    harness.EnvironmentConfig
	HarnessCommand map[string][]string
	CredentialFor  ModelCredentialFor
	Throttle       *harness.ProbeBackoff
}

// harnessGroupOutcome is one group's preflight result: info when usable, else
// the refusal reason, with retryAt set exactly when the failure was transient.
// key is the backoff key the outcome was recorded under.
type harnessGroupOutcome struct {
	Info    harness.PreflightInfo
	Reason  string
	RetryAt time.Time
	Key     string
}

// Key identifies the group to the probe backoff: the same harness, options,
// command, environment policy and agent:model grant make the same sign-in
// request of the same provider. credential is the grant's configured identity,
// so a reload that changes the grant probes at once instead of waiting out the
// old grant's backoff.
func (p harnessGroupPreflight) Key(credential string) string {
	options, _ := json.Marshal(p.Spec.HarnessOptions)
	parts := []string{string(p.Harness), string(options), credential,
		strings.Join(p.Environment.ExtraAllowlist, ","), strings.Join(p.Environment.Unset, ",")}
	return strings.Join(append(parts, p.HarnessCommand[string(p.Harness)]...), "\x00")
}

// Run preflights the group unless a transient failure is still being waited
// out, in which case it refuses with that failure without probing — and
// without resolving or minting any credential, since the backoff is keyed by
// the grant's configured identity. A transient failure
// (credentials.ErrTransientProvider: a rate-limited, failing or unreachable
// provider, including while resolving the credential) is backed off; any
// other outcome clears the backoff, and a non-transient failure stays the
// permanent refusal it always was.
func (p harnessGroupPreflight) Run(ctx context.Context) harnessGroupOutcome {
	key := p.Key(p.CredentialIdentity())
	if wait, waiting := p.Throttle.Pending(key); waiting {
		return harnessGroupOutcome{Reason: transientHarnessReason(wait.Cause, wait.RetryAt), RetryAt: wait.RetryAt, Key: key}
	}
	modelCredential, err := p.ModelCredential()
	if err != nil {
		return p.Finish(key, harness.PreflightInfo{}, fmt.Errorf("requires harness %q, but its agent:model credential could not be resolved: %w", p.Harness, err))
	}
	info, err := p.Check(ctx, modelCredential)
	return p.Finish(key, info, err)
}

// Finish records a probe's result in the backoff and shapes the outcome.
func (p harnessGroupPreflight) Finish(key string, info harness.PreflightInfo, err error) harnessGroupOutcome {
	if err == nil {
		p.Throttle.Clear(key)
		return harnessGroupOutcome{Info: info, Key: key}
	}
	if errors.Is(err, credentials.ErrTransientProvider) {
		retryAt := p.Throttle.Record(key, err)
		return harnessGroupOutcome{Reason: transientHarnessReason(err, retryAt), RetryAt: retryAt, Key: key}
	}
	p.Throttle.Clear(key)
	return harnessGroupOutcome{Reason: err.Error(), Key: key}
}

// UsesModelCredential reports whether the group's preflight carries the
// configured agent:model credential: not without a source, and not for
// Codex's ambient ChatGPT session.
func (p harnessGroupPreflight) UsesModelCredential() bool {
	return p.CredentialFor != nil && (p.Harness != apiv1.HarnessCodex || !harness.CodexUsesAmbientChatGPT(p.Spec.HarnessOptions))
}

// CredentialIdentity is the backoff key's credential component.
func (p harnessGroupPreflight) CredentialIdentity() string {
	if !p.UsesModelCredential() || p.CredentialFor.Identity == nil {
		return "ambient"
	}
	return p.CredentialFor.Identity(p.Harness)
}

// ModelCredential returns the group's agent:model credential resolver; the
// adapter resolves it during the probe.
func (p harnessGroupPreflight) ModelCredential() (func(context.Context) (string, error), error) {
	if !p.UsesModelCredential() || p.CredentialFor.Resolve == nil {
		return nil, nil
	}
	return p.CredentialFor.Resolve(p.Harness)
}

func (p harnessGroupPreflight) Check(ctx context.Context, modelCredential func(context.Context) (string, error)) (harness.PreflightInfo, error) {
	h := p.Harness
	adapter, err := HarnessAdapterFor(h, p.Environment, p.HarnessCommand, modelCredential)
	if err != nil {
		return harness.PreflightInfo{}, fmt.Errorf("requires harness %q, but its adapter could not be configured: %w", h, err)
	}
	ctx, cancel := context.WithTimeout(ctx, HarnessPreflightTimeout)
	defer cancel()
	result, err := PreflightAdapterConfig(ctx, adapter, p.Spec)
	if err != nil {
		return harness.PreflightInfo{}, fmt.Errorf("requires harness %q, whose startup preflight failed: %w", h, err)
	}
	if result.Version == "" {
		return harness.PreflightInfo{}, fmt.Errorf("requires harness %q, whose startup preflight returned no version", h)
	}
	// Check checks whether a harness version satisfies the configured policy.
	return result, nil
}

func PreflightAdapterConfig(ctx context.Context, adapter harness.Adapter, spec apiv1.GooberSpec) (harness.PreflightInfo, error) {
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

// PreflightAdapterConfig builds the harness configuration used for admission probes.
