package engine

import (
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/temporaltest"
)

// A topology (b) gaggle (GitHub backlog, Azure DevOps code) routes backlog
// work and backlog-family credentials by role, resolved from the instance
// config. A stage pod has no instance config, so a backlog stage there would
// open Azure DevOps with the GitHub backlog credential. The engine therefore
// refuses every stage of such a run before a pod is created, as a normal,
// journaled stage failure with its own code.
func TestModeThreeRefusesTopologyBStageBeforeDispatch(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@hourly"}},
		Start:    "query-backlog",
		Tasks: []apiv1.Task{
			{Name: "query-backlog", Type: apiv1.TaskDeterministic, Goal: "list backlog issues",
				Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-health"}, Workspace: apiv1.WorkspaceScratch},
				Capabilities: []string{"github:issues:read"}},
		},
	}
	in := runInput("mode-three-topology-b", spec)
	in.RepoRef = apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: "web"}
	in.RoleRoutedBacklogProvider = apiv1.ProviderGitHub
	in.Placements = []PinnedPlacement{{
		Stage: "query-backlog", Queue: dispatcher.QueueName("web", "linux-toolchain"),
		Eligible: remoteEligible(), Memory: "1Gi",
	}}
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux-toolchain", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenderStore(t)})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v (the refusal must be a normal stage failure)", err)
	}
	var result RunResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", result.Status, StatusFailed)
	}
	if result.FailureCode != CrossProviderBacklogPodCode {
		t.Fatalf("failure code = %q, want %q", result.FailureCode, CrossProviderBacklogPodCode)
	}
	if !strings.Contains(result.FailureMessage, "topology (b)") || !strings.Contains(result.FailureMessage, "self runner") {
		t.Fatalf("failure message = %q, want it to name topology (b) and the self-runner remedy", result.FailureMessage)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("a topology (b) stage must never reach the dispatcher — no pod may be created")
	}
}

// The same run with no role-routed backlog (every same-provider gaggle, and
// every input persisted before the field existed) reaches the dispatcher
// exactly as before.
func TestModeThreeDispatchesWithoutRoleRoutedBacklog(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@hourly"}},
		Start:    "query-backlog",
		Tasks: []apiv1.Task{
			{Name: "query-backlog", Type: apiv1.TaskDeterministic, Goal: "list backlog issues",
				Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-health"}, Workspace: apiv1.WorkspaceScratch},
				Capabilities: []string{"github:issues:read"}},
		},
	}
	in := runInput("mode-three-same-provider", spec)
	in.RepoRef = apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: "web"}
	in.Placements = []PinnedPlacement{{
		Stage: "query-backlog", Queue: dispatcher.QueueName("web", "linux-toolchain"),
		Eligible: remoteEligible(), Memory: "1Gi",
	}}
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux-toolchain", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenderStore(t)})
	env.ExecuteWorkflow(Run, in)
	if fake.calls.Load() == 0 {
		t.Fatal("a same-provider gaggle's stage must still reach the dispatcher")
	}
}
