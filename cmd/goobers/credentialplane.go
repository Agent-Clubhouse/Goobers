package main

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/externaltelemetry"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

// credentialplane.go implements the daemon side of the write API's credential
// plane (distributed-state-and-coordination.md §11, DS9/DS10; #2931 honored
// as decided — see docs/design/goobernetes-decisions.md):
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
	Project apiv1.RepoRef
	// Backlog selects the backlog-role binding when the backlog lives on
	// another provider than Project (topology (b)).
	Backlog         apiv1.BacklogRef
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
			Backlog:         g.Spec.Backlog,
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
	// grants signs and admits stage credential-refresh grants (Goobers#6120,
	// credentialrefresh.go). Nil disables mid-stage refresh: no grant is
	// minted and the refresh route answers 503.
	grants *stageGrantIssuer

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
//
// A request that asks for a stage credential-refresh grant (Goobers#6120)
// also receives one when the stage is a deterministic task: the pod hands it
// to the stage child, which re-resolves through the refresh route instead of
// holding a frozen value for the whole stage.
func (s *daemonCredentialService) Resolve(ctx context.Context, request httpapi.CredentialResolveRequest) (httpapi.CredentialResolveResponse, error) {
	resolved, err := s.resolveStage(ctx, request, stageResolveMode{marker: credentialResolutionMarker})
	if err != nil {
		return httpapi.CredentialResolveResponse{}, err
	}
	response := resolved.response(request)
	if request.Grant {
		response.Grant = s.mintPodStageGrant(request, resolved)
	}
	return response, nil
}

// stageResolveMode distinguishes a stage-start resolve from a mid-stage
// refresh: the refresh route refuses agentic stages and journals under its
// own marker with the grant's attempt.
type stageResolveMode struct {
	marker            string
	deterministicOnly bool
	attempt           int32
}

// stageResolution is one successful resolve, before it is rendered.
type stageResolution struct {
	profile      stageProfile
	scheme       string
	minted       []httpapi.MintedCredential
	materialized []string
}

func (r stageResolution) response(request httpapi.CredentialResolveRequest) httpapi.CredentialResolveResponse {
	return httpapi.CredentialResolveResponse{
		RunID:          request.RunID,
		Stage:          request.Stage,
		Credentials:    r.minted,
		RepoAuthScheme: r.scheme,
	}
}

// resolveStage is the shared resolve core of the resolve and refresh routes.
func (s *daemonCredentialService) resolveStage(ctx context.Context, request httpapi.CredentialResolveRequest, mode stageResolveMode) (stageResolution, error) {
	pinned, err := s.loadPinnedStage(ctx, request)
	if err != nil {
		return stageResolution{}, err
	}
	defer pinned.release()
	if mode.deterministicOnly && !pinned.profile.deterministic {
		return stageResolution{}, credentialPlaneError(http.StatusForbidden, "credential_refresh_agentic_stage",
			fmt.Sprintf("stage %q is not a deterministic task; mid-stage credential refresh serves deterministic stages only", request.Stage))
	}
	requested, err := gateRequestedCapabilities(pinned.profile, request)
	if err != nil {
		return stageResolution{}, err
	}
	if err := requireCurrentMergeAuthority(s.layout, apiv1.InvocationEnvelope{
		ConfigGeneration: pinned.identity.ConfigGeneration, Gaggle: pinned.identity.Gaggle,
		WorkflowID: pinned.identity.Workflow, TaskID: request.Stage, Capabilities: requested,
	}); err != nil {
		return stageResolution{}, credentialPlaneError(http.StatusForbidden, "merge_authority_revoked", err.Error())
	}
	resolved, err := s.mintStageCredentials(ctx, pinned, requested)
	if err != nil {
		return stageResolution{}, err
	}
	if err := s.journalResolution(pinned, request, mode, requested, resolved.materialized); err != nil {
		return stageResolution{}, err
	}
	return resolved, nil
}

// pinnedStage is a run's stage identity reconstructed from its pinned state.
type pinnedStage struct {
	identity journal.RunIdentity
	defs     credentialPlaneDefinitions
	profile  stageProfile
	release  func()
}

