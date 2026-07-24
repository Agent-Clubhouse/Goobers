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

	m, err := compileAcknowledged(
		Definition{Name: w.Name, Version: 1, Spec: w.Spec},
		WithGoobers(goobers),
		WithKnownChecks([]string{"output-equals", "status-equals"}))

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
	if got := siblings.Next; got != "gather-review-threads" {
		t.Errorf("gather-sibling-context next = %q, want gather-review-threads", got)
	}
	if len(siblings.PolicyActions) != 1 || siblings.PolicyActions[0] != "flag-scope-drift" {
		t.Errorf("gather-sibling-context policyActions = %v, want [flag-scope-drift]", siblings.PolicyActions)
	}

	threads, ok := m.Task("gather-review-threads")
	if !ok {
		t.Fatal("gather-review-threads stage not found")
	}
	if threads.Run == nil ||
		len(threads.Run.Command) != 2 ||
		threads.Run.Command[0] != "goobers" ||
		threads.Run.Command[1] != "gather-review-threads" {
		t.Errorf("gather-review-threads command = %v, want [goobers gather-review-threads]", threads.Run)
	}
	if threads.Run != nil && threads.Run.Workspace != apiv1.WorkspaceScratch {
		t.Errorf("gather-review-threads workspace = %q, want scratch", threads.Run.Workspace)
	}
	if threads.Inputs["resultFile"] != "remediation-brief.json" {
		t.Errorf("gather-review-threads resultFile = %q, want remediation-brief.json", threads.Inputs["resultFile"])
	}
	if len(threads.Capabilities) != 1 || threads.Capabilities[0] != "github:pr:write" {
		t.Errorf("gather-review-threads capabilities = %v, want [github:pr:write]", threads.Capabilities)
	}
	if threads.Next != "gather-issue-context" {
		t.Errorf("gather-review-threads next = %q, want gather-issue-context", threads.Next)
	}

	issues, ok := m.Task("gather-issue-context")
	if !ok {
		t.Fatal("gather-issue-context stage not found")
	}
	if issues.Run == nil ||
		len(issues.Run.Command) != 2 ||
		issues.Run.Command[0] != "goobers" ||
		issues.Run.Command[1] != "gather-issue-context" {
		t.Errorf("gather-issue-context command = %v, want [goobers gather-issue-context]", issues.Run)
	}
	if issues.Run != nil && issues.Run.Workspace != apiv1.WorkspaceScratch {
		t.Errorf("gather-issue-context workspace = %q, want scratch", issues.Run.Workspace)
	}
	if issues.Inputs["resultFile"] != "remediation-brief.json" {
		t.Errorf("gather-issue-context resultFile = %q, want remediation-brief.json", issues.Inputs["resultFile"])
	}
	if len(issues.Capabilities) != 2 ||
		issues.Capabilities[0] != "github:pr:write" ||
		issues.Capabilities[1] != "github:issues:write" {
		t.Errorf("gather-issue-context capabilities = %v, want [github:pr:write github:issues:write]", issues.Capabilities)
	}
	if issues.Next != "implement" {
		t.Errorf("gather-issue-context next = %q, want implement", issues.Next)
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
	if got := implement.Next; got != "validate-finding-responses" {
		t.Errorf("implement next = %q, want pre-publication finding response validation", got)
	}
	if !containsString(implement.ExpectedOutputs, "findingResponses") {
		t.Errorf("implement expectedOutputs = %v, missing findingResponses account", implement.ExpectedOutputs)
	}

	validateResponses, ok := m.Task("validate-finding-responses")
	if !ok {
		t.Fatal("validate-finding-responses stage not found")
	}
	if validateResponses.Run == nil ||
		len(validateResponses.Run.Command) != 3 ||
		validateResponses.Run.Command[0] != "goobers" ||
		validateResponses.Run.Command[1] != "respond-to-findings" ||
		validateResponses.Run.Command[2] != "--check" {
		t.Errorf("validate-finding-responses command = %v, want [goobers respond-to-findings --check]", validateResponses.Run)
	}
	if validateResponses.Run != nil && validateResponses.Run.Workspace != apiv1.WorkspaceScratch {
		t.Errorf("validate-finding-responses workspace = %q, want scratch", validateResponses.Run.Workspace)
	}
	if validateResponses.Inputs["resultFile"] != "finding-response-validation.json" {
		t.Errorf("validate-finding-responses resultFile = %q, want finding-response-validation.json", validateResponses.Inputs["resultFile"])
	}
	if len(validateResponses.Capabilities) != 1 || validateResponses.Capabilities[0] != "github:issues:write" {
		t.Errorf("validate-finding-responses capabilities = %v, want [github:issues:write]", validateResponses.Capabilities)
	}
	if len(validateResponses.PolicyActions) != 1 || validateResponses.PolicyActions[0] != "respond-to-findings" {
		t.Errorf("validate-finding-responses policyActions = %v, want [respond-to-findings]", validateResponses.PolicyActions)
	}
	if validateResponses.Next != "finding-responses-gate" {
		t.Errorf("validate-finding-responses next = %q, want finding-responses-gate", validateResponses.Next)
	}
	responseGate, ok := m.Gate("finding-responses-gate")
	if !ok {
		t.Fatal("finding-responses-gate not found")
	}
	if responseGate.Evaluator != apiv1.EvaluatorAutomated ||
		responseGate.Automated == nil ||
		responseGate.Automated.Check != "status-equals" {
		t.Errorf("finding-responses-gate evaluator = %+v, want automated status-equals", responseGate)
	}
	if responseGate.Branches["pass"] != "review" ||
		responseGate.Branches["fail"] != "implement" ||
		responseGate.Branches["escalate"] != "park-invalid-finding-responses" {
		t.Errorf("finding-responses-gate branches = %v, want pass->review, fail->implement, and escalate->park-invalid-finding-responses", responseGate.Branches)
	}
	invalidResponsesPark, ok := m.Task("park-invalid-finding-responses")
	if !ok {
		t.Fatal("park-invalid-finding-responses not found")
	}
	if invalidResponsesPark.Next != TargetEscalate {
		t.Errorf("park-invalid-finding-responses next = %q, want %q", invalidResponsesPark.Next, TargetEscalate)
	}
	if invalidResponsesPark.Run == nil ||
		len(invalidResponsesPark.Run.Command) != 4 ||
		invalidResponsesPark.Run.Command[0] != "goobers" ||
		invalidResponsesPark.Run.Command[1] != "remediation-checkpoint" ||
		invalidResponsesPark.Run.Command[2] != "--escalate" {
		t.Errorf("park-invalid-finding-responses command = %v, want goobers remediation-checkpoint --escalate <reason>", invalidResponsesPark.Run)
	}
	if len(invalidResponsesPark.PolicyActions) != 2 ||
		invalidResponsesPark.PolicyActions[0] != "record-remediation-checkpoint" ||
		invalidResponsesPark.PolicyActions[1] != "escalate-pr" {
		t.Errorf(
			"park-invalid-finding-responses policyActions = %v, want [record-remediation-checkpoint escalate-pr]",
			invalidResponsesPark.PolicyActions,
		)
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
	if gather.Next != "gather-ci-failures" {
		t.Fatalf("gather-pr-context next = %q, want gather-ci-failures", gather.Next)
	}

	gatherCI, ok := m.Task("gather-ci-failures")
	if !ok {
		t.Fatal("gather-ci-failures not found")
	}
	if gatherCI.Run == nil ||
		len(gatherCI.Run.Command) != 2 ||
		gatherCI.Run.Command[0] != "goobers" ||
		gatherCI.Run.Command[1] != "gather-ci-failures" {
		t.Fatalf("gather-ci-failures command = %v, want [goobers gather-ci-failures]", gatherCI.Run)
	}
	if gatherCI.Run.Workspace != apiv1.WorkspaceScratch {
		t.Errorf("gather-ci-failures workspace = %q, want scratch", gatherCI.Run.Workspace)
	}
	if gatherCI.Inputs["resultFile"] != "remediation-brief.json" {
		t.Errorf("gather-ci-failures resultFile = %q, want remediation-brief.json", gatherCI.Inputs["resultFile"])
	}
	if gatherCI.Next != "rebase-pr" {
		t.Errorf("gather-ci-failures next = %q, want rebase-pr", gatherCI.Next)
	}
	if len(gatherCI.Capabilities) != 1 || gatherCI.Capabilities[0] != "github:pr:write" {
		t.Errorf("gather-ci-failures capabilities = %v, want [github:pr:write]", gatherCI.Capabilities)
	}
	for _, output := range []string{
		"selectedNumber", "head", "base", "isBehindBase",
		"hasSubstantiveFindings", "hasFailingCI", "workspaceBranch",
	} {
		if !containsString(gatherCI.ExpectedOutputs, output) {
			t.Errorf("gather-ci-failures expectedOutputs = %v, missing %q", gatherCI.ExpectedOutputs, output)
		}
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
	if !strings.Contains(implement.Goal, "gather-ci-failures remediation-brief.json") {
		t.Fatalf("implement goal does not direct the agent to the enriched brief: %q", implement.Goal)
	}
	if !strings.Contains(implement.Goal, "resolved/outdated state") {
		t.Fatalf("implement goal does not direct the agent to review-thread liveness: %q", implement.Goal)
	}
	if !strings.Contains(implement.Goal, "originating issue body") {
		t.Fatalf("implement goal does not direct the agent to originating issue context: %q", implement.Goal)
	}
}

func TestPRRemediationImplementerRequiresCompleteFindingAccount(t *testing.T) {
	path := filepath.Join(
		"..", "..", "selfhost", "gaggles", "goobers", "goobers", "implementer", "instructions.md",
	)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read implementer instructions: %v", err)
	}
	instructions := strings.Join(strings.Fields(string(raw)), " ")
	for _, required := range []string{
		"`pr-remediation` workflow invokes",
		"original merge-review verdict remains the authoritative checklist",
		"all integers from 1 through `N` exactly once",
		"Mechanically decode the finished scalar",
		"never return only the latest reviewer finding",
	} {
		if !strings.Contains(instructions, required) {
			t.Errorf("implementer instructions missing remediation contract %q", required)
		}
	}
}

// TestPRRemediationPublishesAndResponds pins the cycle's terminal steps.
// Without a publish stage the agentic chain's work stays local to the run's
// worktree and is discarded at teardown — the run would report success having
// changed nothing on the PR, the most expensive possible no-op.
func TestPRRemediationPublishesAndResponds(t *testing.T) {
	_, m := loadPRRemediation(t)

	push, ok := m.Task("push-remediated")
	if !ok {
		t.Fatal("push-remediated not found — the remediation would never reach the PR")
	}
	if push.Next != "respond-to-findings" {
		t.Errorf("push-remediated next = %q, want respond-to-findings after the branch is published", push.Next)
	}
	wantCaps := map[string]bool{"repo:push": false, "github:pr:write": false, "github:issues:write": false}
	for _, c := range push.Capabilities {
		if _, ok := wantCaps[c]; ok {
			wantCaps[c] = true
		}
	}

	respond, ok := m.Task("respond-to-findings")
	if !ok {
		t.Fatal("respond-to-findings not found — the published remediation would remain silent")
	}
	if respond.Next != "" {
		t.Errorf("respond-to-findings next = %q, want terminal", respond.Next)
	}
	if respond.Run == nil {
		t.Fatal("respond-to-findings has no deterministic run command")
	}
	if len(respond.Run.Command) != 2 ||
		respond.Run.Command[0] != "goobers" || respond.Run.Command[1] != "respond-to-findings" {
		t.Errorf("respond-to-findings command = %v, want [goobers respond-to-findings]", respond.Run.Command)
	}
	if respond.Run.Workspace != apiv1.WorkspaceScratch {
		t.Errorf("respond-to-findings workspace = %q, want scratch: it reads declared journal inputs, not repository state", respond.Run.Workspace)
	}
	if len(respond.Capabilities) != 1 || respond.Capabilities[0] != "github:issues:write" {
		t.Errorf("respond-to-findings capabilities = %v, want only github:issues:write", respond.Capabilities)
	}
	if respond.Inputs["resultFile"] != "remediation-response.json" {
		t.Errorf("respond-to-findings resultFile = %q, want durable remediation-response.json", respond.Inputs["resultFile"])
	}
	if len(respond.InputsFrom) != 0 {
		t.Errorf("respond-to-findings inputsFrom = %v, want none so omitting the stage only removes legibility", respond.InputsFrom)
	}
	if !containsString(respond.ExpectedOutputs, "posted") {
		t.Errorf("respond-to-findings outputs = %v, missing posted status", respond.ExpectedOutputs)
	}
	for c, granted := range wantCaps {
		if !granted {
			t.Errorf("push-remediated is missing capability %q", c)
		}
	}
	if push.Inputs["resultFile"] != "push-remediated-result.json" ||
		!containsString(push.ExpectedOutputs, "published") {
		t.Errorf("push-remediated result contract = inputs %v outputs %v, want durable published status", push.Inputs, push.ExpectedOutputs)
	}
	if push.Retry == nil || push.Retry.MaxAttempts != 1 || push.Retry.BackoffSeconds != 120 {
		t.Errorf("push-remediated retry = %+v, want fail-fast policy attempts with a 120s infrastructure backoff", push.Retry)
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

	rebase, ok := m.Task("rebase-pr")
	if !ok {
		t.Fatal("rebase-pr not found")
	}
	checkpoint, ok := m.Task("remediation-checkpoint")
	if !ok {
		t.Fatal("remediation-checkpoint not found")
	}
	for _, output := range []string{"conflict", "conflictLocations", "attemptedHeadSha", "rebaseBaseSha"} {
		if !containsString(rebase.ExpectedOutputs, output) {
			t.Errorf("rebase-pr expectedOutputs = %v, missing %q structural-collision evidence", rebase.ExpectedOutputs, output)
		}
		if checkpoint.InputsFrom[output] != output {
			t.Errorf("remediation-checkpoint inputsFrom[%q] = %q, want %q", output, checkpoint.InputsFrom[output], output)
		}
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
