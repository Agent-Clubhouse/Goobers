package vnext

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/builtincmd"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/providerstage"
	"github.com/goobers/goobers/internal/workflow/internal/model"
)

// builtinManifest is the provider-stage manifest resolved at this
// interpreter's own DSL version: a requirement change gated to a later DSL
// version is invisible here, so editing the manifest for a newer DSL can
// never re-break shipped 2.0 configs (the ed11ae81 class, #3504).
var builtinManifest = providerstage.ForVersion(DSLVersion)

type options struct {
	goobers                    map[string]apiv1.GooberSpec
	knownChecks                map[string]bool
	knownHarnesses             map[string]bool
	allowPreviewFeatures       *bool
	gaggleRequiredCapabilities []string
}

// Option customizes compilation.
type Option func(*options)

// WithGoobers supplies the goober definitions a workflow's agentic stages and
// reviewer gates reference, keyed by goober name. Passing it enables capability
// admission (a stage may only use capabilities granted to its goober).
// WithKnownHarnesses additionally enables unknown-harness rejection
// (ARCHITECTURE.md §5). Without goober definitions, compilation validates only
// the workflow-intrinsic state machine — which is all the runner needs at run
// time, since capability/harness admission happens at config-validation time.
func WithGoobers(goobers map[string]apiv1.GooberSpec) Option {
	return func(o *options) { o.goobers = goobers }
}

// WithKnownChecks supplies the names of every automated check actually
// registered (internal/gate.DefaultChecks()'s keys, or a custom registry's),
// so Compile can catch a typo'd AutomatedGate.Check at compile time instead of
// it failing only when a run actually reaches that gate (#124). Without it,
// check names are not validated — the same "runner path" default as
// WithGoobers, since internal/gate can't be imported here (it already imports
// this package) and already fails closed on an unknown check at evaluation
// time regardless.
func WithKnownChecks(names []string) Option {
	return func(o *options) { o.knownChecks = toSet(names) }
}

// WithKnownHarnesses supplies the names registered in the production harness
// Registry. When WithGoobers is used, a referenced goober's harness must be in
// this set; callers should pass Registry.Names() so runtime lookup and compile
// admission use the same source of truth.
func WithKnownHarnesses(names []string) Option {
	return func(o *options) { o.knownHarnesses = toSet(names) }
}

// WithGaggleRequiredCapabilities supplies the workflow's gaggle-level runner
// capability requirements (GaggleSpec.RequiredCapabilities) so per-stage
// platform admission (#2861) evaluates each stage's EFFECTIVE requirement set
// — the gaggle-level tokens union the stage-level ones, mirroring
// internal/instance's schedule-time merge (WorkflowRequiredCapabilities).
// Callers that hold the gaggle must pass it: a gaggle-level os= token is a
// platform every stage shares, and evaluating stage-level requirements alone
// could flag a transition the shared token proves same-platform.
func WithGaggleRequiredCapabilities(caps []string) Option {
	return func(o *options) { o.gaggleRequiredCapabilities = caps }
}

// PreviewFeaturesAnnotation is the instance Manifest annotation that explicitly
// acknowledges use of unstable preview DSL features.
const PreviewFeaturesAnnotation = "goobers.dev/allow-preview-features"

// WithPreviewFeatures applies an instance's preview-feature acknowledgement to
// compilation. Preview features are rejected when this option is omitted.
func WithPreviewFeatures(enabled bool) Option {
	return func(o *options) { o.allowPreviewFeatures = &enabled }
}

// FeatureDiagnostic describes one support-level finding without coupling the
// workflow package to the validator's severity and code types.
type FeatureDiagnostic struct {
	Feature  Feature
	Blocking bool
	Message  string
}

// CheckFeatureSupport applies support-level policy to resolved DSL features.
func CheckFeatureSupport(features []Feature, allowPreview bool) []FeatureDiagnostic {
	var diagnostics []FeatureDiagnostic
	for _, feature := range features {
		switch feature.Level {
		case SupportPreview:
			diagnostic := FeatureDiagnostic{Feature: feature}
			if allowPreview {
				diagnostic.Message = fmt.Sprintf(
					"DSL feature %q is preview and unstable (available since %s)",
					feature.ID, feature.SinceVersion,
				)
			} else {
				diagnostic.Blocking = true
				diagnostic.Message = fmt.Sprintf(
					"DSL feature %q is preview and requires explicit instance opt-in via Manifest annotation %q: %q",
					feature.ID, PreviewFeaturesAnnotation, "true",
				)
			}
			diagnostics = append(diagnostics, diagnostic)
		case SupportDeprecated:
			diagnostics = append(diagnostics, FeatureDiagnostic{
				Feature: feature,
				Message: fmt.Sprintf(
					"DSL feature %q is deprecated; use %q instead; removal is targeted for %s",
					feature.ID, feature.Replacement, feature.RemovalTargetVersion,
				),
			})
		case SupportRemoved:
			diagnostics = append(diagnostics, FeatureDiagnostic{
				Feature:  feature,
				Blocking: true,
				Message: fmt.Sprintf(
					"DSL feature %q was removed; %s was the last supporting version",
					feature.ID, feature.LastSupportingVersion,
				),
			})
		}
	}
	return diagnostics
}