// loadPinnedStage locates the run, reconstructs its PINNED definition and
// derives the stage's credential profile from it.
func (s *daemonCredentialService) loadPinnedStage(ctx context.Context, request httpapi.CredentialResolveRequest) (pinnedStage, error) {
	defs := s.defs.Load()
	if defs == nil || s.shared == nil || s.log == nil {
		return pinnedStage{}, credentialPlaneError(
			http.StatusServiceUnavailable, "credentials_unavailable", "the credential plane is not configured")
	}
	if !apiv1.ValidRunID(request.RunID) {
		return pinnedStage{}, credentialPlaneError(
			http.StatusBadRequest, "invalid_run_id", "run ID is invalid")
	}
	runDir, err := s.locateRun(*defs, request.RunID)
	if err != nil {
		return pinnedStage{}, err
	}
	reader, identity, err := openRunIdentity(runDir)
	if err != nil {
		return pinnedStage{}, err
	}
	pinned := pinnedStage{identity: identity, defs: *defs, release: func() {}}
	if identity.ConfigGeneration != "" {
		pinnedDefinitions, release, err := pinnedCredentialDefinitions(ctx, s.layout, identity.ConfigGeneration)
		if err != nil {
			return pinnedStage{}, credentialPlaneError(http.StatusConflict, "config_generation_unverifiable", err.Error())
		}
		pinned.defs, pinned.release = pinnedDefinitions, release
	}
	profile, err := s.pinnedStageProfile(reader, pinned, request.Stage)
	if err != nil {
		pinned.release()
		return pinnedStage{}, err
	}
	pinned.profile = profile
	return pinned, nil
}

func openRunIdentity(runDir string) (*journal.Reader, journal.RunIdentity, error) {
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return nil, journal.RunIdentity{}, credentialPlaneError(
			http.StatusInternalServerError, "run_read_failed", "run journal could not be read")
	}
	identity, err := reader.Identity()
	if err != nil {
		return nil, journal.RunIdentity{}, credentialPlaneError(
			http.StatusInternalServerError, "run_read_failed", "run identity could not be read")
	}
	return reader, identity, nil
}

// pinnedStageProfile verifies the stage against the run's PINNED definition
// (WF-016): the journaled, content-addressed snapshot, digest-checked twice by
// PinnedWorkflowMachine. A run whose pin cannot be reconstructed resolves
// nothing — falling back to the currently-served definition would let a
// config edit widen a live run's grants.
func (s *daemonCredentialService) pinnedStageProfile(reader *journal.Reader, pinned pinnedStage, stage string) (stageProfile, error) {
	pinnedMachine := s.pinnedMachine
	if pinnedMachine == nil {
		pinnedMachine = runner.PinnedWorkflowMachine
	}
	machine, err := pinnedMachine(reader, pinned.identity)
	if err != nil {
		return stageProfile{}, credentialPlaneError(
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
	return stageCredentialProfile(machine, pinned.defs, stage, func() (map[string][]string, bool, error) {
		return loadGateCapabilities(reader, pinned.identity)
	})
}

// gateRequestedCapabilities is the capability gate: a requested capability
// outside the stage's declared set is refused with a typed 403 NAMING the
// capability — the runtime counterpart of SEC-042's admission check, and §13
// item 7's "a stage whose declared capabilities are empty can resolve
// nothing". An empty request resolves the full declared set.
func gateRequestedCapabilities(profile stageProfile, request httpapi.CredentialResolveRequest) ([]string, error) {
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
			return nil, credentialPlaneError(
				http.StatusForbidden, "capability_undeclared",
				fmt.Sprintf("capability %q is not declared by stage %q; nothing materializes for an undeclared capability", capabilityName, request.Stage))
		}
	}
	return requested, nil
}

