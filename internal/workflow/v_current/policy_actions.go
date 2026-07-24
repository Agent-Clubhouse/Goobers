package vcurrent

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
)

type policyActionContract struct {
	requiredCapabilities []capability.Capability
}

var policyActionContracts = map[string]policyActionContract{
	"approve-issue":                 {requiredCapabilities: []capability.Capability{capability.GitHubIssuesApprove}},
	"assign-milestone":              {requiredCapabilities: []capability.Capability{capability.GitHubMilestonesWrite}},
	"claim-backlog-items":           {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"clear-healed-demotions":        {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"clear-healed-escalations":      {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
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
	"flag-foundation-coupling":      {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"flag-scope-drift":              {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
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
	"unpark-resolved-siblings":      {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"update-issue":                  {requiredCapabilities: []capability.Capability{capability.GitHubIssuesWrite}},
	"update-pr-branch":              {requiredCapabilities: []capability.Capability{capability.GitHubPRWrite}},
	"watch-merge-queue":             {requiredCapabilities: []capability.Capability{capability.GitHubPRMerge}},
}

var commandPolicyActions = map[string][]string{
	"apply-verdict":          {"publish-review", "route-verdict", "close-pr"},
	"gather-sibling-context": {"flag-scope-drift"},
	"issue-close-out":        {"update-issue"},
	"merge-pr":               {"merge-pr", "delete-branch"},
	"merge-queue-poll":       {"watch-merge-queue", "route-queue-outcome", "delete-branch"},
	"open-pr":                {"open-or-update-pr"},
	"post-merge":             {"close-issues", "fan-out-remediation", "unpark-resolved-siblings", "clear-healed-escalations", "clear-healed-demotions"},
	"pr-select":              {"flag-foundation-coupling"},
	"push-branch":            {"push-repository-branch"},
	"push-remediated":        {"push-pr-branch", "clear-remediation"},
	"rebase-pr":              {"rebase-pr", "clear-remediation"},
	"reconcile-post-merge":   {"close-issues", "fan-out-remediation", "unpark-resolved-siblings", "clear-healed-escalations", "clear-healed-demotions", "delete-branch"},
	"record-merge-refusal":   {"record-merge-refusal", "demote-pr"},
	"remediation-checkpoint": {"record-remediation-checkpoint", "escalate-pr"},
	"respond-to-findings":    {"respond-to-findings"},
	"set-milestone":          {"assign-milestone"},
	"update-behind-pr":       {"update-pr-branch", "clear-remediation"},
}

var commandArgumentPolicyActions = map[string]map[string][]string{
	"backlog-query": {
		"claim":   {"claim-backlog-items"},
		"release": {"release-backlog-claim"},
	},
	"reconcile-branches": {
		"delete": {"delete-branch"},
	},
}

var commandArgumentPolicyActionInputs = map[string]map[string]string{
	"reconcile-branches": {
		"delete": "deleteBranches",
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
		goober, ok := goobers[task.Goober]
		if !ok {
			continue
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
		taskCapabilities := toSet(task.Capabilities)
		for _, action := range goober.ConditionalPolicyActions {
			if declared[action] {
				continue
			}
			contract, ok := policyActionContracts[action]
			if !ok {
				continue
			}
			for _, required := range contract.requiredCapabilities {
				if taskCapabilities[string(required)] {
					problems = append(problems, fmt.Sprintf(
						"task %q grants capability %q for goober %q conditional policy action %q, but policyActions does not declare it",
						task.Name, required, task.Goober, action))
				}
			}
		}
	}
	if goobers == nil {
		return problems
	}
	for _, gate := range def.Spec.Gates {
		if gate.Evaluator != apiv1.EvaluatorAgentic || gate.Agentic == nil || gate.Agentic.Goober == "" {
			continue
		}
		name := gate.Agentic.Goober
		goober, ok := goobers[name]
		if !ok {
			continue
		}
		if !checkedGoobers[name] {
			problems = append(problems, gooberPolicyActionProblems(name, goober, known)...)
			checkedGoobers[name] = true
		}
		for _, action := range append(append([]string(nil), goober.PolicyActions...), goober.ConditionalPolicyActions...) {
			problems = append(problems, fmt.Sprintf(
				"agentic gate %q invokes goober %q whose persona prescribes policy action %q, but agentic gates cannot opt into policy actions",
				gate.Name, name, action))
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
	argumentActions := commandArgumentPolicyActions[command]
	if task.Run == nil || len(argumentActions) == 0 {
		return actions
	}

	names := make([]string, 0, len(argumentActions))
	for name := range argumentActions {
		names = append(names, name)
	}
	sort.Strings(names)

	enabled := make(map[string]bool, len(names))
	for _, name := range names {
		defaultEnabled := false
		if inputName := commandArgumentPolicyActionInputs[command][name]; inputName != "" {
			_, dynamic := task.InputsFrom[inputName]
			defaultEnabled = dynamic
			if raw, ok := task.Inputs[inputName]; ok && !dynamic {
				if parsed, err := strconv.ParseBool(raw); err == nil {
					defaultEnabled = parsed
				}
			}
		}
		enabled[name] = defaultEnabled
	}
	for _, arg := range task.Run.Command[2:] {
		if arg == "--" {
			break
		}
		for _, name := range names {
			for _, prefix := range []string{"--" + name, "-" + name} {
				switch {
				case arg == prefix:
					enabled[name] = true
				case strings.HasPrefix(arg, prefix+"="):
					if parsed, err := strconv.ParseBool(strings.TrimPrefix(arg, prefix+"=")); err == nil {
						enabled[name] = parsed
					}
				}
			}
		}
	}
	for _, name := range names {
		if enabled[name] {
			actions = append(actions, argumentActions[name]...)
		}
	}
	return actions
}

func knownPolicyActions() []string {
	actions := make([]string, 0, len(policyActionContracts))
	for action := range policyActionContracts {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions
}