// CheckWorkflowFeatureSupport resolves a workflow and applies support policy.
func CheckWorkflowFeatureSupport(def Definition, allowPreview bool) []FeatureDiagnostic {
	features, err := FeaturesForWorkflow(def)
	if err != nil {
		return []FeatureDiagnostic{{Blocking: true, Message: err.Error()}}
	}
	return CheckFeatureSupport(features, allowPreview)
}

// CheckGaggleFeatureSupport resolves a gaggle and applies support policy.
func CheckGaggleFeatureSupport(spec apiv1.GaggleSpec, allowPreview bool) []FeatureDiagnostic {
	features, err := FeaturesForGaggle(spec)
	if err != nil {
		return []FeatureDiagnostic{{Blocking: true, Message: err.Error()}}
	}
	return CheckFeatureSupport(features, allowPreview)
}

// CheckGooberFeatureSupport resolves a goober and applies support policy.
func CheckGooberFeatureSupport(spec apiv1.GooberSpec, allowPreview bool) []FeatureDiagnostic {
	features, err := FeaturesForGoober(spec)
	if err != nil {
		return []FeatureDiagnostic{{Blocking: true, Message: err.Error()}}
	}
	return CheckFeatureSupport(features, allowPreview)
}

func blockingFeatureProblems(diagnostics []FeatureDiagnostic) []string {
	var problems []string
	for _, diagnostic := range diagnostics {
		if diagnostic.Blocking {
			problems = append(problems, diagnostic.Message)
		}
	}
	return problems
}

// Compile validates a Definition and returns the compiled Machine. It is pure
// (no I/O, no wall clock, no Temporal) and deterministic: the same definition
// always yields the same machine and the same content digest.
//
// It rejects: duplicate state names, a missing/undefined start, transitions to
// undefined states, gates with no branches or branches to undefined states,
// states unreachable from start, loops with no exit to a terminal, removed DSL
// features, preview DSL features unless WithPreviewFeatures(true) is supplied,
// and — when WithGoobers is supplied — a goober granting or a stage declaring
// a capability outside the canonical registry (internal/capability, issue #74),
// stages using capabilities their goober does not grant, and goobers on an
// unknown harness when WithKnownHarnesses is also supplied. Built-in task
// capability requirements are always enforced (a declared capability that
// explicitly subsumes the required one satisfies it —
// internal/capability.Subsumes); a deterministic shell-out task naming a
// `goobers` subcommand outside the internal/builtincmd inventory is
// rejected (a non-shell inputs.kind exempts its placeholder command); and a
// provably cross-platform task transition handing off unpushed repo
// workspace state is rejected (#2861, see pushBoundaryProblems). Errors are
// aggregated so one compile reports every problem, each message actionable
// on its own.
func Compile(def Definition, opts ...Option) (*Machine, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	scriptProblems := runScriptProblems(def)
	m, err := newMachine(def)
	if err != nil {
		return nil, fmt.Errorf("digest workflow %q: %w", def.Name, err)
	}

	var problems []string
	allowPreview := false
	if o.allowPreviewFeatures != nil {
		allowPreview = *o.allowPreviewFeatures
	}
	problems = append(problems, blockingFeatureProblems(CheckWorkflowFeatureSupport(def, allowPreview))...)
	problems = append(problems, scriptProblems...)
	if o.goobers != nil {
		names := make([]string, 0, len(o.goobers))
		for name := range o.goobers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, problem := range blockingFeatureProblems(CheckGooberFeatureSupport(o.goobers[name], allowPreview)) {
				problems = append(problems, fmt.Sprintf("goober %q: %s", name, problem))
			}
		}
	}
	problems = append(problems, structuralProblems(m)...)
	// Reachability and loop analysis only make sense on a well-formed graph;
	// when the structure is broken those problems are already reported and the
	// graph walk would only cascade noise.
	if len(problems) == 0 {
		problems = append(problems, reachabilityProblems(m)...)
	}
	problems = append(problems, scheduleProblems(def)...)
	problems = append(problems, gateOutcomeProblems(def, o.knownChecks)...)
	problems = append(problems, triggerFieldProblems(def)...)
	problems = append(problems, admissionProblems(def, o.goobers, o.knownHarnesses, true)...)
	problems = append(problems, pushBoundaryProblems(def, o.gaggleRequiredCapabilities)...)
	problems = append(problems, gateVocabProblems(def)...)
	problems = append(problems, gateParamProblems(def)...)
	problems = append(problems, workspaceProblems(def)...)
	problems = append(problems, dottedStateNameProblems(def)...)
	problems = append(problems, parallelProblems(m)...)
	problems = append(problems, claimLedgerPlacementProblems(def)...)

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
	}

	return m, nil
}