// mintStageCredentials materializes the requested capabilities (plus the
// goober's invocation-internal keys) fail closed: a granted capability whose
// token cannot be resolved fails the whole call — a stage never starts
// half-credentialed (the Injector's own contract). Every resolved value is
// registered with the shared scrubber registry inside Materialize, BEFORE
// this returns.
func (s *daemonCredentialService) mintStageCredentials(ctx context.Context, pinned pinnedStage, requested []string) (stageResolution, error) {
	connectorCredential, err := s.stageConnectorCredential(ctx, pinned.profile, requested)
	if err != nil {
		return stageResolution{}, err
	}
	scope, ok := pinned.defs.Scopes[pinned.identity.Gaggle]
	if !ok {
		return stageResolution{}, credentialPlaneError(
			http.StatusConflict, "gaggle_unavailable",
			fmt.Sprintf("gaggle %q for run %q is no longer configured", pinned.identity.Gaggle, pinned.identity.RunID))
	}
	injector, err := s.stageInjector(scope, pinned.profile)
	if err != nil {
		return stageResolution{}, credentialPlaneError(
			http.StatusInternalServerError, "credential_wiring_failed", "stage credential sources could not be constructed")
	}
	set, err := injector.Materialize(ctx, requested)
	if err != nil {
		return stageResolution{}, credentialPlaneError(
			http.StatusBadGateway, "credential_resolution_failed",
			"a granted credential for the stage could not be resolved")
	}
	resolved := collectMintedCredentials(ctx, set, requested, pinned.profile.implicitKeys)
	if connectorCredential != nil {
		resolved.minted = append(resolved.minted, *connectorCredential)
		resolved.materialized = append(resolved.materialized, connectorCredential.Capability)
	}
	resolved.profile = pinned.profile
	resolved.scheme = s.repoAuthScheme(scope, len(resolved.minted))
	return resolved, nil
}

// stageConnectorCredential resolves external-telemetry's connector secret
// (#4341): the ONE connector this stage's pinned inputs.connector names,
// resolved here rather than through the ordinary grant-injector — a
// connector's auth ref lives in externalTelemetry.connectors, not in a
// credentials: grant, and which connector applies varies per stage rather
// than per gaggle.
func (s *daemonCredentialService) stageConnectorCredential(ctx context.Context, profile stageProfile, requested []string) (*httpapi.MintedCredential, error) {
	if profile.externalTelemetryConnector == "" || !containsString(requested, string(capability.TelemetryRead)) {
		return nil, nil
	}
	return s.resolveExternalTelemetryConnectorCredential(ctx, profile.externalTelemetryConnector)
}

// collectMintedCredentials renders the requested capabilities plus the
// goober's invocation-internal credential keys Materialize always includes. A
// declared capability with no configured grant is simply absent (not every
// capability is credentialed) — buildCredentialEnv's own skip.
func collectMintedCredentials(ctx context.Context, set *credentials.Set, requested, implicitKeys []string) stageResolution {
	keys := make([]string, 0, len(requested)+len(implicitKeys))
	seen := make(map[string]bool, cap(keys))
	for _, key := range append(append([]string(nil), requested...), implicitKeys...) {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	resolved := stageResolution{
		minted:       make([]httpapi.MintedCredential, 0, len(keys)),
		materialized: make([]string, 0, len(keys)),
	}
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
		resolved.minted = append(resolved.minted, entry)
		resolved.materialized = append(resolved.materialized, key)
	}
	return resolved
}

// journalResolution is the audit trail (§11): WHICH capabilities were
// resolved for WHICH stage — names only, never values — journaled before the
// response is written. Fail closed: when the resolution cannot be journaled,
// nothing is handed out (the values were already minted, but they are
// registered with the scrubbers and die with this request).
func (s *daemonCredentialService) journalResolution(pinned pinnedStage, request httpapi.CredentialResolveRequest, mode stageResolveMode, requested, materialized []string) error {
	record := map[string]any{
		"kind":         mode.marker,
		"goober":       pinned.profile.goober,
		"requested":    requested,
		"materialized": materialized,
	}
	if mode.deterministicOnly {
		record["attempt"] = mode.attempt
	}
	if err := s.log.Append(journal.Event{
		Type:     journal.EventRunnerAnnotation,
		Gaggle:   pinned.identity.Gaggle,
		Workflow: pinned.identity.Workflow,
		RunID:    request.RunID,
		Stage:    request.Stage,
		Runner:   record,
	}); err != nil {
		return credentialPlaneError(
			http.StatusInternalServerError, "audit_failed", "credential resolution could not be journaled")
	}
	return nil
}

