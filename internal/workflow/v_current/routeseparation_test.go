package vcurrent

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
)

// route-backlog-item's separation of duties (personal-gaggle-routing §5.7) is
// what keeps routing from becoming a privilege-escalation path: a router
// decides where work goes, so a router that could ALSO write the repository or
// its pull requests could route work to itself and act on it, collapsing the
// router/destination boundary the topology depends on. The compiler refuses
// that combination rather than trusting authors to keep the two apart.

func routerTask(caps ...string) apiv1.Task {
	return apiv1.Task{
		Name: "route-backlog",
		Type: apiv1.TaskDeterministic,
		Goal: "Apply routing labels and release ownership.",
		Run: &apiv1.DeterministicRun{
			Command: []string{"goobers", "backlog-query", "--route"},
		},
		Capabilities:  caps,
		PolicyActions: []string{"route-backlog-item"},
	}
}

func routerProblems(t *testing.T, task apiv1.Task) []string {
	t.Helper()
	return policyActionProblems(Definition{
		Name:    "router",
		Version: 1,
		Spec:    apiv1.WorkflowSpec{Gaggle: "router", Tasks: []apiv1.Task{task}},
	}, nil)
}

func TestRouteBacklogItemRequiresIssueWrite(t *testing.T) {
	problems := routerProblems(t, routerTask())
	if len(problems) == 0 {
		t.Fatal("route-backlog-item without github:issues:write must be rejected")
	}
	if !strings.Contains(strings.Join(problems, "\n"), string(capability.GitHubIssuesWrite)) {
		t.Fatalf("diagnostic should name the required capability, got: %v", problems)
	}
}

func TestRouteBacklogItemAcceptsIssueWriteAlone(t *testing.T) {
	problems := routerProblems(t, routerTask(string(capability.GitHubIssuesWrite)))
	if len(problems) != 0 {
		t.Fatalf("a router holding only github:issues:write must be accepted, got: %v", problems)
	}
}

func TestRouteBacklogItemRefusesRepositoryAndPRWritingRouters(t *testing.T) {
	for _, forbidden := range []capability.Capability{
		capability.RepoPush,
		capability.GitHubPRWrite,
		capability.GitHubPRMerge,
		capability.GitHubPRReview,
	} {
		task := routerTask(string(capability.GitHubIssuesWrite), string(forbidden))
		problems := routerProblems(t, task)
		joined := strings.Join(problems, "\n")
		if !strings.Contains(joined, "must not be combined with capability") {
			t.Errorf("router holding %q must be refused, got: %v", forbidden, problems)
			continue
		}
		if !strings.Contains(joined, string(forbidden)) {
			t.Errorf("diagnostic should name %q, got: %v", forbidden, problems)
		}
	}
}

// --- Static route inputs (§5.4) ---

// allowedRouteLabels (and its siblings) are the constraints on the routing
// transaction, not data flowing into it. Since inputsFrom values are merged
// into the same flat Inputs map the static ones occupy, a runtime read cannot
// tell an override from an author's declaration — so the mapping is refused at
// compile/config time, which is the last point where the provenance exists.

func routerTaskWithInputsFrom(inputsFrom map[string]string) apiv1.Task {
	task := routerTask(string(capability.GitHubIssuesWrite))
	task.Inputs = map[string]string{
		"allowedRouteLabels": "goobers:routed,repo:*",
		"routePlanFile":      "route-plan.json",
		"trustLabel":         "goobers:route-approved",
	}
	task.InputsFrom = inputsFrom
	return task
}

func TestRouteRejectsInputsFromOnSecuritySensitiveStaticInputs(t *testing.T) {
	for _, input := range []string{"allowedRouteLabels", "routePlanFile", "trustLabel", "claimLabel"} {
		task := routerTaskWithInputsFrom(map[string]string{input: "decision"})
		problems := routerProblems(t, task)
		joined := strings.Join(problems, "\n")
		if !strings.Contains(joined, "must declare input") {
			t.Errorf("inputsFrom on %q must be refused, got: %v", input, problems)
			continue
		}
		if !strings.Contains(joined, input) {
			t.Errorf("diagnostic should name %q, got: %v", input, problems)
		}
		if !strings.Contains(joined, "decision") {
			t.Errorf("diagnostic should name the upstream output, got: %v", problems)
		}
	}
}