func claimLedgerPlacementProblems(def Definition) []string {
	var firstTask string
	var firstPlacement []string
	for _, task := range def.Spec.Tasks {
		if task.Run == nil || len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" ||
			!builtinManifest.MutatesClaimLedger(task.Run.Command[1], task.Run.Command[2:]) {
			continue
		}
		placement := canonicalPlacement(task.RequiredCapabilities)
		if firstTask == "" {
			firstTask = task.Name
			firstPlacement = placement
			continue
		}
		if strings.Join(placement, "\x00") != strings.Join(firstPlacement, "\x00") {
			return []string{fmt.Sprintf(
				"claims-mutating tasks %q and %q declare incompatible requiredCapabilities %q and %q; all claims-mutating tasks must use identical placement requirements",
				firstTask, task.Name, firstPlacement, placement,
			)}
		}
	}
	return nil
}

func canonicalPlacement(required []string) []string {
	placement := append([]string(nil), required...)
	sort.Strings(placement)
	if len(placement) < 2 {
		return placement
	}
	distinct := placement[:1]
	for _, value := range placement[1:] {
		if value != distinct[len(distinct)-1] {
			distinct = append(distinct, value)
		}
	}
	return distinct
}

func runScriptProblems(def Definition) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.Run != nil && task.Run.Command != nil && task.Run.Script != "" {
			problems = append(problems, fmt.Sprintf(
				"task %q: run.command and run.script are mutually exclusive",
				task.Name,
			))
		}
	}
	return problems
}

// newMachine builds the state-lookup maps for a definition without validating.
// Duplicate names collapse in the maps; structuralProblems reports them.
func newMachine(def Definition) (*Machine, error) {
	tasks := make(map[string]apiv1.Task, len(def.Spec.Tasks))
	gates := make(map[string]apiv1.Gate, len(def.Spec.Gates))
	for _, task := range def.Spec.Tasks {
		tasks[task.Name] = task
	}
	for _, gate := range def.Spec.Gates {
		gates[gate.Name] = gate
	}
	parallels := make(map[string]apiv1.Parallel, len(def.Spec.Parallels))
	for _, parallel := range def.Spec.Parallels {
		parallels[parallel.Name] = parallel
	}
	return model.NewMachine(def, tasks, gates, parallels, buildGraph(def))
}

func newMachineForCheck(def Definition) (*Machine, []string) {
	machine, err := newMachine(def)
	if err != nil {
		return nil, []string{fmt.Sprintf("digest workflow %q: %v", def.Name, err)}
	}
	return machine, nil
}

