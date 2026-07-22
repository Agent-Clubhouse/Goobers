package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// loadPRRemediation reads and compiles the REAL shipped pr-remediation
// definition against the REAL implementer/reviewer goobers, the same
// divergence-guard approach TestSelfhostWorkflowsCompile takes (#124): a
// synthetic fixture would happily keep passing while the definition the
// dogfood instance actually runs drifted.
func loadPRRemediation(t *testing.T) (apiv1.Workflow, *Machine) {
	t.Helper()
	root := filepath.Join("..", "..", "selfhost", "gaggles", "goobers")

	raw, err := os.ReadFile(filepath.Join(root, "workflows", "pr-remediation.yaml"))
	if err != nil {
		t.Fatalf("read pr-remediation.yaml: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal pr-remediation.yaml: %v", err)
	}

	goobers := map[string]apiv1.GooberSpec{}
	for _, name := range []string{"implementer", "reviewer"} {
		var g apiv1.Goober
		graw, err := os.ReadFile(filepath.Join(root, "goobers", name, "goober.yaml"))
		if err != nil {
			t.Fatalf("read %s goober: %v", name, err)
		}
		if err := yaml.Unmarshal(graw, &g); err != nil {
			t.Fatalf("unmarshal %s goober: %v", name, err)
		}
		registered := false
		for _, wf := range g.Spec.Workflows {
			if wf == "pr-remediation" {
				registered = true
				break
			}
		}
		if !registered {
			t.Errorf("%s is not registered for pr-remediation — the agentic chain cannot dispatch it", name)
		}
		goobers[g.Name] = g.Spec
	}

	m, err := Compile(
		Definition{Name: w.Name, Version: 1, Spec: w.Spec},
		WithGoobers(goobers),
		WithKnownChecks([]string{"output-equals", "status-equals"}),
	)
	if err != nil {
		t.Fatalf("compile pr-remediation against selfhost's real goobers: %v", err)
	}
	return w, m
}

