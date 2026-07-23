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
	"approve-issue":                 {requiredCapabilities: []capability.Capability{capability.GitHubIssuesApprove}},
	"claim-backlog-items":           {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"clear-remediation":             {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"close-issue":                   {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"close-issues":                  {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"close-pr":                      {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"comment-on-issue":              {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"create-issue":                  {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"delete-branch":                 {requiredCapabilities: []capability.Capability{capability.GitHubBranchDelete}},
	"demote-pr":                     {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"edit-issue":                    {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"escalate-pr":                   {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"fan-out-remediation":           {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"label-issue":                   {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"merge-pr":                      {requiredCapabilities: []capability.Capability{capability.GitHubPRMerge}},
	"modify-repository":             {requiredCapabilities: []capability.Capability{capability.RepoPush}},
	"open-or-update-pr":             {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"publish-review":                {requiredCapabilities: []capability.Capability{capability.GitHubPRReview}},
	"push-repository-branch":        {requiredCapabilities: []capability.Capability{capability.RepoPush}},
	"push-pr-branch":                {requiredCapabilities: []capability.Capability{capability.RepoPush}},
	"rebase-pr":                     {requiredCapabilities: []capability.Capability{capability.RepoPush}},
	"record-merge-refusal":          {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"record-remediation-checkpoint": {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"release-backlog-claim":         {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"respond-to-findings":           {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"rework-pr":                     {requiredCapabilities: []capability.Capability{capability.RepoPush}},
	"route-queue-outcome":           {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"route-verdict":                 {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"update-issue":                  {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"update-pr-branch":              {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"watch-merge-queue":             {requiredCapabilities: []capability.Capability{capability.GitHubPRMerge}},
}

var commandPolicyActions = map[string][]string{
	"apply-verdict":          {"publish-review", "route-verdict", "close-pr"},
	"issue-close-out":        {"update-issue"},
	"merge-pr":               {"merge-pr", "delete-branch"},
	"merge-queue-poll":       {"watch-merge-queue", "route-queue-outcome", "delete-branch"},
	"open-pr":                {"open-or-update-pr"},
	"post-merge":             {"close-issues", "fan-out-remediation"},
	"push-branch":            {"push-repository-branch"},
	"push-remediated":        {"push-pr-branch", "clear-remediation"},
	"rebase-pr":              {"rebase-pr", "clear-remediation"},
	"reconcile-post-merge":   {"close-issues", "fan-out-remediation", "delete-branch"},
	"record-merge-refusal":   {"record-merge-refusal", "demote-pr"},
	"remediation-checkpoint": {"record-remediation-checkpoint", "escalate-pr"},
	"respond-to-findings":    {"respond-to-findings"},
	"update-behind-pr":       {"update-pr-branch", "clear-remediation"},
}

var commandArgumentPolicyActions = map[string]map[string][]string{
	"backlog-query": {
		"--claim":   {"claim-backlog-items"},
		"--release": {"release-backlog-claim"},
	},
}

func policyActionProblems(def Definition, goobers map[string]apiv1.GooberSpec) []string {
	var problems []string
	known := knownPolicyActions()
	checkedGoobers := map[string]bool{}
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
		for _, action := range prescribedCommandPolicyActions(task) {
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

		if goobers == nil || task.Type != apiv1.TaskAgentic || task.Goober == "" {
			continue
		}
		if hasMutationCapability(task.Capabilities) && len(task.PolicyActions) == 0 {
			problems = append(problems, fmt.Sprintf(
				"agentic task %q has mutation-capable grants but declares no policyActions", task.Name))
		}
		goober, ok := goobers[task.Goober]
		if !ok {
			continue
		}
		if hasMutationCapability(task.Capabilities) &&
			len(goober.PolicyActions) == 0 && len(goober.ConditionalPolicyActions) == 0 {
			problems = append(problems, fmt.Sprintf(
				"agentic task %q invokes mutation-capable goober %q, but the goober declares no persona policyActions",
				task.Name, task.Goober))
		}
		if !checkedGoobers[task.Goober] {
			problems = append(problems, gooberPolicyActionProblems(task.Goober, goober, known)...)
			checkedGoobers[task.Goober] = true
		}
		for _, action := range goober.PolicyActions {
			if !declared[action] {
				problems = append(problems, fmt.Sprintf(
					"task %q invokes goober %q whose persona prescribes policy action %q, but policyActions does not declare it",
					task.Name, task.Goober, action))
			}
		}
	}
	return problems
}

func gooberPolicyActionProblems(name string, goober apiv1.GooberSpec, known []string) []string {
	var problems []string
	declared := map[string]bool{}
	for _, group := range [][]string{goober.PolicyActions, goober.ConditionalPolicyActions} {
		for _, action := range group {
			if declared[action] {
				problems = append(problems, fmt.Sprintf("goober %q declares duplicate policy action %q", name, action))
				continue
			}
			declared[action] = true
			contract, ok := policyActionContracts[action]
			if !ok {
				problems = append(problems, fmt.Sprintf(
					"goober %q declares unknown policy action %q (known actions: %s)",
					name, action, strings.Join(known, ", ")))
				continue
			}
			grants := toSet(goober.Capabilities)
			for _, required := range contract.requiredCapabilities {
				if !grants[string(required)] {
					problems = append(problems, fmt.Sprintf(
						"goober %q policy action %q requires capability %q, but the goober does not grant it",
						name, action, required))
				}
			}
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

func prescribedCommandPolicyActions(task apiv1.Task) []string {
	command := policyCommand(task)
	actions := append([]string(nil), commandPolicyActions[command]...)
	if task.Run == nil {
		return actions
	}
	for index, arg := range task.Run.Command {
		if index < 2 {
			continue
		}
		actions = append(actions, commandArgumentPolicyActions[command][arg]...)
	}
	return actions
}

func hasMutationCapability(capabilities []string) bool {
	mutationCapabilities := map[string]bool{
		string(capability.RepoPush):            true,
		string(capability.GitHubIssuesWrite):   true,
		string(capability.GitHubIssuesApprove): true,
		string(capability.GitHubPRWrite):       true,
		string(capability.GitHubPRReview):      true,
		string(capability.GitHubBranchDelete):  true,
		string(capability.GitHubPRMerge):       true,
	}
	for _, grant := range capabilities {
		if mutationCapabilities[grant] {
			return true
		}
	}
	return false
}

func knownPolicyActions() []string {
	actions := make([]string, 0, len(policyActionContracts))
	for action := range policyActionContracts {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions
}