// structuralProblems reports state-machine integrity errors: duplicate names, a
// missing/undefined start, and transitions/branches that do not resolve.
func structuralProblems(m *Machine) []string {
	def := m.Def
	var problems []string

	seen := make(map[string]bool, len(def.Spec.Tasks)+len(def.Spec.Gates)+len(def.Spec.Parallels))
	dup := func(name string) {
		if seen[name] {
			problems = append(problems, fmt.Sprintf("duplicate state %q", name))
		}
		seen[name] = true
	}
	for _, t := range def.Spec.Tasks {
		dup(t.Name)
	}
	for _, g := range def.Spec.Gates {
		dup(g.Name)
	}
	for _, p := range def.Spec.Parallels {
		dup(p.Name)
	}

	if def.Spec.Start == TerminalComplete {
		problems = append(problems, "start state is empty")
	} else if !m.Has(def.Spec.Start) {
		problems = append(problems, fmt.Sprintf("start state %q is not defined", def.Spec.Start))
	}

	for _, t := range def.Spec.Tasks {
		if isStateName(t.Next) && !m.Has(t.Next) {
			problems = append(problems, fmt.Sprintf("task %q next state %q is not defined", t.Name, t.Next))
		}
		if t.MinimumIntegrity != "" && !t.MinimumIntegrity.Valid() {
			problems = append(problems, fmt.Sprintf(
				"task %q minimumIntegrity %q is not one of trusted, maintainer, unapproved, derived",
				t.Name, t.MinimumIntegrity,
			))
		}
		contextSources := make(map[string]bool, len(t.ContextFrom))
		for _, source := range t.ContextFrom {
			if contextSources[source] {
				problems = append(problems, fmt.Sprintf("task %q contextFrom source %q is duplicated", t.Name, source))
				continue
			}
			contextSources[source] = true
			if _, isTask := m.Task(source); isTask {
				continue
			}
			if _, isGate := m.Gate(source); !isGate {
				problems = append(problems, fmt.Sprintf("task %q contextFrom source %q is not a defined task or gate", t.Name, source))
			}
		}
		switch t.OnTimeout {
		case "", apiv1.TaskOnTimeoutFail, apiv1.TaskOnTimeoutSalvage:
		default:
			problems = append(problems, fmt.Sprintf("task %q onTimeout %q is not one of fail, salvage", t.Name, t.OnTimeout))
		}
		// Salvage completes a timed-out stage with its committed diff (#724) —
		// only meaningful for an agentic stage whose deliverable is that diff; a
		// deterministic stage has no such session to time out and salvage.
		if t.OnTimeout == apiv1.TaskOnTimeoutSalvage && t.Type != apiv1.TaskAgentic {
			problems = append(problems, fmt.Sprintf("task %q onTimeout=salvage requires an agentic task", t.Name))
		}
	}
	for _, g := range def.Spec.Gates {
		if len(g.Branches) == 0 {
			problems = append(problems, fmt.Sprintf("gate %q has no branches", g.Name))
		}
		for _, outcome := range sortedKeys(g.Branches) {
			target := g.Branches[outcome]
			if isStateName(target) && !m.Has(target) {
				problems = append(problems, fmt.Sprintf("gate %q branch %q -> %q is not a defined state", g.Name, outcome, target))
			}
		}
	}
	return problems
}

// reachabilityProblems reports states unreachable from the start and states that
// are reachable but cannot reach any terminal (a loop with no exit — WF-015
// within a run). It assumes a structurally valid graph (see Compile).
func reachabilityProblems(m *Machine) []string {
	def := m.Def
	var problems []string

	// Forward reachability from start.
	reachable := map[string]bool{}
	stack := []string{def.Spec.Start}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if isTerminal(s) || reachable[s] {
			continue
		}
		reachable[s] = true
		stack = append(stack, m.Outgoing(s)...)
	}

	// Any defined state not reached from start is dead config.
	for _, name := range stateNames(def) {
		if !reachable[name] {
			problems = append(problems, fmt.Sprintf("state %q is unreachable from start %q", name, def.Spec.Start))
		}
	}

	// Terminal-reachability: a state can reach a terminal if any outgoing edge is
	// terminal or leads to a state that can. Fixed-point over the reachable set.
	canExit := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, name := range stateNames(def) {
			if canExit[name] {
				continue
			}
			for _, t := range m.Outgoing(name) {
				// Reaching @join settles a BRANCH; the run's own exit is then
				// guaranteed by the join state, whose canExit is computed
				// independently (and by the parallel, whose Outgoing includes
				// it). A non-branch state abusing @join is caught by rule 4 in
				// parallelProblems, not here.
				if isTerminal(t) || model.IsReservedBranchTarget(t) || canExit[t] {
					canExit[name] = true
					changed = true
					break
				}
			}
		}
	}
	for _, name := range stateNames(def) {
		if reachable[name] && !canExit[name] {
			problems = append(problems, fmt.Sprintf("state %q cannot reach a terminal outcome (loop with no exit)", name))
		}
	}
	return problems
}

