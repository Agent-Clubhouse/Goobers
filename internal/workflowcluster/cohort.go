package workflowcluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TaskClassLabel explicitly assigns a workflow to a task cohort.
const TaskClassLabel = "goobers.dev/task-class"

// Cohort returns the workflow's privacy-safe task cohort. An explicit task
// class wins; otherwise the label is a digest of structured execution signals.
func Cohort(wf apiv1.Workflow) string {
	if label := strings.TrimSpace(wf.Labels[TaskClassLabel]); label != "" {
		return label
	}

	triggerTypes := map[string]bool{}
	workItemKinds := map[string]bool{}
	for _, trigger := range wf.Spec.Triggers {
		triggerTypes[string(trigger.Type)] = true
		switch trigger.Type {
		case apiv1.TriggerBacklogItem:
			workItemKinds["issue"] = true
		case apiv1.TriggerSchedule:
			workItemKinds["schedule"] = true
		case apiv1.TriggerWebhook:
			for _, event := range trigger.Events {
				workItemKinds[webhookKind(event)] = true
			}
			if len(trigger.Events) == 0 {
				workItemKinds["webhook"] = true
			}
		case apiv1.TriggerManual:
			workItemKinds["manual"] = true
		case apiv1.TriggerSignal:
			workItemKinds["signal"] = true
		}
	}

	sideEffects := map[string]bool{}
	for _, task := range wf.Spec.Tasks {
		for _, action := range task.PolicyActions {
			sideEffects[action] = true
		}
	}
	if len(sideEffects) == 0 {
		sideEffects["none"] = true
	}

	repoCapabilities := map[string]bool{}
	if wf.Spec.Requires != nil {
		for _, capability := range wf.Spec.Requires.Capabilities {
			if strings.HasPrefix(capability, "repo:") {
				repoCapabilities[capability] = true
			}
		}
	}
	for _, task := range wf.Spec.Tasks {
		for _, capability := range task.Capabilities {
			if strings.HasPrefix(capability, "repo:") {
				repoCapabilities[capability] = true
			}
		}
	}

	signals := struct {
		TriggerTypes     []string `json:"triggerTypes"`
		WorkItemKinds    []string `json:"workItemKinds"`
		SideEffects      []string `json:"sideEffects"`
		RepoCapabilities []string `json:"repoCapabilities"`
	}{
		TriggerTypes:     sortedKeys(triggerTypes),
		WorkItemKinds:    sortedKeys(workItemKinds),
		SideEffects:      sortedKeys(sideEffects),
		RepoCapabilities: sortedKeys(repoCapabilities),
	}
	data, _ := json.Marshal(signals)
	sum := sha256.Sum256(data)
	return "auto:" + hex.EncodeToString(sum[:8])
}

func webhookKind(event string) string {
	event = strings.ToLower(strings.TrimSpace(event))
	switch {
	case strings.HasPrefix(event, "pull_request"):
		return "pr"
	case event == "issues", strings.HasPrefix(event, "issue_"):
		return "issue"
	default:
		return "webhook"
	}
}

func cohortStrata(points []Point) Strata {
	strata := make(Strata, len(points))
	for _, point := range points {
		if point.Cohort != "" {
			strata[point.ID] = point.Cohort
		}
	}
	return strata
}

func cohortLabels(points []Point) []string {
	labels := map[string]bool{}
	for _, point := range points {
		if point.Cohort != "" {
			labels[point.Cohort] = true
		}
	}
	return sortedKeys(labels)
}
