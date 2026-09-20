package workflowsafety

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runcontrol"
	wf "github.com/goobers/goobers/internal/workflow"
)

// Safety diagnostic codes and fixed limits for advisory graph exploration.
const (
	EvidenceCode   = "SAF001"
	PublishCode    = "SAF002"
	FeedbackCode   = "SAF003"
	RecoveryCode   = "SAF004"
	CycleCode      = "SAF005"
	CoverageCode   = "SAF006"
	SuppressedCode = "SAF007"
	maxExpansions  = 2048
	maxDepth       = 128
	maxFindings    = 128
)

// Codes returns the closed set of strict-neutral safety warning codes.
func Codes() []string {
	return []string{EvidenceCode, PublishCode, FeedbackCode, RecoveryCode, CycleCode, CoverageCode, SuppressedCode}
}

// Details is additive diagnostic metadata shared by CLI and persistent status.
type Details struct {
	Version           string   `json:"version"`
	ID                string   `json:"id"`
	Gaggle            string   `json:"gaggle"`
	Workflow          string   `json:"workflow"`
	Stage             string   `json:"stage"`
	File              string   `json:"file,omitempty"`
	Line              int      `json:"line,omitempty"`
	Col               int      `json:"col,omitempty"`
	WitnessPath       []string `json:"witnessPath"`
	Confidence        string   `json:"confidence"`
	Coverage          string   `json:"coverage"`
	Impact            string   `json:"impact"`
	Action            string   `json:"action"`
	Limitations       string   `json:"limitations"`
	Budget            int      `json:"budget,omitempty"`
	BudgetSource      string   `json:"budgetSource,omitempty"`
	SuppressedCode    string   `json:"suppressedCode,omitempty"`
	SuppressionReason string   `json:"suppressionReason,omitempty"`
}

// Finding couples a safety rule and summary with its scope and evidence.
type Finding struct {
	Code    string
	Summary string
	Details Details
}

// Message renders the shared CLI, daemon, and status explanation.
func (f Finding) Message() string {
	d := f.Details
	message := fmt.Sprintf("workflow %q/%q stage %q: %s Path: %s. Impact: %s Action: %s Confidence: %s; coverage: %s. %s",
		d.Gaggle, d.Workflow, d.Stage, f.Summary, strings.Join(d.WitnessPath, " -> "), d.Impact, d.Action, d.Confidence, d.Coverage, d.Limitations)
	if d.Line > 0 {
		message += fmt.Sprintf(" Source: %s:%d:%d.", d.File, d.Line, d.Col)
	}
	if d.Budget > 0 {
		message += fmt.Sprintf(" Policy repass bound: %d (%s); infrastructure retries use their separate runtime budget.", d.Budget, d.BudgetSource)
	}
	if d.SuppressedCode != "" {
		message += fmt.Sprintf(" Suppressed %s: %s.", d.SuppressedCode, d.SuppressionReason)
	}
	return message + " Advisory only; the workflow remains enabled."
}

// Options supplies inherited controls and diagnostic identity, never adapters.
type Options struct {
	GaggleRunControls *apiv1.RunControls
	// BinaryIdentity participates in finding identity, not execution identity.
	BinaryIdentity string
}

type frame struct {
	state       string
	path        []string
	visited     []string
	patch       bool
	unknown     bool
	codeSubject bool
	pr          bool
	rebound     bool
	// Each pending rejection is scoped to its gate, never satisfied by a
	// publisher of another verdict.
	pending      []string
	feedback     string
	lastTask     string
	cycleStart   string
	cycleChange  bool
	cycleUnknown bool
}

type analyzer struct {
	m          *wf.Machine
	opts       Options
	contracts  Contracts
	findings   []Finding
	reported   map[string]bool
	seen       map[string]bool
	expansions int
	bounded    bool
}

// Analyze walks the actual compiled graph. It does not compile another
// interpretation of YAML and never invokes a task, provider, or agent.
func Analyze(m *wf.Machine, opts Options) []Finding {
	a := &analyzer{m: m, opts: opts, reported: map[string]bool{}, seen: map[string]bool{}}
	c, err := parseContracts(m)
	a.contracts = c
	if err != nil {
		a.add(CoverageCode, m.Def.Spec.Start, nil, err.Error(),
			"Custom assertions and suppressions could not be interpreted.",
			"Correct the optional safety annotation; built-in analysis still runs.", "unknown", "incomplete", 0, "")
	}
	a.walk(frame{state: m.Def.Spec.Start})
	if a.bounded {
		a.addBoundFinding()
	}
	sort.Slice(a.findings, func(i, j int) bool {
		x, y := a.findings[i], a.findings[j]
		if x.Details.Stage != y.Details.Stage {
			return x.Details.Stage < y.Details.Stage
		}
		if x.Code != y.Code {
			return x.Code < y.Code
		}
		return strings.Join(x.Details.WitnessPath, "\x00") < strings.Join(y.Details.WitnessPath, "\x00")
	})
	return a.findings
}

