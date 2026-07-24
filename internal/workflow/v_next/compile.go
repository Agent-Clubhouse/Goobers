package vnext

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/workflow/internal/model"
)

type options struct {
	goobers              map[string]apiv1.GooberSpec
	knownChecks          map[string]bool
	knownHarnesses       map[string]bool
	allowPreviewFeatures *bool
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
// human evaluators while durable pause/resume is unavailable, states
// unreachable from start, loops with no exit to a terminal, removed DSL
// features, preview DSL features unless WithPreviewFeatures(true) is supplied,
// and — when WithGoobers is supplied — a goober granting or a stage declaring
// a capability outside the canonical registry (internal/capability, issue #74),
// stages using capabilities their goober does not grant, and goobers on an
// unknown harness when WithKnownHarnesses is also supplied. Built-in task
// capability requirements are always enforced. Errors are aggregated so one
// compile reports every problem, each message actionable on its own.
func Compile(def Definition, opts ...Option) (*Machine, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

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
	problems = append(problems, evaluatorSupportProblems(def)...)
	problems = append(problems, gateOutcomeProblems(def, o.knownChecks)...)
	problems = append(problems, triggerFieldProblems(def)...)
	problems = append(problems, admissionProblems(def, o.goobers, o.knownHarnesses, true)...)
	problems = append(problems, gateVocabProblems(def)...)
	problems = append(problems, gateParamProblems(def)...)
	problems = append(problems, workspaceProblems(def)...)

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
	}

	return m, nil
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
	return model.NewMachine(def, tasks, gates, buildGraph(def))
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

	seen := make(map[string]bool, len(def.Spec.Tasks)+len(def.Spec.Gates))
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

	if def.Spec.Start == TerminalComplete {
		problems = append(problems, "start state is empty")
	} else if !m.Has(def.Spec.Start) {
		problems = append(problems, fmt.Sprintf("start state %q is not defined", def.Spec.Start))
	}

	for _, t := range def.Spec.Tasks {
		if !isTerminal(t.Next) && !m.Has(t.Next) {
			problems = append(problems, fmt.Sprintf("task %q next state %q is not defined", t.Name, t.Next))
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
			if !isTerminal(target) && !m.Has(target) {
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
				if isTerminal(t) || canExit[t] {
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
	for _, t := range def.Spec.Tasks {
		if t.Inputs["kind"] == "ci-poll" && !toSet(t.Capabilities)[string(capability.GitHubPRWrite)] {
			problems = append(problems, fmt.Sprintf("task %q with inputs.kind=%q must declare capability %q", t.Name, "ci-poll", capability.GitHubPRWrite))
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

	checkHarness := func(gooberName, ctx string) {
		g, ok := goobers[gooberName]
		if !ok {
			return // existence is the config validator's cross-ref concern.
		}
		h := g.Harness
		if h == "" {
			h = apiv1.HarnessCopilot // schema default
		}
		if knownHarnesses != nil && !knownHarnesses[string(h)] {
			problems = append(problems, fmt.Sprintf("%s goober %q uses unknown harness %q", ctx, gooberName, h))
		}
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
		checkHarness(t.Goober, fmt.Sprintf("task %q", t.Name))
		g, ok := goobers[t.Goober]
		if !ok {
			continue
		}
		grants := toSet(g.Capabilities)
		for _, cap := range t.Capabilities {
			if !grants[cap] {
				problems = append(problems, fmt.Sprintf("task %q uses capability %q not granted to goober %q", t.Name, cap, t.Goober))
			}
		}
	}
	for _, gate := range def.Spec.Gates {
		if gate.Evaluator == apiv1.EvaluatorAgentic && gate.Agentic != nil && gate.Agentic.Goober != "" {
			checkHarness(gate.Agentic.Goober, fmt.Sprintf("gate %q reviewer", gate.Name))
		}
	}
	return problems
}

func unknownCapability(value string) string {
	message := fmt.Sprintf("unknown capability %q", value)
	if suggestion, ok := capability.Suggest(value); ok {
		message += fmt.Sprintf(" (did you mean %q?)", suggestion)
	}
	return message
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
// check name. "ci-status" is the one exception (#239): a ci-poll timeout
// surfaces as OutcomeTimeout ("timeout"), distinct from pass/fail, so a
// workflow's ci-gate can route it to escalation instead of the "fail"
// branch's implement repass — that third outcome must be just as
// compile-time-checkable (a branch declared for it resolves; a missing
// branch fails closed) as pass/fail already are.
var automatedCheckOutcomes = map[string][]string{
	"ci-status": {"pass", "fail", "timeout"},
	// "land-outcome"/"queue-outcome" (issue #758): merge-policy abstraction
	// — a merge-pr stage that actually landed a pull request reports
	// whether it merged directly or only enqueued it, and a subsequent
	// merge-queue-poll stage reports whether the queue went on to merge,
	// evict, or time out watching it. See internal/gate.DefaultChecks'
	// doc comments on each check.
	"land-outcome":  {"merged", "enqueued", "fail"},
	"queue-outcome": {"merged", "evicted", "timeout", "fail"},
}

const humanGateUnsupportedMessage = "human gates ship with durable pause/resume (#168/#465); until then use an automated gate or remove this block"

func evaluatorSupportProblems(def Definition) []string {
	var problems []string
	for _, g := range def.Spec.Gates {
		if g.Evaluator == apiv1.EvaluatorHuman {
			problems = append(problems, fmt.Sprintf("gate %q: %s", g.Name, humanGateUnsupportedMessage))
		}
	}
	return problems
}

// gateOutcomeProblems reports two distinct defect classes per gate (#124):
//   - a branch key that is not one of the evaluator's producible outcomes —
//     silently dead configuration, never taken;
//   - a producible outcome with no matching branch — the evaluator can
//     return it, but the gate has nowhere to send it, which today only fails
//     at evaluation time instead of at compile time.
//
// Human gates have no evaluator outcome to check against (§5: "a human gate
// executes nothing") and are skipped here; evaluatorSupportProblems rejects
// them until durable pause/resume ships. knownChecks, when non-nil,
// additionally flags an AutomatedGate.Check name outside the supplied
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
