package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// loadPRRemediation reads and compiles the REAL shipped pr-remediation
// definition against the REAL implementer/reviewer goobers, the same
// divergence-guard approach TestReferenceWorkflowsCompile takes (#124): a
// synthetic fixture would happily keep passing while the definition the
// dogfood instance actually runs drifted.
func loadPRRemediation(t *testing.T) (apiv1.Workflow, *Machine) {
	t.Helper()
	root := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers")

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
		WithKnownChecks([]string{"failure-class", "output-equals", "status-equals"}))

	if err != nil {
		t.Fatalf("compile pr-remediation against the real reference workflows' goobers: %v", err)
	}
	return w, m
}

func TestPRRemediationDeclaresWorkDrivenPolling(t *testing.T) {
	w, _ := loadPRRemediation(t)
	for _, trigger := range w.Spec.Triggers {
		if trigger.Type == apiv1.TriggerSchedule && trigger.Priority == 100 {
			return
		}
	}
	t.Fatal("pr-remediation has no high-priority schedule trigger for eligibility-driven fan-out")
}

func TestPRRemediationThreadsUpdateSelectionIntoFullRemediation(t *testing.T) {
	w, _ := loadPRRemediation(t)
	for _, task := range w.Spec.Tasks {
		if task.Name != "gather-pr-context" {
			continue
		}
		if got := task.InputsFrom["selectedNumber"]; got != "selectedNumber" {
			t.Fatalf("gather-pr-context selectedNumber input = %q, want update-behind-pr selectedNumber", got)
		}
		if !reflect.DeepEqual(task.Capabilities, []string{"github:pr:write", "repo:push"}) {
			t.Fatalf("gather-pr-context capabilities = %v, want [github:pr:write repo:push]", task.Capabilities)
		}
		return
	}
	t.Fatal("gather-pr-context task not found")
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
	if got := updateGate.Branches["pass"]; got != "release-claim" {
		t.Errorf("update-behind-gate pass -> %q, want release-claim", got)
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
	if got := checkpointGate.Branches["pass"]; got != "checkpoint-continue-gate" {
		t.Errorf("checkpoint-gate pass -> %q, want checkpoint-continue-gate", got)
	}
	if checkpointGate.Automated == nil || checkpointGate.Automated.Params["key"] != "escalationOutcome" {
		t.Errorf("checkpoint-gate input = %+v, want escalationOutcome", checkpointGate.Automated)
	}
	// #1860: this escalation path must release the run's PR claim like every
	// other terminal/escalate path in this workflow, not reach @escalate
	// directly — a policy-excluded/budget-exhausted escalation here would
	// otherwise strand the claim the same way the pre-#1860 workflow did.
	if got := checkpointGate.Branches["fail"]; got != "release-escalated-claim" {
		t.Errorf("checkpoint-gate fail -> %q, want release-escalated-claim", got)
	}
	continueGate, ok := m.Gate("checkpoint-continue-gate")
	if !ok {
		t.Fatal("checkpoint-continue-gate not found")
	}
	if continueGate.Automated == nil || continueGate.Automated.Params["key"] != "continueRemediation" {
		t.Errorf("checkpoint-continue-gate input = %+v, want continueRemediation", continueGate.Automated)
	}
	// checkpoint-continue-gate is the original checkpoint-gate (#1860), so it
	// keeps #1860's claim-guard routing rather than the pre-#1860
	// gather-review-threads/"" destinations.
	if got, ok := continueGate.Branches["pass"]; !ok || got != "guard-before-agent-context" {
		t.Errorf("checkpoint-continue-gate pass -> %q, want guard-before-agent-context", got)
	}
	if got, ok := continueGate.Branches["fail"]; !ok || got != "release-claim" {
		t.Errorf("checkpoint-continue-gate fail -> %q, want release-claim: an escalated PR must stop, not loop", got)
	}

	siblings, ok := m.Task("gather-sibling-context")
	if !ok {
		t.Fatal("gather-sibling-context stage not found")
	}
	wantSiblingRouting := map[string]string{
		"selectedNumber":         "selectedNumber",
		"head":                   "head",
		"base":                   "base",
		"hasSubstantiveFindings": "hasSubstantiveFindings",
		"hasFailingCI":           "hasFailingCI",
	}
	for key, want := range wantSiblingRouting {
		if got := siblings.InputsFrom[key]; got != want {
			t.Errorf("gather-sibling-context inputsFrom[%q] = %q, want %q", key, got, want)
		}
	}
	if got := siblings.Inputs["resultFile"]; got != "sibling-context.json" {
		t.Errorf("gather-sibling-context resultFile = %q, want sibling-context.json", got)
	}
	if got := siblings.Inputs["minSeverity"]; got != "info" {
		t.Errorf("gather-sibling-context minSeverity = %q, want info", got)
	}
	if got := siblings.Next; got != "rebase-pr" {
		t.Errorf("gather-sibling-context next = %q, want rebase-pr", got)
	}
	if want := []string{"flag-scope-drift", "route-verdict"}; !reflect.DeepEqual(siblings.PolicyActions, want) {
		t.Errorf("gather-sibling-context policyActions = %v, want %v", siblings.PolicyActions, want)
	}
	for _, output := range []string{
		"selectedNumber", "head", "base", "hasSubstantiveFindings",
		"hasFailingCI", "hasSiblingOverlap",
	} {
		if !containsString(siblings.ExpectedOutputs, output) {
			t.Errorf("gather-sibling-context expectedOutputs = %v, missing %q", siblings.ExpectedOutputs, output)
		}
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
	if !reflect.DeepEqual(issues.Capabilities, []string{"github:pr:write", "github:issues:read"}) {
		t.Errorf("gather-issue-context capabilities = %v, want [github:pr:write github:issues:read]", issues.Capabilities)
	}
	if issues.Next != "guard-before-implement" {
		t.Errorf("gather-issue-context next = %q, want guard-before-implement", issues.Next)
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
	if !containsString(implement.ExpectedOutputs, "threadResponses") {
		t.Errorf("implement expectedOutputs = %v, missing threadResponses account", implement.ExpectedOutputs)
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
	if len(validateResponses.Capabilities) != 0 {
		t.Errorf("validate-finding-responses capabilities = %v, want none for check-only validation", validateResponses.Capabilities)
	}
	if len(validateResponses.PolicyActions) != 0 {
		t.Errorf("validate-finding-responses policyActions = %v, want none for check-only validation", validateResponses.PolicyActions)
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
	if responseGate.Branches["pass"] != "guard-before-review" ||
		responseGate.Branches["fail"] != "guard-before-implement" ||
		responseGate.Branches["escalate"] != "park-invalid-finding-responses" {
		t.Errorf("finding-responses-gate branches = %v, want pass->guard-before-review, fail->guard-before-implement, and escalate->park-invalid-finding-responses", responseGate.Branches)
	}
	invalidResponsesPark, ok := m.Task("park-invalid-finding-responses")
	if !ok {
		t.Fatal("park-invalid-finding-responses not found")
	}
	if invalidResponsesPark.Next != "release-escalated-claim" {
		t.Errorf("park-invalid-finding-responses next = %q, want release-escalated-claim", invalidResponsesPark.Next)
	}
	if invalidResponsesPark.Run == nil ||
		len(invalidResponsesPark.Run.Command) != 6 ||
		invalidResponsesPark.Run.Command[0] != "goobers" ||
		invalidResponsesPark.Run.Command[1] != "remediation-checkpoint" ||
		invalidResponsesPark.Run.Command[2] != "--escalate" ||
		invalidResponsesPark.Run.Command[4] != "--escalation-outcome" ||
		invalidResponsesPark.Run.Command[5] != "budget-exhausted" {
		t.Errorf("park-invalid-finding-responses command = %v, want forced budget-exhausted checkpoint", invalidResponsesPark.Run)
	}
	if got := invalidResponsesPark.Inputs["resultFile"]; got != "finding-response-escalation-result.json" {
		t.Errorf("park-invalid-finding-responses resultFile = %q, want finding-response-escalation-result.json", got)
	}
	if len(invalidResponsesPark.PolicyActions) != 2 ||
		invalidResponsesPark.PolicyActions[0] != "record-remediation-checkpoint" ||
		invalidResponsesPark.PolicyActions[1] != "escalate-pr" {
		t.Errorf(
			"park-invalid-finding-responses policyActions = %v, want [record-remediation-checkpoint escalate-pr]",
			invalidResponsesPark.PolicyActions,
		)
	}
	for _, output := range []string{"escalationOutcome", "remediationAttempted", "attemptedCauses", "escalationReason"} {
		if !containsString(invalidResponsesPark.ExpectedOutputs, output) {
			t.Errorf("park-invalid-finding-responses expectedOutputs = %v, missing %q", invalidResponsesPark.ExpectedOutputs, output)
		}
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
		"pass":          "guard-before-local-ci",
		"needs-changes": "guard-before-implement",
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
		"pass":  "guard-before-push",
		"fail":  "guard-before-implement",
		"infra": "park-infrastructure-failure",
	} {
		if got := localGate.Branches[branch]; got != want {
			t.Errorf("local-gate %s -> %q, want %q", branch, got, want)
		}
	}
	if localGate.Automated == nil || localGate.Automated.Check != "failure-class" {
		t.Errorf("local-gate automated check = %+v, want failure-class", localGate.Automated)
	}
	infraPark, ok := m.Task("park-infrastructure-failure")
	if !ok {
		t.Fatal("park-infrastructure-failure not found")
	}
	if infraPark.Next != "release-escalated-claim" ||
		infraPark.Run == nil ||
		!containsString(infraPark.Run.Command, "infrastructure-failure") {
		t.Errorf("park-infrastructure-failure = %+v, want explicit infrastructure disposition before claim release", infraPark)
	}

	// A reviewer "fail" verdict must terminate ESCALATED, not merely abort
	// (design doc §4 D2, and the same rationale implementation.yaml's own
	// park-escalated documents: every escalation surface keys on the phase).
	park, ok := m.Task("park-escalated")
	if !ok {
		t.Fatal("park-escalated not found")
	}
	if park.Next != "release-escalated-claim" {
		t.Errorf("park-escalated next = %q, want release-escalated-claim", park.Next)
	}

	for name, next := range map[string]string{
		"guard-before-agent-context": "gather-review-threads",
		"guard-before-implement":     "implement",
		"guard-before-review":        "review",
		"guard-before-local-ci":      "local-ci",
		"guard-before-push":          "push-remediated",
	} {
		guard, ok := m.Task(name)
		if !ok {
			t.Errorf("%s not found", name)
			continue
		}
		if guard.Run == nil || !reflect.DeepEqual(guard.Run.Command, []string{"goobers", "pr-claim"}) {
			t.Errorf("%s command = %v, want PR lifecycle check", name, guard.Run)
		}
		if guard.Next != next {
			t.Errorf("%s next = %q, want %q", name, guard.Next, next)
		}
	}
	release, ok := m.Task("release-claim")
	if !ok {
		t.Fatal("release-claim not found")
	}
	if release.Run == nil || !reflect.DeepEqual(release.Run.Command, []string{"goobers", "pr-claim", "--release"}) {
		t.Errorf("release-claim command = %v, want explicit PR claim release", release.Run)
	}
	if release.Next != "" {
		t.Errorf("release-claim next = %q, want terminal", release.Next)
	}
	escalatedRelease, ok := m.Task("release-escalated-claim")
	if !ok {
		t.Fatal("release-escalated-claim not found")
	}
	if escalatedRelease.Run == nil || !reflect.DeepEqual(escalatedRelease.Run.Command, []string{"goobers", "pr-claim", "--release"}) {
		t.Errorf("release-escalated-claim command = %v, want explicit PR claim release", escalatedRelease.Run)
	}
	if escalatedRelease.Next != TargetEscalate {
		t.Errorf("release-escalated-claim next = %q, want %q", escalatedRelease.Next, TargetEscalate)
	}
	if got := park.Inputs["resultFile"]; got != "reviewer-escalation-result.json" {
		t.Errorf("park-escalated resultFile = %q, want reviewer-escalation-result.json", got)
	}
	for _, output := range []string{"escalationOutcome", "remediationAttempted", "attemptedCauses", "escalationReason"} {
		if !containsString(park.ExpectedOutputs, output) {
			t.Errorf("park-escalated expectedOutputs = %v, missing %q", park.ExpectedOutputs, output)
		}
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
	if gatherCI.Next != "gather-sibling-context" {
		t.Errorf("gather-ci-failures next = %q, want gather-sibling-context", gatherCI.Next)
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
		"hasSiblingOverlap":      "hasSiblingOverlap",
	}
	if len(rebase.InputsFrom) != len(wantRouting) {
		t.Fatalf("rebase-pr inputsFrom = %v, want routing-only subset %v", rebase.InputsFrom, wantRouting)
	}
	for key, want := range wantRouting {
		if got := rebase.InputsFrom[key]; got != want {
			t.Errorf("rebase-pr inputsFrom[%q] = %q, want %q", key, got, want)
		}
	}
	if !containsString(rebase.Capabilities, "github:pr:write") {
		t.Errorf("rebase-pr capabilities = %v, missing github:pr:write used to verify post-merge handoff authors", rebase.Capabilities)
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
		"..", "..", "reference-workflows", "gaggles", "goobers", "goobers", "implementer", "instructions.md",
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
	if respond.Next != "published-remediation-gate" {
		t.Errorf("respond-to-findings next = %q, want published-remediation-gate", respond.Next)
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
	publishedGate, ok := m.Gate("published-remediation-gate")
	if !ok || publishedGate.Automated == nil ||
		publishedGate.Automated.Check != "output-equals" ||
		publishedGate.Automated.Params["key"] != "posted" ||
		publishedGate.Automated.Params["equals"] != "true" ||
		publishedGate.Branches["pass"] != "resolve-review-threads" ||
		publishedGate.Branches["fail"] != "release-claim" {
		t.Errorf("published-remediation-gate = %+v, want posted publication routing", publishedGate)
	}
	resolveThreads, ok := m.Task("resolve-review-threads")
	if !ok {
		t.Fatal("resolve-review-threads not found")
	}
	if resolveThreads.Run == nil || !reflect.DeepEqual(resolveThreads.Run.Command, []string{"goobers", "resolve-review-threads"}) {
		t.Errorf("resolve-review-threads command = %v", resolveThreads.Run)
	}
	if resolveThreads.Next != "review-threads-gate" ||
		!containsString(resolveThreads.ExpectedOutputs, "unresolvedThreadCount") {
		t.Errorf("resolve-review-threads routing contract = next %q outputs %v", resolveThreads.Next, resolveThreads.ExpectedOutputs)
	}
	threadGate, ok := m.Gate("review-threads-gate")
	if !ok || threadGate.Automated == nil ||
		threadGate.Automated.Check != "output-equals" ||
		threadGate.Automated.Params["key"] != "unresolvedThreadCount" ||
		threadGate.Automated.Params["equals"] != "0" ||
		threadGate.Branches["pass"] != "release-claim" ||
		threadGate.Branches["fail"] != "park-unresolved-review-threads" {
		t.Errorf("review-threads-gate = %+v, want unresolved-count routing", threadGate)
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
	for _, output := range []string{"remediationCauses", "conflict", "conflictLocations", "attemptedHeadSha", "rebaseBaseSha"} {
		if !containsString(rebase.ExpectedOutputs, output) {
			t.Errorf("rebase-pr expectedOutputs = %v, missing %q structural-collision evidence", rebase.ExpectedOutputs, output)
		}
		if checkpoint.InputsFrom[output] != output {
			t.Errorf("remediation-checkpoint inputsFrom[%q] = %q, want %q", output, checkpoint.InputsFrom[output], output)
		}
	}
	wantBudgets := map[string]string{
		"conflictBudget":       "2",
		"substantiveBudget":    "2",
		"failingCIBudget":      "2",
		"siblingOverlapBudget": "2",
		"humanCommentBudget":   "2",
	}
	for input, want := range wantBudgets {
		if got := checkpoint.Inputs[input]; got != want {
			t.Errorf("remediation-checkpoint inputs[%q] = %q, want DSL-declared budget %q", input, got, want)
		}
	}
	if remediate := rebase.Inputs["remediate"]; !strings.Contains(remediate, "human-comment") {
		t.Errorf("rebase-pr remediate = %q, want it to include human-comment", remediate)
	}
	declared := map[string]bool{}
	for _, out := range checkpoint.ExpectedOutputs {
		declared[out] = true
	}
	for _, want := range []string{
		"continueRemediation", "selectedNumber", "head", "headSha",
		"escalationOutcome", "remediationAttempted", "attemptedCauses", "escalationReason",
	} {
		if !declared[want] {
			t.Errorf("remediation-checkpoint expectedOutputs = %v, missing %q", checkpoint.ExpectedOutputs, want)
		}
	}
	if checkpoint.Next != "checkpoint-gate" {
		t.Errorf("remediation-checkpoint next = %q, want checkpoint-gate — a terminal checkpoint is exactly the #892 dead end", checkpoint.Next)
	}
}