// repoAuthScheme is the non-secret authorization scheme of the gaggle's Azure
// DevOps repository credential, stated beside the minted values so a stage pod
// builds the right header without inferring it from the token (#5656). Empty
// when nothing was minted or the gaggle's repository is not on Azure DevOps.
func (s *daemonCredentialService) repoAuthScheme(scope credentialGaggleScope, minted int) string {
	if minted == 0 {
		return ""
	}
	repo, ok := adoRepoForGaggle(s.config, scope.Project)
	if !ok {
		return ""
	}
	return adoauth.AuthScheme(repo)
}

// resolveExternalTelemetryConnectorCredential mints connectorName's auth
// secret for capability telemetry:read (#4341).
//
// This deliberately answers a DIFFERENT question than the ordinary
// grant-injector: the connector's Auth.Token ref lives in
// externalTelemetry.connectors, an operator declares it once per connector
// rather than per gaggle, and it is not a credentials.Grant at all — so it
// is resolved directly against the instance config rather than threaded
// through buildCredentials. A connector with no configured Auth.Token (the
// none/ambient auth modes) mints nothing here, which is correct: the pod's
// own connector construction (the second #4341 PR) runs with no credential
// for it, exactly as the local path does today.
func (s *daemonCredentialService) resolveExternalTelemetryConnectorCredential(ctx context.Context, connectorName string) (*httpapi.MintedCredential, error) {
	var connector *externaltelemetry.ConnectorConfig
	for i := range s.config.ExternalTelemetry.Connectors {
		if s.config.ExternalTelemetry.Connectors[i].Name == connectorName {
			connector = &s.config.ExternalTelemetry.Connectors[i]
			break
		}
	}
	if connector == nil {
		return nil, credentialPlaneError(http.StatusConflict, "connector_unavailable",
			fmt.Sprintf("external telemetry connector %q is no longer configured", connectorName))
	}
	ref := connector.Auth.Token
	if ref == nil || (ref.Env == "" && ref.File == "") {
		// No auth secret declared (auth mode none, or ambient credentials the
		// connector's own factory resolves itself) — nothing to mint.
		return nil, nil
	}
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{
		Name: connectorName, Env: ref.Env, File: ref.File,
	}})
	if err != nil {
		return nil, credentialPlaneError(http.StatusInternalServerError, "credential_wiring_failed",
			"external telemetry connector credential source could not be constructed")
	}
	value, err := resolver.Resolve(ctx, connectorName)
	if err != nil {
		return nil, credentialPlaneError(http.StatusBadGateway, "credential_resolution_failed",
			fmt.Sprintf("external telemetry connector %q credential could not be resolved", connectorName))
	}
	s.shared.Register([]byte(value))
	return &httpapi.MintedCredential{Capability: string(capability.TelemetryRead), Value: value}, nil
}

// locateRun finds the run directory across the configured gaggles (plus the
// legacy ungaggled runs dir), the same candidate walk the intervention
// service uses. Exactly one match is required.
func (s *daemonCredentialService) locateRun(defs credentialPlaneDefinitions, runID string) (string, error) {
	gaggles := make([]string, 0, len(defs.Scopes))
	for gaggle := range defs.Scopes {
		gaggles = append(gaggles, gaggle)
	}
	found, err := locateOwnedRun(s.layout, gaggles, runID, true)
	return found.dir, err
}