// admissionProblems reports capability and harness violations. Built-in task
// requirements are intrinsic to the workflow and always checked; goober grant
// and harness checks require the referenced goober definitions.
func admissionProblems(def Definition, goobers map[string]apiv1.GooberSpec, knownHarnesses map[string]bool, checkAllGooberCapabilities bool) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.NestedAgentPolicy == nil {
			continue
		}
		if task.Type != apiv1.TaskAgentic {
			problems = append(problems, fmt.Sprintf("task %q: nestedAgentPolicy is only valid for agentic tasks", task.Name))
			continue
		}
		if err := task.NestedAgentPolicy.Validate(); err != nil {
			problems = append(problems, fmt.Sprintf("task %q: %v", task.Name, err))
		}
	}
	for _, t := range def.Spec.Tasks {
		capabilities := toSet(t.Capabilities)
		if t.Run != nil && len(t.Run.Command) >= 2 && t.Run.Command[0] == "goobers" {
			subcommand := t.Run.Command[1]
			// A deterministic stage that actually shells out (inputs.kind empty
			// or "shell") may only invoke an inventoried built-in subcommand
			// (internal/builtincmd): anything else reaches the CLI's own
			// unknown-command error only at runtime, after the run has already
			// claimed work and provisioned a worktree. The kind exemption is
			// load-bearing: a non-shell inputs.kind (ci-poll,
			// external-telemetry) dispatches on the kind and the command is a
			// schema-required placeholder that must stay unchecked.
			if t.Type == apiv1.TaskDeterministic && isShellStage(t) && !builtincmd.Known(subcommand) {
				problems = append(problems, unknownSubcommand(t.Name, subcommand))
			}
			for _, use := range builtinManifest.RequiredCapabilities(subcommand, t.Run.Command[2:]) {
				// Most requirements accept an explicitly subsuming capability,
				// but separately brokered credentials must be declared exactly.
				satisfied := anyCapabilitySatisfies(t.Capabilities, use.Capability)
				if use.RequiresExactCapability() {
					satisfied = hasExactCapability(t.Capabilities, use.Capability)
				}
				if !satisfied {
					var credential string
					if use.RequiresExactCapability() {
						credential = fmt.Sprintf(" (requires %s)", capability.CredentialEnvVar(string(use.Capability)))
					}
					problems = append(problems, fmt.Sprintf(
						"task %q invokes built-in subcommand %q but does not declare capability %q%s; %s",
						t.Name, subcommand, use.Capability, credential, use.Consequence,
					))
				}
			}
		}
		if t.Inputs["kind"] == "ci-poll" && !capabilities[string(capability.ProviderPRWrite)] {
			problems = append(problems, fmt.Sprintf("task %q with inputs.kind=%q must declare capability %q", t.Name, "ci-poll", capability.ProviderPRWrite))
		}
		if capabilities[string(capability.ProviderPRWrite)] &&
			(capabilities[string(capability.GitHubPRWrite)] || capabilities[string(capability.ADOPRWrite)]) {
			problems = append(problems, fmt.Sprintf("task %q declares mutually exclusive provider-neutral and provider-specific PR write capabilities", t.Name))
		}
		if t.Inputs["kind"] == "external-telemetry" && !capabilities[string(capability.TelemetryRead)] {
			problems = append(problems, fmt.Sprintf("task %q with inputs.kind=%q must declare capability %q", t.Name, "external-telemetry", capability.TelemetryRead))
		}
		for _, c := range t.Capabilities {
			if capability.Known(c) && !capability.StageDeclarable(c) {
				problems = append(problems, fmt.Sprintf("task %q declares runner-only capability %q", t.Name, c))
			}
		}
	}
	problems = append(problems, policyActionProblems(def, goobers)...)
	if goobers == nil {
		return problems
	}

	if checkAllGooberCapabilities {
		// Every granted capability must be a canonical one (internal/capability,
		// issue #74) — sorted for deterministic error ordering, since map
		// iteration order is not.
		names := make([]string, 0, len(goobers))
		for name := range goobers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, c := range goobers[name].Capabilities {
				if !capability.Known(c) {
					problems = append(problems, fmt.Sprintf("goober %q grants %s", name, unknownCapability(c)))
				} else if !capability.StageDeclarable(c) {
					problems = append(problems, fmt.Sprintf("goober %q grants runner-only capability %q", name, c))
				}
			}
		}
	}

	checkHarness := func(gooberName, ctx string) (apiv1.GooberSpec, apiv1.Harness, bool) {
		g, ok := goobers[gooberName]
		if !ok {
			return apiv1.GooberSpec{}, "", false // existence is the config validator's cross-ref concern.
		}
		h := g.Harness
		if h == "" {
			h = apiv1.HarnessCopilot // schema default
		}
		if knownHarnesses != nil && !knownHarnesses[string(h)] {
			problems = append(problems, fmt.Sprintf("%s goober %q uses unknown harness %q", ctx, gooberName, h))
		}
		if requiresModelCapability(h, knownHarnesses) && !toSet(g.Capabilities)[string(capability.AgentModel)] {
			problems = append(problems, fmt.Sprintf(
				"%s uses goober %q (harness: %s) but the goober does not grant capability %q; the harness will receive no model credential",
				ctx, gooberName, h, capability.AgentModel,
			))
		}
		return g, h, true
	}

	for _, t := range def.Spec.Tasks {
		// A capability string must be canonical (internal/capability, #74)
		// regardless of task type — a deterministic task's Capabilities feed
		// its own credential resolution exactly like an agentic task's do
		// (internal/executor's stage env, #18), so a typo here is the same
		// SEC-042 drift class either way, not just an agentic-task concern
		// (#124: this loop previously skipped every deterministic task
		// entirely).
		for _, cap := range t.Capabilities {
			if !capability.Known(cap) {
				problems = append(problems, fmt.Sprintf("task %q declares %s", t.Name, unknownCapability(cap)))
			}
		}
		if t.Type != apiv1.TaskAgentic || t.Goober == "" {
			continue
		}
		g, h, ok := checkHarness(t.Goober, fmt.Sprintf("task %q", t.Name))
		if !ok {
			continue
		}
		grants := toSet(g.Capabilities)
		for _, cap := range t.Capabilities {
			if !grants[cap] {
				problems = append(problems, fmt.Sprintf("task %q uses capability %q not granted to goober %q", t.Name, cap, t.Goober))
			}
		}
		taskCapabilities := toSet(t.Capabilities)
		if requiresModelCapability(h, knownHarnesses) && !taskCapabilities[string(capability.AgentModel)] {
			problems = append(problems, fmt.Sprintf(
				"task %q uses goober %q (harness: %s) but does not declare capability %q; the harness will receive no model credential",
				t.Name, t.Goober, h, capability.AgentModel,
			))
		}
		requiredMCPCapabilities := map[string]bool{}
		for _, server := range g.MCPServers {
			for _, ref := range server.CredentialRefs {
				requiredMCPCapabilities[ref.Capability] = true
			}
		}
		requiredNames := make([]string, 0, len(requiredMCPCapabilities))
		for name := range requiredMCPCapabilities {
			requiredNames = append(requiredNames, name)
		}
		sort.Strings(requiredNames)
		for _, name := range requiredNames {
			if !taskCapabilities[name] {
				problems = append(problems, fmt.Sprintf(
					"task %q must declare MCP credential capability %q required by goober %q",
					t.Name, name, t.Goober,
				))
			}
		}
	}
	for _, gate := range def.Spec.Gates {
		if gate.Evaluator == apiv1.EvaluatorAgentic && gate.Agentic != nil && gate.Agentic.Goober != "" {
			_, _, _ = checkHarness(gate.Agentic.Goober, fmt.Sprintf("gate %q reviewer", gate.Name))
		}
	}
	return problems
}