// TestPRRemediationWiresTheAgenticChain is issue #392's regression guard. The
// workflow shipped for months with rebase-gate's "fail" branch dead-ending at
// a checkpoint that could only escalate, which meant every PR merge-review did
// not pass became a permanently human-blocked open PR (#892, closed as a
// duplicate of #392). Unit tests over the individual stages all passed the
// whole time — nothing asserted the GRAPH, which is where the capability was
// missing. This asserts the graph.
func TestPRRemediationWiresTheAgenticChain(t *testing.T) {
	_, m := loadPRRemediation(t)

	updateGate, ok := m.Gate("update-behind-gate")
	if !ok {
		t.Fatal("update-behind-gate not found")
	}
	if got := updateGate.Branches["pass"]; got != "" {
		t.Errorf("update-behind-gate pass -> %q, want terminal", got)
	}
	if got := updateGate.Branches["fail"]; got != "gather-pr-context" {
		t.Errorf("update-behind-gate fail -> %q, want gather-pr-context", got)
	}

	// The routing spine: a PR that needs the agent must actually reach it.
	rebaseGate, ok := m.Gate("rebase-gate")
	if !ok {
		t.Fatal("rebase-gate not found")
	}
	if got := rebaseGate.Branches["fail"]; got != "remediation-checkpoint" {
		t.Errorf("rebase-gate fail -> %q, want remediation-checkpoint", got)
	}

	checkpointGate, ok := m.Gate("checkpoint-gate")
	if !ok {
		t.Fatal("checkpoint-gate not found — loop control cannot route into the agentic chain")
	}
	if got := checkpointGate.Branches["pass"]; got != "gather-sibling-context" {
		t.Errorf("checkpoint-gate pass -> %q, want gather-sibling-context", got)
	}
	if got, ok := checkpointGate.Branches["fail"]; !ok || got != "" {
		t.Errorf("checkpoint-gate fail -> %q, want terminal: an escalated PR must stop, not loop", got)
	}

	siblings, ok := m.Task("gather-sibling-context")
	if !ok {
		t.Fatal("gather-sibling-context stage not found")
	}
	if got := siblings.InputsFrom["selectedNumber"]; got != "selectedNumber" {
		t.Errorf("gather-sibling-context selectedNumber input = %q, want checkpoint's selectedNumber output", got)
	}
	if got := siblings.Inputs["resultFile"]; got != "sibling-context.json" {
		t.Errorf("gather-sibling-context resultFile = %q, want sibling-context.json", got)
	}
	if got := siblings.Next; got != "implement" {
		t.Errorf("gather-sibling-context next = %q, want implement", got)
	}

	implement, ok := m.Task("implement")
	if !ok {
		t.Fatal("implement stage not found")
	}
	if implement.Type != apiv1.TaskAgentic {
		t.Errorf("implement type = %q, want agentic", implement.Type)
	}
	if implement.Goober != "implementer" {
		t.Errorf("implement goober = %q, want the shared implementer", implement.Goober)
	}
	if got := implement.Next; got != "review" {
		t.Errorf("implement next = %q, want the review gate", got)
	}

	// The full executor chain, exactly as implementation.yaml shapes it:
	// review -> local-ci -> local-gate -> publish.
	review, ok := m.Gate("review")
	if !ok {
		t.Fatal("review gate not found")
	}
	if review.Evaluator != apiv1.EvaluatorAgentic {
		t.Errorf("review evaluator = %q, want agentic", review.Evaluator)
	}
	for branch, want := range map[string]string{
		"pass":          "local-ci",
		"needs-changes": "implement",
		"fail":          "park-escalated",
		"escalate":      "park-escalated",
	} {
		if got := review.Branches[branch]; got != want {
			t.Errorf("review %s -> %q, want %q", branch, got, want)
		}
	}

	localGate, ok := m.Gate("local-gate")
	if !ok {
		t.Fatal("local-gate not found")
	}
	for branch, want := range map[string]string{
		"pass": "push-remediated",
		"fail": "implement",
	} {
		if got := localGate.Branches[branch]; got != want {
			t.Errorf("local-gate %s -> %q, want %q", branch, got, want)
		}
	}

	// A reviewer "fail" verdict must terminate ESCALATED, not merely abort
	// (design doc §4 D2, and the same rationale implementation.yaml's own
	// park-escalated documents: every escalation surface keys on the phase).
	park, ok := m.Task("park-escalated")
	if !ok {
		t.Fatal("park-escalated not found")
	}
	if park.Next != TargetEscalate {
		t.Errorf("park-escalated next = %q, want %q", park.Next, TargetEscalate)
	}
}

// TestPRRemediationRebindsTheWorkspaceBranch guards the seam the whole chain
// silently depends on. If gather-pr-context stops declaring workspaceBranch,
// nothing fails loudly: implement/review/local-ci would each be provisioned on
// a pristine branch cut from main, the reviewer would judge an empty diff, and
// the run would "succeed" having remediated nothing.
func TestPRRemediationRebindsTheWorkspaceBranch(t *testing.T) {
	_, m := loadPRRemediation(t)

	gather, ok := m.Task("gather-pr-context")
	if !ok {
		t.Fatal("gather-pr-context not found")
	}
	found := false
	for _, out := range gather.ExpectedOutputs {
		// Mirrors internal/runner.WorkspaceBranchOutput. Spelled literally
		// rather than imported: internal/runner imports internal/workflow, so
		// the reverse would be an import cycle — and a literal is the right
		// assertion for a cross-package wire contract anyway.
		if out == "workspaceBranch" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("gather-pr-context expectedOutputs = %v, missing workspaceBranch — "+
			"every later stage would be provisioned on a fresh branch off main instead of the PR's", gather.ExpectedOutputs)
	}
}

