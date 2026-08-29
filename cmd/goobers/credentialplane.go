package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

// credentialplane.go implements the daemon side of the write API's credential
// plane (distributed-state-and-coordination.md §11, DS9/DS10; #2931 honored
// as decided — decision record Goobers-Review/Goobernetes-v1/decisions/0002):
// a stage pod, authenticated as its run, resolves short-lived credentials
// scoped to exactly its stage's declared credential capabilities.
//
// The resolution machinery is deliberately the SAME capability-gated path the
// local runner's buildCredentialEnv resolves through: buildCredentials for
// the gaggle's grants, then a credentials.Injector scoped exactly as the
// stage's executor would be (runner-owned for deterministic stages,
// goober-scoped for agentic stages and reviewer gates), materialized fail
// closed. Nothing materializes for an undeclared capability, and the deny is
// a typed 403 naming the capability.
//
// NO VALUES AT REST: the resolver, injector, and materialized Set are built
// per request and dropped when it returns — the plane keeps no cache of
// resolved values beyond the request lifetime. (A GitHub App capability
// therefore mints per resolve; the 45s route budget contains the 30s mint
// ceiling.) Every resolved value is registered with the daemon's shared
// exact-value scrubber registry BEFORE the response is written — the same
// registry the instance log's scrubber and the #2931 dispatch canary read —
// so a value that later leaks into any journal/log line is redacted, and one
// that leaks into a dispatch envelope refuses the stage.
//
// AUDIT: every resolution appends a runner.annotation instance event naming
// which capabilities were resolved for which stage of which run — capability
// names only, never values — before the response is written.

// credentialResolutionMarker identifies the credential plane's audit record
// under journal.EventRunnerAnnotation (the runner.* namespace is the
// sanctioned non-normative home for mode-3 lifecycle bookkeeping).
const credentialResolutionMarker = "credentials.resolved"

// credentialGaggleScope is what the plane needs to rebuild one gaggle's
// credential grants: the same inputs buildRunnerConfig hands buildCredentials.
type credentialGaggleScope struct {
	Project         apiv1.RepoRef
	AdditionalRepos []apiv1.RepoRef
}

// credentialPlaneDefinitions is the config-derived snapshot the plane
// resolves against. Replaced wholesale on config reload (the same
// swap-don't-mutate discipline as interventionDefinitionRegistry).
type credentialPlaneDefinitions struct {
	// Scopes maps gaggle name to its credential scope.
	Scopes map[string]credentialGaggleScope
	// Goobers maps goober name to its resolved spec, for goober-scoped
	// injector construction and BYO MCP credential-key lookup. NOT for
	// reviewer-gate capabilities: those resolve from the run's pinned
	// gate-goober state (PinnedGateGooberCapabilities), never this live
	// snapshot — see stageCredentialProfile.
	Goobers map[string]apiv1.GooberSpec
}

// credentialPlaneDefinitionsFromSet derives the plane's snapshot from a
// loaded config set.
func credentialPlaneDefinitionsFromSet(set *instance.ConfigSet) credentialPlaneDefinitions {
	defs := credentialPlaneDefinitions{
		Scopes:  make(map[string]credentialGaggleScope, len(set.Gaggles)),
		Goobers: goobersByName(set),
	}
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		defs.Scopes[g.Name] = credentialGaggleScope{
			Project:         g.Spec.Project,
			AdditionalRepos: g.Spec.AdditionalRepos,
		}
	}
	return defs
}