func requiresModelCapability(h apiv1.Harness, knownHarnesses map[string]bool) bool {
	return knownHarnesses != nil && (h == apiv1.HarnessCopilot || h == apiv1.HarnessClaudeCode)
}

func unknownCapability(value string) string {
	message := fmt.Sprintf("unknown capability %q", value)
	if suggestion, ok := capability.Suggest(value); ok {
		message += fmt.Sprintf(" (did you mean %q?)", suggestion)
	}
	return message
}

// anyCapabilitySatisfies reports whether any declared capability satisfies a
// requirement for required — exact membership or an explicit subsumption
// (internal/capability.Subsumes).
func anyCapabilitySatisfies(declared []string, required capability.Capability) bool {
	for _, held := range declared {
		if capability.Subsumes(capability.Capability(held), required) {
			return true
		}
	}
	return false
}

func hasExactCapability(declared []string, required capability.Capability) bool {
	for _, held := range declared {
		if capability.Capability(held) == required {
			return true
		}
	}
	return false
}

func unknownSubcommand(taskName, subcommand string) string {
	message := fmt.Sprintf(
		"task %q shells out to unknown built-in subcommand %q (not in the built-in inventory; the stage would only fail at runtime)",
		taskName, subcommand,
	)
	if suggestion, ok := builtincmd.Suggest(subcommand); ok {
		message += fmt.Sprintf(" (did you mean %q?)", suggestion)
	}
	return message
}