func TestPRRemediationHandsTheVersionedBriefToImplement(t *testing.T) {
	_, m := loadPRRemediation(t)

	gather, ok := m.Task("gather-pr-context")
	if !ok {
		t.Fatal("gather-pr-context not found")
	}
	if got := gather.Inputs["resultFile"]; got != "remediation-brief.json" {
		t.Fatalf("gather-pr-context resultFile = %q, want remediation-brief.json", got)
	}

	rebase, ok := m.Task("rebase-pr")
	if !ok {
		t.Fatal("rebase-pr not found")
	}
	wantRouting := map[string]string{
		"selectedNumber":         "selectedNumber",
		"head":                   "head",
		"base":                   "base",
		"hasSubstantiveFindings": "hasSubstantiveFindings",
		"hasFailingCI":           "hasFailingCI",
	}
	if len(rebase.InputsFrom) != len(wantRouting) {
		t.Fatalf("rebase-pr inputsFrom = %v, want routing-only subset %v", rebase.InputsFrom, wantRouting)
	}
	for key, want := range wantRouting {
		if got := rebase.InputsFrom[key]; got != want {
			t.Errorf("rebase-pr inputsFrom[%q] = %q, want %q", key, got, want)
		}
	}

	implement, ok := m.Task("implement")
	if !ok {
		t.Fatal("implement not found")
	}
	if !strings.Contains(implement.Goal, "remediation-brief.json") {
		t.Fatalf("implement goal does not direct the agent to the brief: %q", implement.Goal)
	}
}

// TestPRRemediationPublishesAndClearsTheLabel pins the cycle's terminal step.
// Without a publish stage the agentic chain's work stays local to the run's
// worktree and is discarded at teardown — the run would report success having
// changed nothing on the PR, the most expensive possible no-op.
func TestPRRemediationPublishesAndClearsTheLabel(t *testing.T) {
	_, m := loadPRRemediation(t)

	push, ok := m.Task("push-remediated")
	if !ok {
		t.Fatal("push-remediated not found — the remediation would never reach the PR")
	}
	if push.Next != "" {
		t.Errorf("push-remediated next = %q, want terminal", push.Next)
	}
	wantCaps := map[string]bool{"repo:push": false, "github:pr:write": false, "github:issues:write": false}
	for _, c := range push.Capabilities {
		if _, ok := wantCaps[c]; ok {
			wantCaps[c] = true
		}
	}
	for c, granted := range wantCaps {
		if !granted {
			t.Errorf("push-remediated is missing capability %q", c)
		}
	}

	// pr-remediation is the ONLY workflow that pushes to existing PR
	// branches, and it must never gain the merge capability (design doc §2's
	// capability-isolation rationale — that is why decider and executor are
	// separate workflows at all).
	for _, task := range m.Def.Spec.Tasks {
		for _, c := range task.Capabilities {
			if c == "github:pr:merge" {
				t.Errorf("stage %q declares github:pr:merge; only merge-review may hold it", task.Name)
			}
		}
	}
}

// TestPRRemediationCheckpointEchoesPushContext covers the non-obvious data-flow
// constraint #392 had to design around: Task.InputsFrom resolves against the
// immediately preceding TASK's outputs, and implement/local-ci each become
// that upstream in turn. Anything push-remediated needs must therefore be
// re-emitted by remediation-checkpoint (or re-derived), never assumed to flow
// through from gather-pr-context.
func TestPRRemediationCheckpointEchoesPushContext(t *testing.T) {
	_, m := loadPRRemediation(t)

	checkpoint, ok := m.Task("remediation-checkpoint")
	if !ok {
		t.Fatal("remediation-checkpoint not found")
	}
	declared := map[string]bool{}
	for _, out := range checkpoint.ExpectedOutputs {
		declared[out] = true
	}
	for _, want := range []string{"continueRemediation", "selectedNumber", "head", "headSha"} {
		if !declared[want] {
			t.Errorf("remediation-checkpoint expectedOutputs = %v, missing %q", checkpoint.ExpectedOutputs, want)
		}
	}
	if checkpoint.Next != "checkpoint-gate" {
		t.Errorf("remediation-checkpoint next = %q, want checkpoint-gate — a terminal checkpoint is exactly the #892 dead end", checkpoint.Next)
	}
}