// daemonCredentialService is the credential plane over the daemon's own
// credential wiring. It implements httpapi.CredentialService.
type daemonCredentialService struct {
	layout instance.Layout
	config *instance.Config
	stores credentials.StoreResolver
	// shared is the instance-global exact-value scrubber registry. Every
	// value the plane materializes is registered here (the Injector registers
	// each value before returning it), which is what makes later journal/log
	// leaks redactable and feeds the #2931 dispatch canary.
	shared *journal.RegistryScrubber
	log    *journal.InstanceLog
	defs   atomic.Pointer[credentialPlaneDefinitions]

	// buildSources overrides gaggle credential-source construction in tests;
	// nil uses buildCredentials — the same composition the runner wiring uses.
	buildSources func(scope credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error)
	// pinnedMachine overrides pinned-definition reconstruction in tests; nil
	// uses runner.PinnedWorkflowMachine (full WF-016 digest verification).
	pinnedMachine func(reader *journal.Reader, identity journal.RunIdentity) (*workflow.Machine, error)
	// pinnedGateCapabilities overrides pinned gate-goober capability loading
	// in tests; nil uses runner.PinnedGateGooberCapabilities.
	pinnedGateCapabilities func(reader *journal.Reader, identity journal.RunIdentity) (map[string][]string, bool, error)
}

func newDaemonCredentialService(
	layout instance.Layout,
	config *instance.Config,
	stores credentials.StoreResolver,
	shared *journal.RegistryScrubber,
	log *journal.InstanceLog,
) *daemonCredentialService {
	return &daemonCredentialService{
		layout: layout,
		config: config,
		stores: stores,
		shared: shared,
		log:    log,
	}
}

// Replace swaps the config-derived snapshot (initial wiring and config
// reload).
func (s *daemonCredentialService) Replace(defs credentialPlaneDefinitions) {
	s.defs.Store(&defs)
}

func credentialPlaneError(status int, code, message string) error {
	return httpapi.NewInterventionError(status, code, message, nil)
}