// TestRouteReportsEverySensitiveInputsFromMapping: an author wiring several at
// once should see all of them, not just the first, and in a stable order.
func TestRouteReportsEverySensitiveInputsFromMapping(t *testing.T) {
	task := routerTaskWithInputsFrom(map[string]string{
		"allowedRouteLabels": "widenedAllowlist",
		"routePlanFile":      "chosenPlan",
		"resultFile":         "wherever",
	})
	problems := routerProblems(t, task)
	for _, want := range []string{"allowedRouteLabels", "routePlanFile"} {
		if !strings.Contains(strings.Join(problems, "\n"), want) {
			t.Errorf("mapping on %q was not reported, got: %v", want, problems)
		}
	}
	reported := 0
	for _, problem := range problems {
		if strings.Contains(problem, "must declare input") {
			reported++
		}
	}
	if reported != 2 {
		t.Fatalf("want exactly the two sensitive mappings reported, got %d: %v", reported, problems)
	}
	// Stable ordering: allowedRouteLabels sorts before routePlanFile.
	if strings.Index(strings.Join(problems, "\n"), "allowedRouteLabels") > strings.Index(strings.Join(problems, "\n"), "routePlanFile") {
		t.Fatalf("diagnostics must be deterministically ordered, got: %v", problems)
	}
}

// TestRouteAcceptsInputsFromOnNonSensitiveInputs keeps the check narrow: the
// route stage still consumes ordinary handoffs, e.g. the plan a preceding
// agentic stage emitted a result-file path for.
func TestRouteAcceptsInputsFromOnNonSensitiveInputs(t *testing.T) {
	task := routerTaskWithInputsFrom(map[string]string{"resultFile": "resultFile"})
	for _, problem := range routerProblems(t, task) {
		if strings.Contains(problem, "must declare input") {
			t.Fatalf("a non-sensitive inputsFrom mapping must be accepted, got: %v", problem)
		}
	}
}

// TestStaticRouteInputsOnlyApplyToRouteMode: the same input names on a claim or
// reconcile stage are ordinary configuration and must not be refused.
func TestStaticRouteInputsOnlyApplyToRouteMode(t *testing.T) {
	for _, args := range [][]string{
		{"goobers", "backlog-query", "--claim"},
		{"goobers", "backlog-query", "--reconcile"},
		{"goobers", "backlog-health"},
	} {
		task := routerTaskWithInputsFrom(map[string]string{"trustLabel": "resolvedTrustLabel"})
		task.Run = &apiv1.DeterministicRun{Command: args}
		task.PolicyActions = nil
		for _, problem := range staticRouteInputProblems(task) {
			t.Errorf("%v must not be gated by the route contract, got: %v", args, problem)
		}
	}
}

// TestStaticRouteInputsApplyWithoutDeclaredPolicyAction closes the obvious
// bypass: the check keys on the command actually running --route, so omitting
// route-backlog-item does not disable it.
func TestStaticRouteInputsApplyWithoutDeclaredPolicyAction(t *testing.T) {
	task := routerTaskWithInputsFrom(map[string]string{"allowedRouteLabels": "widened"})
	task.PolicyActions = nil
	if len(staticRouteInputProblems(task)) == 0 {
		t.Fatal("dropping the policy action must not disable the static route input check")
	}
}

// TestNonRouterTasksKeepWritingCapabilities is the necessary converse: the
// separation is a property of route-backlog-item, not a global ban. A
// destination gaggle's implementation stage still holds repo:push.
func TestNonRouterTasksKeepWritingCapabilities(t *testing.T) {
	task := apiv1.Task{
		Name: "open-pr",
		Type: apiv1.TaskDeterministic,
		Goal: "Open a pull request.",
		Run: &apiv1.DeterministicRun{
			Command: []string{"goobers", "open-pr"},
		},
		Capabilities:  []string{string(capability.RepoPush), string(capability.GitHubPRWrite)},
		PolicyActions: []string{"open-pr"},
	}
	problems := policyActionProblems(Definition{
		Name:    "implementation",
		Version: 1,
		Spec:    apiv1.WorkflowSpec{Gaggle: "destination", Tasks: []apiv1.Task{task}},
	}, nil)
	for _, problem := range problems {
		if strings.Contains(problem, "must not be combined with capability") {
			t.Fatalf("a non-router task must keep its writing capabilities, got: %v", problems)
		}
	}
}