// stageProfile is the credential identity of one stage of a pinned
// definition: which goober (if any) it executes as, its declared credential
// capabilities, and the goober's invocation-internal credential keys.
type stageProfile struct {
	goober       string
	harness      string
	capabilities []string
	implicitKeys []string
	// deterministic is true for a deterministic task: the only stage kind a
	// credential-refresh grant is minted for or refreshed for (Goobers#6120
	// phase 1).
	deterministic bool
	// timeout is the pinned task's own timeoutSeconds, zero when unset. It
	// sizes a pod stage's refresh grant.
	timeout time.Duration
	// externalTelemetryConnector is the pinned task's inputs.connector value
	// (#4341), when set — the name of the ONE externalTelemetry.connectors
	// entry this stage's telemetry:read resolution must mint, resolved from
	// the pinned definition rather than trusted from the request the same
	// way every other implicit grant here is.
	externalTelemetryConnector string
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

// gateWorkspaceIsRepoBacked reports whether an agentic gate's reviewer is cut
// a repository checkout. Unlike taskWorkspaceIsRepoBacked, an UNSPECIFIED
// workspace qualifies: a reviewer's working tree is never optional — every
// agentic reviewer, self-placed or pod-placed, is provisioned a repo worktree
// (engine.Activities.ReviewGoober reads "" as the writable repo; the engine's
// dispatchRemoteGate resolves the same default before dispatch) — so
// AgenticGate.Workspace selects the MODE of the checkout, never whether there
// is one. A stage that says nothing has still said "review the repository".
func gateWorkspaceIsRepoBacked(gate apiv1.Gate) bool {
	workspace := gate.EffectiveWorkspace()
	if workspace == "" {
		workspace = apiv1.WorkspaceRepo
	}
	return workspace.IsRepoBacked()
}

// An unknown stage is a typed 404: a pod cannot probe another workflow's
// stage names into grants.
func stageCredentialProfile(machine *workflow.Machine, defs credentialPlaneDefinitions, stage string, loadGateCapabilities func() (map[string][]string, bool, error)) (stageProfile, error) {
	if task, ok := machine.Task(stage); ok {
		profile := stageProfile{
			goober:                     task.Goober,
			capabilities:               append([]string(nil), task.Capabilities...),
			externalTelemetryConnector: task.Inputs[executor.InputTelemetryConnector],
			deterministic:              task.Goober == "" && task.Type != apiv1.TaskAgentic,
			timeout:                    time.Duration(task.TimeoutSeconds) * time.Second,
		}
		if task.Goober != "" {
			spec, ok := defs.Goobers[task.Goober]
			if !ok {
				return stageProfile{}, credentialPlaneError(http.StatusConflict, "goober_unavailable",
					fmt.Sprintf("goober %q for stage %q is no longer configured", task.Goober, stage))
			}
			profile.implicitKeys = mcpconfig.BYOCredentialKeys(spec.MCPServers)
			profile.harness = string(spec.Harness)
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
		profile := stageProfile{
			goober:       reviewer,
			harness:      string(spec.Harness),
			capabilities: append([]string(nil), pinnedCapabilities[reviewer]...),
			implicitKeys: mcpconfig.BYOCredentialKeys(spec.MCPServers),
		}
		// A reviewer evaluated in a pod (decision 001 rulings 7–8) checks the
		// repository out with the same in-pod askpass checkout capability a
		// task uses (#3773/#3777) — the same implicit key, for the same
		// reason, with the same boundary: the pod consumes it inside the
		// checkout and never exports it to the reviewer's environment. No
		// second, weaker credential path is created for gates.
		if gateWorkspaceIsRepoBacked(gate) {
			profile.implicitKeys = append(profile.implicitKeys, string(capability.RepoPush))
		}
		return profile, nil
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
			return buildGaggleCredentials(s.config, s.stores, scope.Project, scope.Backlog, scope.AdditionalRepos, s.shared)
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
	harnessName := profile.harness
	if harnessName == "" {
		harnessName = string(apiv1.HarnessCopilot)
	}
	gooberGrants := buildGooberCredentialGrants(profile.goober, harnessName, credentialKeys, grants)
	return credentials.NewGooberInjectorWithCredentialKeys(resolver, profile.goober, gooberGrants, profile.implicitKeys, s.shared)
}
