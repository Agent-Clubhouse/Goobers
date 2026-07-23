package workflow

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
)

type policyActionContract struct {
	requiredCapabilities []capability.Capability
}

var policyActionContracts = map[string]policyActionContract{
	"clear-remediation":   {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"close-issues":        {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"close-pr":            {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"delete-branch":       {requiredCapabilities: []capability.Capability{capability.GitHubBranchDelete}},
	"demote-pr":           {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"escalate-pr":         {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"fan-out-remediation": {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"merge-pr":            {requiredCapabilities: []capability.Capability{capability.GitHubPRMerge}},
	"publish-review":      {requiredCapabilities: []capability.Capability{capability.GitHubPRReview}},
	"push-pr-branch":      {requiredCapabilities: []capability.Capability{capability.RepoPush}},
	"rebase-pr":           {requiredCapabilities: []capability.Capability{capability.RepoPush}},
	"respond-to-findings": {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"rework-pr":           {requiredCapabilities: []capability.Capability{capability.RepoPush}},
	"route-queue-outcome": {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"route-verdict":       {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"update-pr-branch":    {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"watch-merge-queue":   {requiredCapabilities: []capability.Capability{capability.GitHubPRMerge}},
}

var commandPolicyActions = map[string][]string{
	"apply-verdict":          {"publish-review", "route-verdict", "close-pr"},
	"merge-pr":               {"merge-pr", "delete-branch"},
	"merge-queue-poll":       {"watch-merge-queue", "route-queue-outcome", "delete-branch"},
	"post-merge":             {"close-issues", "fan-out-remediation"},
	"push-remediated":        {"push-pr-branch", "clear-remediation"},
	"rebase-pr":              {"rebase-pr", "clear-remediation"},
	"reconcile-post-merge":   {"close-issues", "fan-out-remediation", "delete-branch"},
	"record-merge-refusal":   {"demote-pr"},
	"remediation-checkpoint": {"escalate-pr"},
	"respond-to-findings":    {"respond-to-findings"},
	"update-behind-pr":       {"update-pr-branch", "clear-remediation"},
}

func policyActionProblems(def Definition) []string {
	var problems []string
	known := knownPolicyActions()
	for _, task := range def.Spec.Tasks {
		declared := make(map[string]bool, len(task.PolicyActions))
		checked := make(map[string]bool, len(task.PolicyActions))
		for _, action := range task.PolicyActions {
			if declared[action] {
				problems = append(problems, fmt.Sprintf("task %q declares duplicate policy action %q", task.Name, action))
				continue
			}
			declared[action] = true
			contract, ok := policyActionContracts[action]
			if !ok {
				problems = append(problems, fmt.Sprintf(
					"task %q declares unknown policy action %q (known actions: %s)",
					task.Name, action, strings.Join(known, ", ")))
				continue
			}
			problems = append(problems, missingPolicyActionCapabilities(task, action, contract)...)
			checked[action] = true
		}

		command := policyCommand(task)
		for _, action := range commandPolicyActions[command] {
			if !declared[action] {
				problems = append(problems, fmt.Sprintf(
					"task %q command %q prescribes policy action %q but policyActions does not declare it",
					task.Name, "goobers "+command, action))
			}
			if checked[action] {
				continue
			}
			problems = append(problems, missingPolicyActionCapabilities(task, action, policyActionContracts[action])...)
			checked[action] = true
		}
	}
	return problems
}

func missingPolicyActionCapabilities(task apiv1.Task, action string, contract policyActionContract) []string {
	declared := toSet(task.Capabilities)
	var problems []string
	for _, required := range contract.requiredCapabilities {
		if !declared[string(required)] {
			problems = append(problems, fmt.Sprintf(
				"task %q policy action %q requires capability %q, but the task does not declare it",
				task.Name, action, required))
		}
	}
	return problems
}

func policyCommand(task apiv1.Task) string {
	if task.Run == nil || len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" {
		return ""
	}
	return task.Run.Command[1]
}

func knownPolicyActions() []string {
	actions := make([]string, 0, len(policyActionContracts))
	for action := range policyActionContracts {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions
}
