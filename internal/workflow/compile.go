package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// knownHarnesses is the set of agent harnesses the compiler admits. v0 ships the
// Copilot CLI adapter only; other harnesses are additional adapters behind the
// same contract (ARCHITECTURE.md §5), added here as they land.
var knownHarnesses = map[apiv1.Harness]bool{
	apiv1.HarnessCopilot: true,
}

type options struct {
	goobers map[string]apiv1.GooberSpec
}

// Option customizes compilation.
type Option func(*options)

// WithGoobers supplies the goober definitions a workflow's agentic stages and
// reviewer gates reference, keyed by goober name. Passing it enables capability
// admission (a stage may only use capabilities granted to its goober) and
// unknown-harness rejection (ARCHITECTURE.md §5). Without it, compilation
// validates only the workflow-intrinsic state machine — which is all the runner
// needs at run time, since capability/harness admission happens at config-
// validation time.
func WithGoobers(goobers map[string]apiv1.GooberSpec) Option {
	return func(o *options) { o.goobers = goobers }
}

// Compile validates a Definition and returns the compiled Machine. It is pure
// (no I/O, no wall clock, no Temporal) and deterministic: the same definition
// always yields the same machine and the same content digest.
//
// It rejects: duplicate state names, a missing/undefined start, transitions to
// undefined states, gates with no branches or branches to undefined states,
// states unreachable from start, loops with no exit to a terminal, and — when
// WithGoobers is supplied — stages using capabilities their goober does not
// grant and goobers on an unknown harness. Errors are aggregated so one compile
// reports every problem, each message actionable on its own.
func Compile(def Definition, opts ...Option) (*Machine, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	m := newMachine(def)

	var problems []string
	problems = append(problems, structuralProblems(m)...)
	// Reachability and loop analysis only make sense on a well-formed graph;
	// when the structure is broken those problems are already reported and the
	// graph walk would only cascade noise.
	if len(problems) == 0 {
		problems = append(problems, reachabilityProblems(m)...)
	}
	problems = append(problems, scheduleProblems(def)...)
	problems = append(problems, admissionProblems(def, o.goobers)...)

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
	}

	digest, err := computeDigest(def)
	if err != nil {
		return nil, fmt.Errorf("digest workflow %q: %w", def.Name, err)
	}
	m.digest = digest
	return m, nil
}

// newMachine builds the state-lookup maps for a definition without validating.
// Duplicate names collapse in the maps; structuralProblems reports them.
func newMachine(def Definition) *Machine {
	m := &Machine{
		Def:   def,
		tasks: make(map[string]apiv1.Task, len(def.Spec.Tasks)),
		gates: make(map[string]apiv1.Gate, len(def.Spec.Gates)),
	}
	for _, t := range def.Spec.Tasks {
		m.tasks[t.Name] = t
	}
	for _, g := range def.Spec.Gates {
		m.gates[g.Name] = g
	}
	return m
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
	} else if !m.has(def.Spec.Start) {
		problems = append(problems, fmt.Sprintf("start state %q is not defined", def.Spec.Start))
	}

	for _, t := range def.Spec.Tasks {
		if !isTerminal(t.Next) && !m.has(t.Next) {
			problems = append(problems, fmt.Sprintf("task %q next state %q is not defined", t.Name, t.Next))
		}
	}
	for _, g := range def.Spec.Gates {
		if len(g.Branches) == 0 {
			problems = append(problems, fmt.Sprintf("gate %q has no branches", g.Name))
		}
		for _, outcome := range sortedKeys(g.Branches) {
			target := g.Branches[outcome]
			if !isTerminal(target) && !m.has(target) {
				problems = append(problems, fmt.Sprintf("gate %q branch %q -> %q is not a defined state", g.Name, outcome, target))
			}
		}
	}
	return problems
}

// outgoing returns the transition targets of a state (task Next, or every gate
// branch target). Terminal targets are included so terminal-reachability can see
// them; undefined targets are the caller's concern.
func (m *Machine) outgoing(state string) []string {
	if t, ok := m.tasks[state]; ok {
		return []string{t.Next}
	}
	if g, ok := m.gates[state]; ok {
		out := make([]string, 0, len(g.Branches))
		for _, k := range sortedKeys(g.Branches) {
			out = append(out, g.Branches[k])
		}
		return out
	}
	return nil
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
		stack = append(stack, m.outgoing(s)...)
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
			for _, t := range m.outgoing(name) {
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

// admissionProblems reports capability and harness violations. It needs the
// referenced goober definitions; with none supplied it is a no-op (the runner
// path, where admission already happened at config-validation time).
func admissionProblems(def Definition, goobers map[string]apiv1.GooberSpec) []string {
	if goobers == nil {
		return nil
	}
	var problems []string

	checkHarness := func(gooberName, ctx string) {
		g, ok := goobers[gooberName]
		if !ok {
			return // existence is the config validator's cross-ref concern.
		}
		h := g.Harness
		if h == "" {
			h = apiv1.HarnessCopilot // schema default
		}
		if !knownHarnesses[h] {
			problems = append(problems, fmt.Sprintf("%s goober %q uses unknown harness %q", ctx, gooberName, h))
		}
	}

	for _, t := range def.Spec.Tasks {
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

// computeDigest returns a stable content digest of the pinned definition. It
// canonicalizes to JSON (encoding/json emits struct fields in declaration order
// and map keys sorted) and hashes the bytes, so semantically identical
// definitions digest identically regardless of YAML formatting.
func computeDigest(def Definition) (string, error) {
	b, err := json.Marshal(def)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