func (a *analyzer) add(code, stage string, path []string, summary, impact, action, confidence, coverage string, budget int, source string) {
	if len(path) == 0 {
		path = []string{stage}
	}
	key := code + "\x00" + stage + "\x00" + summary
	if a.reported[key] {
		return
	}
	if len(a.findings) >= maxFindings {
		a.bounded = true
		return
	}
	a.reported[key] = true
	d := Details{
		Version: CatalogVersion, Gaggle: a.m.Def.Spec.Gaggle, Workflow: a.m.Def.Name, Stage: stage,
		WitnessPath: append([]string{}, path...), Confidence: confidence, Coverage: coverage,
		Impact: impact, Action: action, Budget: budget, BudgetSource: source,
		Limitations: "Static wiring only, not live delivery or a proof of safety; custom declarations are author assertions.",
	}
	for _, s := range a.contracts.Suppressions {
		if s.Code == code && (s.Stage == "" || s.Stage == stage) {
			d.SuppressedCode, d.SuppressionReason = code, s.Reason
			code = SuppressedCode
			break
		}
	}
	controls, _ := json.Marshal(a.opts.GaggleRunControls)
	identity := strings.Join([]string{CatalogVersion, a.opts.BinaryIdentity, a.m.Digest(), string(controls), key, strings.Join(path, "\x00")}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	d.ID = hex.EncodeToString(sum[:])
	a.findings = append(a.findings, Finding{Code: code, Summary: summary, Details: d})
}

func (a *analyzer) addBoundFinding() {
	// Reserve a visible report even if the diagnostic cap itself was hit.
	if len(a.findings) >= maxFindings {
		a.findings = a.findings[:maxFindings-1]
	}
	a.add(CoverageCode, a.m.Def.Spec.Start, nil,
		fmt.Sprintf("analysis stopped at its bound (%d expansions, %d path hops, %d findings)", maxExpansions, maxDepth, maxFindings),
		"Some evidence, publication, feedback, or recovery paths were not checked.",
		"Simplify the graph or inspect the uncovered paths manually; do not treat silence as a safety proof.", "unknown", "incomplete", 0, "")
}

func (a *analyzer) walk(f frame) {
	if a.bounded {
		return
	}
	if len(f.path) >= maxDepth || a.expansions >= maxExpansions {
		a.bounded = true
		return
	}
	// Path is presentation, not state. The remaining bounded abstract state
	// includes completed stages so a forward edge is not mistaken for a retry.
	key, _ := json.Marshal(struct {
		State, Feedback, LastTask, CycleStart                               string
		Patch, Unknown, CodeSubject, PR, Rebound, CycleChange, CycleUnknown bool
		Pending, Visited                                                    []string
	}{f.state, f.feedback, f.lastTask, f.cycleStart, f.patch, f.unknown, f.codeSubject, f.pr, f.rebound, f.cycleChange, f.cycleUnknown, f.pending, f.visited})
	if a.seen[string(key)] {
		return
	}
	a.seen[string(key)] = true
	a.expansions++
	if f.state == wf.TerminalComplete || wf.IsReservedAnyTarget(f.state) {
		if f.state != wf.TargetJoin {
			a.publication(f, f.state)
		}
		return
	}
	if t, ok := a.m.Task(f.state); ok {
		a.task(f, t)
		return
	}
	if g, ok := a.m.Gate(f.state); ok {
		a.gate(f, g)
		return
	}
	if _, ok := a.m.Parallel(f.state); ok {
		a.add(CoverageCode, f.state, appendPath(f.path, f.state), "parallel evidence and publication joins are not modeled",
			"Cross-branch obligations cannot be verified by this bounded pass.",
			"Inspect branch evidence and join publication explicitly.", "unknown", "incomplete", 0, "")
		// Do not fabricate successful branch results or an arbitrary join order.
	}
}

func (a *analyzer) task(f frame, t apiv1.Task) {
	f.path = appendPath(f.path, t.Name)
	c := a.contracts.Stages[t.Name]
	e := c.apply(CommandEffects(t))
	// An assertion supplies its named effect, not knowledge of every other
	// effect a custom command may have.
	f.unknown = f.unknown || !e.Known || e.ConditionalRebind
	if t.Type == apiv1.TaskDeterministic && !e.Known && !c.assertsEffects() {
		a.add(CoverageCode, t.Name, f.path, "custom command or agent effects are unknown",
			"Evidence production, subject changes and publication by this stage are not proven.",
			"Declare a narrowly scoped safety contract if this stage supplies an obligation, or inspect it manually.", "unknown", "partial", 0, "")
	}
	recovery := c.RecoveryOnNoWork
	if e.NoWork {
		a.publication(f, "no-work -> @complete")
	}
	if e.NoWork && recovery != "" && !slices.Contains(f.visited, recovery) {
		a.add(RecoveryCode, t.Name, appendPath(f.path, "no-work -> @complete"),
			fmt.Sprintf("terminal no-work bypasses the declared recovery obligation %q", recovery),
			"The expected handler never runs on this result, even if it is graph-reachable.",
			"Handle recovery before the selector, or use a nonterminal result for this declared obligation; preserve genuine empty-queue completion.", "high", "modeled", 0, "")
	}
	if f.feedback != "" && t.Type == apiv1.TaskAgentic {
		pointers := apiv1.SelectContextPointers([]apiv1.ContextPointer{{Name: f.feedback + ".verdict"}}, t.ContextFrom)
		if len(pointers) == 0 {
			bound, source := a.budgetFor(f.feedback)
			a.add(FeedbackCode, t.Name, f.path,
				fmt.Sprintf("contextFrom excludes rejection evidence from gate %q", f.feedback),
				"Rework is not delivered the full verdict it is expected to address.",
				fmt.Sprintf("Include %q in contextFrom or retain accumulated context. System learning pointers are not a substitute for the full verdict.", f.feedback),
				"high", "modeled", bound, source)
		}
		f.feedback = ""
	}
	if e.Publishes != "" {
		// continueOnError also admits a path on which publication failed.
		if t.ContinueOnError {
			failed := f
			failed.path = appendPath(f.path, "publication failed (continueOnError)")
			failed.state = t.Next
			a.walk(failed)
		}
		f.pending = remove(f.pending, e.Publishes)
	}
	if e.Parks {
		a.publication(f, t.Name)
	}
	f.recordEvidence(t, e, c)
	f.cycleChange = f.cycleChange || e.Changes
	f.cycleUnknown = f.cycleUnknown || !e.Known
	f.visited = addSorted(f.visited, t.Name)
	f.lastTask, f.state = t.Name, t.Next
	a.walk(f)
}

func (f *frame) recordEvidence(t apiv1.Task, e Effects, c StageContract) {
	if e.SelectsPR {
		f.pr = true
		f.patch, f.rebound = false, false
	}
	if e.Rebinds {
		f.rebound = true
	}
	if e.Changes {
		f.patch = false
	}
	f.codeSubject = f.codeSubject || e.CodeSubject || e.Patch || t.CommitsRepo
	if e.Patch {
		mode := t.EffectiveWorkspace()
		writable := mode == "" || mode == apiv1.WorkspaceRepo
		f.patch = c.Evidence == "patch" || (writable && (!f.pr || f.rebound))
		if !f.patch {
			f.unknown = true
		}
	}
	if !e.Known && !e.Patch && t.Type == apiv1.TaskDeterministic {
		f.patch = false
	}
}

func (a *analyzer) reviewEvidence(f frame, g apiv1.Gate) bool {
	profile := a.contracts.Stages[g.Name].Review
	codeReview := g.Evaluator == apiv1.EvaluatorAgentic && (profile == "code" || profile == "pr" || (profile == "" && (f.codeSubject || f.pr)))
	prReview := codeReview && (profile == "pr" || (profile == "" && f.pr))
	if !codeReview || f.patch {
		return prReview
	}
	mode := g.EffectiveWorkspace()
	implicit := mode == "" || mode == apiv1.WorkspaceRepo
	// The runner's diff is independent of task stdout or reviewer grants,
	// but a selected PR needs a binding to that subject's branch.
	if implicit && (!f.pr || f.rebound) {
		return prReview
	}
	confidence, coverage := "high", "modeled"
	if f.unknown || mode == apiv1.WorkspaceRepoReadOnly {
		confidence, coverage = "uncertain", "partial"
	}
	a.add(EvidenceCode, g.Name, appendPath(f.path, g.Name),
		"no verified patch-evidence route for this code review",
		"The reviewer may judge a changed subject without its usable patch.",
		"Supply a subject-pinned patch artifact or use the runner's writable subject workspace. Read-only implicit evidence depends on workspace pinning.",
		confidence, coverage, 0, "")
	return prReview
}

func (a *analyzer) gate(f frame, g apiv1.Gate) {
	prReview := a.reviewEvidence(f, g)
	if c := a.contracts.Stages[g.Name]; c.Publishes == g.Name {
		f.pending = remove(f.pending, g.Name)
	}
	for _, outcome := range sortedKeys(g.Branches) {
		if outcome == wf.BranchEscalate {
			continue
		}
		target, _ := wf.BranchTarget(g, outcome)
		next := f
		next.path = appendPath(f.path, g.Name+"("+outcome+")")
		next.state = target
		rejection := outcome == string(apiv1.VerdictNeedsChanges) || outcome == string(apiv1.VerdictFail)
		if outcome == string(apiv1.VerdictPass) && g.Evaluator == apiv1.EvaluatorAgentic {
			next.pending = remove(next.pending, g.Name)
		}
		if rejection && g.Evaluator == apiv1.EvaluatorAgentic {
			next.feedback = g.Name
			if prReview && a.contracts.Stages[g.Name].Publishes != g.Name {
				next.pending = addSorted(next.pending, g.Name)
			}
		}
		reentry := slices.Contains(f.visited, target)
		if reentry {
			bound, source := a.budgetFor(g.Name)
			if next.cycleStart == g.Name && !next.cycleChange && !next.cycleUnknown && rejection {
				a.add(CycleCode, g.Name, next.path, "a rejection cycle repeats without a recognized subject-changing effect",
					"Rechecking unchanged work can spend the policy budget without addressing the rejection.",
					"Route rejection through real rework and deliver its feedback; do not merely increase the retry limit.", "high", "modeled", bound, source)
			}
			next.cycleStart, next.cycleChange, next.cycleUnknown = g.Name, false, false
			// Ask the actual budget helper for the exhaustion transition.
			// We analyze the boundary without unrolling a potentially huge
			// configured number of retries or inventing a second loop policy.
			b := runcontrol.RepassBudget{RepassAttempts: map[string]int{target: bound},
				InfrastructureRepassAttempts: map[string]int{target: runcontrol.DefaultMaxInfrastructureRepasses}}
			charge := b.Charge(g, outcome, target, true, bound)
			if charge.Exceeded {
				exhausted := next
				exhausted.state = wf.TargetEscalate
				if escalation, ok := wf.BranchTarget(g, wf.BranchEscalate); ok {
					exhausted.state = escalation
				}
				exhausted.path = appendPath(next.path, fmt.Sprintf("budget-exhausted(%d %s repasses)", charge.Bound, budgetClass(charge.Infrastructure)))
				a.walk(exhausted)
			}
		}
		a.walk(next)
	}
}

func (a *analyzer) publication(f frame, stage string) {
	for _, rejected := range f.pending {
		confidence, coverage := "high", "modeled"
		if f.unknown {
			confidence, coverage = "uncertain", "partial"
		}
		a.add(PublishCode, rejected, appendPath(f.path, stage),
			fmt.Sprintf("rejected findings from gate %q can terminate or park without a recognized publisher", rejected),
			"The PR can remain parked without the findings needed to repair it.",
			fmt.Sprintf("Route this path through goobers apply-verdict --gate %s, or declare a publisher for that verdict.", rejected),
			confidence, coverage, 0, "")
	}
}

func (a *analyzer) budgetFor(name string) (int, string) {
	g, _ := a.m.Gate(name)
	effective, err := runcontrol.Resolve(apiv1.RunControls{}, a.opts.GaggleRunControls, a.m.Def.Spec.RunControls)
	if err != nil {
		// A compiled machine normally precludes this. Keep unsupported
		// inheritance visible to callers that construct one themselves.
		a.add(CoverageCode, name, nil, err.Error(), "The retry bound cannot be established.",
			"Correct the run-control declaration.", "unknown", "incomplete", 0, "")
	}
	source := "default; instance override unavailable to config-tree analysis"
	if a.opts.GaggleRunControls != nil && a.opts.GaggleRunControls.MaxRepasses > 0 {
		source = "gaggle"
	}
	if a.m.Def.Spec.RunControls != nil && a.m.Def.Spec.RunControls.MaxRepasses > 0 {
		source = "workflow"
	}
	if g.MaxRepasses > 0 {
		source = "gate"
	}
	return runcontrol.MaxRepassesForGate(g, effective.MaxRepasses), source
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func appendPath(path []string, hop string) []string {
	return append(slices.Clone(path), hop)
}

func addSorted(items []string, item string) []string {
	if slices.Contains(items, item) {
		return items
	}
	out := append(slices.Clone(items), item)
	sort.Strings(out)
	return out
}

func remove(items []string, item string) []string {
	return slices.DeleteFunc(slices.Clone(items), func(s string) bool { return s == item })
}

func budgetClass(infrastructure bool) string {
	if infrastructure {
		return "infrastructure"
	}
	return "policy"
}