// pushBoundaryProblems rejects a provably cross-platform task transition that
// hands off unpushed repo workspace state (#2861, admission-scoped MVP): when
// task A transitions to task B (directly or through gates), BOTH declare
// explicit os= runner-capability tokens pinning disjoint platforms (each
// stage's EFFECTIVE set — gaggle-level requirements union stage-level ones),
// A runs in a writable repo workspace, and A is not itself a push boundary (a
// stage invoking the push-branch or push-remediated built-in), then B's
// runner can never see A's worktree state — the branch must be published
// before the platform crossing. Stages WITHOUT explicit os= tokens are never
// flagged: absence of a token is not absence of a platform difference, but
// admission cannot prove one, and only provable crossings reject (the
// conservative-noise tradeoff was ruled against).
func pushBoundaryProblems(def Definition, gaggleRequired []string) []string {
	tasks := make(map[string]apiv1.Task, len(def.Spec.Tasks))
	for _, t := range def.Spec.Tasks {
		tasks[t.Name] = t
	}
	gates := make(map[string]apiv1.Gate, len(def.Spec.Gates))
	for _, g := range def.Spec.Gates {
		gates[g.Name] = g
	}

	var problems []string
	for _, t := range def.Spec.Tasks {
		upstream := osTokens(gaggleRequired, t.RequiredCapabilities)
		if len(upstream) == 0 || !writesRepoWorkspace(t) || isPushBoundaryStage(t) {
			continue
		}
		for _, successor := range taskSuccessorsThroughGates(t, tasks, gates) {
			downstream := osTokens(gaggleRequired, tasks[successor].RequiredCapabilities)
			if len(downstream) == 0 || tokenSetsIntersect(upstream, downstream) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"task %q (%s) writes repo workspace state and transitions to task %q (%s) on a provably different platform with no push boundary; make %q invoke the push-branch (or push-remediated) built-in, or insert such a stage on %q's platform before the transition, so the branch is published before crossing runners",
				t.Name, strings.Join(upstream, ", "), successor, strings.Join(downstream, ", "), t.Name, t.Name,
			))
		}
	}
	return problems
}

// osTokens returns the sorted, de-duplicated os= tokens in a stage's
// effective runner-capability requirements — its gaggle's RequiredCapabilities
// union its own (the same merge internal/instance applies at schedule time).
func osTokens(gaggleRequired, stageRequired []string) []string {
	seen := map[string]bool{}
	var tokens []string
	for _, c := range append(append([]string(nil), gaggleRequired...), stageRequired...) {
		if strings.HasPrefix(c, "os=") && !seen[c] {
			seen[c] = true
			tokens = append(tokens, c)
		}
	}
	sort.Strings(tokens)
	return tokens
}

func tokenSetsIntersect(a, b []string) bool {
	set := toSet(a)
	for _, token := range b {
		if set[token] {
			return true
		}
	}
	return false
}

// writesRepoWorkspace reports whether a task runs on the writable repository
// worktree — Run.Workspace when set (authoritative for deterministic tasks),
// else Task.Workspace, else the historical writable-repo default.
func writesRepoWorkspace(t apiv1.Task) bool {
	workspace := t.Workspace
	if t.Run != nil && t.Run.Workspace != "" {
		workspace = t.Run.Workspace
	}
	return workspace == "" || workspace.IsWritableRepo()
}

// isPushBoundaryStage reports whether a task is itself the push boundary: a
// shell-out to the push-branch or push-remediated built-in, which publishes
// the run branch so a subsequent stage on any runner can fetch it.
func isPushBoundaryStage(t apiv1.Task) bool {
	if !isShellStage(t) || len(t.Run.Command) < 2 || t.Run.Command[0] != "goobers" {
		return false
	}
	return t.Run.Command[1] == "push-branch" || t.Run.Command[1] == "push-remediated"
}

// taskSuccessorsThroughGates returns the names of every task reachable from
// t's Next by crossing only gates — the tasks that can run immediately after
// t with no intermediate stage. Terminals, reserved targets, and parallel
// states end the walk (a parallel's branches are scheduled by the
// orchestrator, not handed t's worktree). The result is deterministic:
// discovery order with gate branches visited in sorted-outcome order.
func taskSuccessorsThroughGates(t apiv1.Task, tasks map[string]apiv1.Task, gates map[string]apiv1.Gate) []string {
	var successors []string
	seenTasks := map[string]bool{}
	seenGates := map[string]bool{}
	var walk func(target string)
	walk = func(target string) {
		if !isStateName(target) {
			return
		}
		if _, ok := tasks[target]; ok {
			if !seenTasks[target] {
				seenTasks[target] = true
				successors = append(successors, target)
			}
			return
		}
		gate, ok := gates[target]
		if !ok || seenGates[target] {
			return
		}
		seenGates[target] = true
		for _, outcome := range sortedKeys(gate.Branches) {
			walk(gate.Branches[outcome])
		}
	}
	walk(t.Next)
	return successors
}

// agenticOutcomes is the closed set of decisions an agentic gate's reviewer
// can produce (apiv1.VerdictDecision, envelope.go). Every agentic gate's
// Branches must cover all three: an evaluator returning a decision with no
// matching branch fails closed mid-run today (internal/gate/evaluate.go's
// "outcome has no defined branch" error) even though the set of possible
// decisions is fully known at compile time (#124).
var agenticOutcomes = []string{"pass", "fail", "needs-changes"}