// Resolve implements httpapi.CredentialService. See the file comment for the
// contract; the flow is: locate the run, verify the stage against the run's
// PINNED workflow definition (never the currently-served one), gate the
// requested capabilities against the stage's declared set, materialize
// through the gaggle's capability-gated injector, register every value with
// the shared scrubber registry (inside Materialize, before anything is
// returned), journal the audit event, and only then answer.
func (s *daemonCredentialService) Resolve(ctx context.Context, request httpapi.CredentialResolveRequest) (httpapi.CredentialResolveResponse, error) {
	defs := s.defs.Load()
	if defs == nil || s.shared == nil || s.log == nil {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(
			http.StatusServiceUnavailable, "credentials_unavailable", "the credential plane is not configured")
	}
	if !apiv1.ValidRunID(request.RunID) {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(
			http.StatusBadRequest, "invalid_run_id", "run ID is invalid")
	}

	runDir, err := s.locateRun(*defs, request.RunID)
	if err != nil {
		return httpapi.CredentialResolveResponse{}, err
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(
			http.StatusInternalServerError, "run_read_failed", "run journal could not be read")
	}
	identity, err := reader.Identity()
	if err != nil {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(
			http.StatusInternalServerError, "run_read_failed", "run identity could not be read")
	}

	// The stage identity is verified against the run's PINNED definition
	// (WF-016): the journaled, content-addressed snapshot, digest-checked
	// twice by PinnedWorkflowMachine. A run whose pin cannot be reconstructed
	// resolves nothing — falling back to the currently-served definition
	// would let a config edit widen a live run's grants.
	pinned := s.pinnedMachine
	if pinned == nil {
		pinned = runner.PinnedWorkflowMachine
	}
	machine, err := pinned(reader, identity)
	if err != nil {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(
			http.StatusConflict, "run_pin_unverifiable",
			fmt.Sprintf("the run's pinned workflow definition could not be verified: %v", err))
	}

	// The pinned gate-goober state is loaded lazily — only an agentic
	// reviewer gate needs it — from the same journal the workflow pin came
	// from, mirroring how the task path reaches the pinned workflow.
	loadGateCapabilities := s.pinnedGateCapabilities
	if loadGateCapabilities == nil {
		loadGateCapabilities = runner.PinnedGateGooberCapabilities
	}
	profile, err := stageCredentialProfile(machine, *defs, request.Stage, func() (map[string][]string, bool, error) {
		return loadGateCapabilities(reader, identity)
	})
	if err != nil {
		return httpapi.CredentialResolveResponse{}, err
	}

	// Capability gate: a requested capability outside the stage's declared
	// set is refused with a typed 403 NAMING the capability — the runtime
	// counterpart of SEC-042's admission check, and §13 item 7's "a stage
	// whose declared capabilities are empty can resolve nothing".
	allowed := make(map[string]bool, len(profile.capabilities)+len(profile.implicitKeys))
	for _, capabilityName := range profile.capabilities {
		allowed[capabilityName] = true
	}
	for _, key := range profile.implicitKeys {
		allowed[key] = true
	}
	requested := request.Capabilities
	if len(requested) == 0 {
		requested = append([]string(nil), profile.capabilities...)
	}
	for _, capabilityName := range requested {
		if !allowed[capabilityName] {
			return httpapi.CredentialResolveResponse{}, credentialPlaneError(
				http.StatusForbidden, "capability_undeclared",
				fmt.Sprintf("capability %q is not declared by stage %q; nothing materializes for an undeclared capability", capabilityName, request.Stage))
		}
	}

	scope, ok := defs.Scopes[identity.Gaggle]
	if !ok {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(
			http.StatusConflict, "gaggle_unavailable",
			fmt.Sprintf("gaggle %q for run %q is no longer configured", identity.Gaggle, request.RunID))
	}
	injector, err := s.stageInjector(scope, profile)
	if err != nil {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(
			http.StatusInternalServerError, "credential_wiring_failed", "stage credential sources could not be constructed")
	}

	// Materialize fail closed: a granted capability whose token cannot be
	// resolved fails the whole call — a stage never starts half-credentialed
	// (the Injector's own contract). Every resolved value is registered with
	// the shared scrubber registry inside Materialize, BEFORE this returns.
	set, err := injector.Materialize(ctx, requested)
	if err != nil {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(
			http.StatusBadGateway, "credential_resolution_failed",
			"a granted credential for the stage could not be resolved")
	}

	// Collect response entries: the requested capabilities plus the goober's
	// invocation-internal credential keys Materialize always includes. A
	// declared capability with no configured grant is simply absent (not
	// every capability is credentialed) — buildCredentialEnv's own skip.
	keys := make([]string, 0, len(requested)+len(profile.implicitKeys))
	seen := make(map[string]bool, cap(keys))
	for _, key := range append(append([]string(nil), requested...), profile.implicitKeys...) {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	minted := make([]httpapi.MintedCredential, 0, len(keys))
	materialized := make([]string, 0, len(keys))
	for _, key := range keys {
		token, tokenErr := set.Token(ctx, key)
		if tokenErr != nil {
			continue // declared but not credentialed — nothing to hand out
		}
		entry := httpapi.MintedCredential{Capability: key, Value: token}
		if expiresAt, hasExpiry := set.Expiry(key); hasExpiry {
			expiry := expiresAt
			entry.ExpiresAt = &expiry
		}
		minted = append(minted, entry)
		materialized = append(materialized, key)
	}

	// Audit trail (§11): WHICH capabilities were resolved for WHICH stage —
	// names only, never values — journaled before the response is written.
	// Fail closed: when the resolution cannot be journaled, nothing is handed
	// out (the values were already minted, but they are registered with the
	// scrubbers and die with this request).
	if err := s.log.Append(journal.Event{
		Type:     journal.EventRunnerAnnotation,
		Gaggle:   identity.Gaggle,
		Workflow: identity.Workflow,
		RunID:    request.RunID,
		Stage:    request.Stage,
		Runner: map[string]any{
			"kind":         credentialResolutionMarker,
			"goober":       profile.goober,
			"requested":    requested,
			"materialized": materialized,
		},
	}); err != nil {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(
			http.StatusInternalServerError, "audit_failed", "credential resolution could not be journaled")
	}

	return httpapi.CredentialResolveResponse{
		RunID:       request.RunID,
		Stage:       request.Stage,
		Credentials: minted,
	}, nil
}

// locateRun finds the run directory across the configured gaggles (plus the
// legacy ungaggled runs dir), the same candidate walk the intervention
// service uses. Exactly one match is required.
func (s *daemonCredentialService) locateRun(defs credentialPlaneDefinitions, runID string) (string, error) {
	gaggles := make([]string, 0, len(defs.Scopes))
	for gaggle := range defs.Scopes {
		gaggles = append(gaggles, gaggle)
	}
	sort.Strings(gaggles)
	candidates := make([]string, 0, len(gaggles)+1)
	for _, gaggle := range gaggles {
		candidates = append(candidates, filepath.Join(s.layout.ForGaggle(gaggle).RunsDir(), runID))
	}
	candidates = append(candidates, filepath.Join(s.layout.RunsDir(), runID))

	found := ""
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "run.yaml")); err == nil {
			if found != "" && found != dir {
				return "", credentialPlaneError(http.StatusConflict, "ambiguous_run_id", "run ID exists in more than one gaggle")
			}
			found = dir
		} else if !os.IsNotExist(err) {
			return "", credentialPlaneError(http.StatusInternalServerError, "run_lookup_failed", "run could not be inspected")
		}
	}
	if found == "" {
		return "", credentialPlaneError(http.StatusNotFound, "run_not_found", "run was not found")
	}
	return found, nil
}

// stageProfile is the credential identity of one stage of a pinned
// definition: which goober (if any) it executes as, its declared credential
// capabilities, and the goober's invocation-internal credential keys.
type stageProfile struct {
	goober       string
	capabilities []string
	implicitKeys []string
}

// stageCredentialProfile derives the profile from the run's PINNED state:
//   - a task's declared stage-level capabilities, from the pinned workflow
//     definition (agentic tasks additionally scope to their goober;
//     deterministic tasks are runner-owned);
//   - an agentic reviewer gate's capabilities, from the run's pinned
//     gate-goober state (#294) — the same GateGooberCapabilities the engine
//     pinned into the run input at start, loaded via loadGateCapabilities.
//     Reviewer capabilities are NOT part of the pinned workflow spec, so
//     reading them from the live config snapshot would let a config edit
//     after run start change a live run's reviewer grants (PR #3528). A run
//     carrying no such pin fails closed rather than falling back to live
//     defs. The live snapshot is consulted only for the reviewer goober's
//     invocation-internal BYO MCP credential keys (the task path's own
//     behavior for its goober);
//   - an automated or human gate declares nothing and can resolve nothing.
//
// taskWorkspaceIsRepoBacked reports whether a task's declared workspace needs a
// repository checked out. Run.Workspace takes precedence over the task-level
// declaration (apiv1.Task.EffectiveWorkspace, the engine's own resolution) —
// an agentic task has no DeterministicRun and can only express a workspace on
// the task.
func taskWorkspaceIsRepoBacked(task apiv1.Task) bool {
	workspace := task.EffectiveWorkspace()
	// An UNSPECIFIED workspace does not qualify, even though the engine
	// defaults a deterministic task to repo. The implicit grant follows an
	// explicit declaration and nothing else, because §13 item 7 holds that a
	// stage declaring no capabilities resolves nothing — and a stage that
	// declares neither capabilities nor a workspace has said nothing at all to
	// hang a credential on. Requiring the declaration also matches DSL 3.0's
	// explicit-complete direction: a stage that needs a working tree says so.
	if workspace == "" {
		return false
	}
	return workspace.IsRepoBacked()
}