// automatedBuiltinOutcomes is the default outcome set for a check in
// internal/gate.DefaultChecks — every check is boolean (pass/fail) except
// the exceptions listed in automatedCheckOutcomes. V0 ships no mechanism for
// a config-defined gate to select a custom CheckFunc with a different
// outcome set (AutomatedGate.Check always resolves against that fixed
// registry in production), so these two tables are exhaustive for every
// gate a real config can express today. If a custom, non-boolean check
// registry is ever wired into config, this assumption is the first thing to
// revisit.
var automatedBuiltinOutcomes = []string{"pass", "fail"}

// automatedCheckOutcomes overrides automatedBuiltinOutcomes for a specific
// check name. "ci-status" has a third timeout outcome (#239), and
// "failure-class" has a third retryable-infrastructure outcome (#1970).
// A ci-poll timeout
// surfaces as OutcomeTimeout ("timeout"), distinct from pass/fail, so a
// workflow's ci-gate can route it to escalation instead of the "fail"
// branch's implement repass — that third outcome must be just as
// compile-time-checkable (a branch declared for it resolves; a missing
// branch fails closed) as pass/fail already are.
var automatedCheckOutcomes = map[string][]string{
	"ci-status":     {"pass", "fail", "timeout"},
	"failure-class": {"pass", "fail", "infra"},
	// "land-outcome"/"queue-outcome" (issue #758): merge-policy abstraction
	// — a merge-pr stage that actually landed a pull request reports
	// whether it merged directly or only enqueued it, and a subsequent
	// merge-queue-poll stage reports whether the queue went on to merge,
	// evict, or time out watching it. See internal/gate.DefaultChecks'
	// doc comments on each check.
	"land-outcome":  {"merged", "enqueued", "fail"},
	"queue-outcome": {"merged", "evicted", "timeout", "fail"},
}

// gateOutcomeProblems reports two distinct defect classes per gate (#124):
//   - a branch key that is not one of the evaluator's producible outcomes —
//     silently dead configuration, never taken;
//   - a producible outcome with no matching branch — the evaluator can
//     return it, but the gate has nowhere to send it, which today only fails
//     at evaluation time instead of at compile time.
//
// Human gates accept the workflow's declared branch vocabulary as explicit
// decisions and are skipped here because there is no smaller closed outcome
// set to validate. knownChecks, when non-nil, additionally flags an
// AutomatedGate.Check name outside the supplied
// registry (WithKnownChecks) — nil performs no such check (the default;
// internal/gate already fails closed on an unknown check at evaluation time
// regardless).
func gateOutcomeProblems(def Definition, knownChecks map[string]bool) []string {
	var problems []string
	for _, g := range def.Spec.Gates {
		var producible []string
		switch g.Evaluator {
		case apiv1.EvaluatorAgentic:
			producible = agenticOutcomes
		case apiv1.EvaluatorAutomated:
			producible = automatedBuiltinOutcomes
			if g.Automated != nil {
				if custom, ok := automatedCheckOutcomes[g.Automated.Check]; ok {
					producible = custom
				}
				if knownChecks != nil && g.Automated.Check != "" && !knownChecks[g.Automated.Check] {
					problems = append(problems, fmt.Sprintf("gate %q: unknown automated check %q", g.Name, g.Automated.Check))
				}
			}
		default:
			continue
		}
		want := toSet(producible)
		for _, outcome := range sortedKeys(g.Branches) {
			if outcome == BranchEscalate {
				continue
			}
			if !want[outcome] {
				problems = append(problems, fmt.Sprintf("gate %q: branch %q is not a producible outcome for this evaluator (never taken)", g.Name, outcome))
			}
		}
		var uncovered []string
		for _, outcome := range producible {
			if _, ok := g.Branches[outcome]; !ok {
				uncovered = append(uncovered, outcome)
			}
		}
		if len(uncovered) == 1 {
			problems = append(problems, fmt.Sprintf("gate %q: producible outcome %q has no branch (would fail closed at evaluation time)", g.Name, uncovered[0]))
		} else if len(uncovered) > 1 {
			quoted := make([]string, len(uncovered))
			for i, outcome := range uncovered {
				quoted[i] = fmt.Sprintf("%q", outcome)
			}
			problems = append(problems, fmt.Sprintf("gate %q: producible outcomes %s have no branches (would fail closed at evaluation time)", g.Name, strings.Join(quoted, ", ")))
		}
	}
	return problems
}