// An unknown stage is a typed 404: a pod cannot probe another workflow's
// stage names into grants.
func stageCredentialProfile(machine *workflow.Machine, defs credentialPlaneDefinitions, stage string, loadGateCapabilities func() (map[string][]string, bool, error)) (stageProfile, error) {
	if task, ok := machine.Task(stage); ok {
		profile := stageProfile{
			goober:       task.Goober,
			capabilities: append([]string(nil), task.Capabilities...),
		}
		if task.Goober != "" {
			spec, ok := defs.Goobers[task.Goober]
			if !ok {
				return stageProfile{}, credentialPlaneError(http.StatusConflict, "goober_unavailable",
					fmt.Sprintf("goober %q for stage %q is no longer configured", task.Goober, stage))
			}
			profile.implicitKeys = mcpconfig.BYOCredentialKeys(spec.MCPServers)
		}
		// A repo-backed workspace has to be CLONED, and the dispatcher names a
		// capability for exactly that (#3770/#3773). It is IMPLICIT here rather
		// than declared by the stage, because requiring the declaration is the
		// bug that was fixed: open-pr declares provider:pr:write and no repo
		// capability — correctly, it opens a PR and does not push — and could
		// not be provisioned at all.
		//
		// MEASURED: the dispatcher stamped the capability, the pod requested
		// it, and this gate refused with "capability "repo:push" is not
		// declared by stage "open-pr-on-pod"" — so the fix stamped a
		// credential nothing would materialize.
		//
		// This does NOT widen what the stage can do. The pod consumes this
		// credential inside the checkout and never exports it to the stage's
		// environment (dispatchexec builds credEnv from the stage's own
		// credentials only), so the grant ends where the working tree begins.
		if taskWorkspaceIsRepoBacked(task) {
			profile.implicitKeys = append(profile.implicitKeys, string(capability.RepoPush))
		}
		return profile, nil
	}
	if gate, ok := machine.Gate(stage); ok {
		if gate.Evaluator != apiv1.EvaluatorAgentic || gate.Agentic == nil {
			return stageProfile{goober: "", capabilities: nil}, nil
		}
		reviewer := gate.Agentic.Goober
		pinnedCapabilities, pinned, err := loadGateCapabilities()
		if err != nil {
			return stageProfile{}, credentialPlaneError(http.StatusConflict, "run_pin_unverifiable",
				fmt.Sprintf("the run's pinned gate-goober capabilities could not be verified: %v", err))
		}
		if !pinned {
			return stageProfile{}, credentialPlaneError(http.StatusConflict, "gate_pin_missing",
				fmt.Sprintf("the run carries no pinned gate-goober capabilities; refusing to resolve gate %q from the currently-served configuration", stage))
		}
		spec, ok := defs.Goobers[reviewer]
		if !ok {
			return stageProfile{}, credentialPlaneError(http.StatusConflict, "goober_unavailable",
				fmt.Sprintf("reviewer goober %q for gate %q is no longer configured", reviewer, stage))
		}
		// A reviewer absent from the pinned map declared no capabilities at
		// run start and resolves nothing — the same fail-closed stance the
		// runner's gate envelope takes on an unmapped goober (#294).
		return stageProfile{
			goober:       reviewer,
			capabilities: append([]string(nil), pinnedCapabilities[reviewer]...),
			implicitKeys: mcpconfig.BYOCredentialKeys(spec.MCPServers),
		}, nil
	}
	return stageProfile{}, credentialPlaneError(http.StatusNotFound, "stage_unknown",
		fmt.Sprintf("stage %q is not part of the run's pinned workflow definition", stage))
}

// stageInjector builds the Injector for the stage, scoped exactly as the
// stage's executor would be: runner-owned grants for deterministic work
// (BYO MCP sources excluded), goober-scoped grants for agentic work — a
// capability granted only to another goober stays unreachable even if this
// stage declares the same capability name.
func (s *daemonCredentialService) stageInjector(scope credentialGaggleScope, profile stageProfile) (*credentials.Injector, error) {
	build := s.buildSources
	if build == nil {
		build = func(scope credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
			owner := scope.Project.Owner
			if scope.Project.Provider == apiv1.ProviderADO && scope.Project.Project != "" {
				owner += "/" + scope.Project.Project
			}
			return buildCredentials(s.config, s.stores, owner, scope.Project.Name, scope.AdditionalRepos, s.shared)
		}
	}
	resolver, grants, err := build(scope)
	if err != nil {
		return nil, err
	}
	if profile.goober == "" {
		return credentials.NewInjector(resolver, deterministicCredentialGrants(grants), s.shared)
	}
	credentialKeys := append([]string(nil), profile.capabilities...)
	credentialKeys = append(credentialKeys, profile.implicitKeys...)
	gooberGrants := buildGooberCredentialGrants(profile.goober, credentialKeys, grants)
	return credentials.NewGooberInjectorWithCredentialKeys(resolver, profile.goober, gooberGrants, profile.implicitKeys, s.shared)
}
