package providers

import (
	"strings"
	"unicode"
)

type labelNameFold func(string) string

func exactLabelName(name string) string {
	return name
}

func lowerLabelName(name string) string {
	return strings.ToLower(name)
}

func equalFoldLabelName(name string) string {
	return strings.Map(func(r rune) rune {
		folded := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < folded {
				folded = next
			}
		}
		return folded
	}, name)
}

func normalizeLabelSpecs(labels []WorkItemLabel) []WorkItemLabel {
	normalized := make([]WorkItemLabel, len(labels))
	for i, label := range labels {
		label.Name = strings.TrimSpace(label.Name)
		label.Color = strings.TrimPrefix(strings.TrimSpace(label.Color), "#")
		normalized[i] = label
	}
	return normalized
}

func labelNameSet(labels []string, fold labelNameFold) map[string]struct{} {
	set := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		set[fold(label)] = struct{}{}
	}
	return set
}

type labelEnsureStep struct {
	Label  WorkItemLabel
	Create bool
}

func planLabelEnsure(existing []string, labels []WorkItemLabel, fold labelNameFold) []labelEnsureStep {
	have := labelNameSet(existing, fold)
	steps := make([]labelEnsureStep, 0, len(labels))
	for _, label := range normalizeLabelSpecs(labels) {
		key := fold(label.Name)
		_, present := have[key]
		steps = append(steps, labelEnsureStep{Label: label, Create: !present})
		have[key] = struct{}{}
	}
	return steps
}

type labelMutationPlan struct {
	Add    []string
	Remove []string
	Result []string
}

func planLabelMutation(current, add, remove []string, fold labelNameFold) labelMutationPlan {
	current = normalizeLabelNames(current)
	add = normalizeLabelNames(add)
	remove = normalizeLabelNames(remove)

	knownCurrent := current != nil
	currentSet := labelNameSet(current, fold)
	removeSet := labelNameSet(remove, fold)

	plan := labelMutationPlan{Result: make([]string, 0, len(current)+len(add))}
	resultSet := make(map[string]struct{}, len(current)+len(add))
	for _, label := range current {
		key := fold(label)
		if _, drop := removeSet[key]; drop {
			continue
		}
		if _, duplicate := resultSet[key]; duplicate {
			continue
		}
		resultSet[key] = struct{}{}
		plan.Result = append(plan.Result, label)
	}

	seenAdd := make(map[string]struct{}, len(add))
	for _, label := range add {
		key := fold(label)
		if _, duplicate := seenAdd[key]; duplicate {
			continue
		}
		seenAdd[key] = struct{}{}
		if _, present := resultSet[key]; !knownCurrent || !present {
			plan.Add = append(plan.Add, label)
		}
		if _, present := resultSet[key]; !present {
			resultSet[key] = struct{}{}
			plan.Result = append(plan.Result, label)
		}
	}

	seenRemove := make(map[string]struct{}, len(remove))
	for _, label := range remove {
		key := fold(label)
		if _, duplicate := seenRemove[key]; duplicate {
			continue
		}
		seenRemove[key] = struct{}{}
		if _, present := currentSet[key]; !knownCurrent || present {
			plan.Remove = append(plan.Remove, label)
		}
	}
	return plan
}

func normalizeLabelNames(labels []string) []string {
	if labels == nil {
		return nil
	}
	normalized := make([]string, 0, len(labels))
	for _, label := range labels {
		if label = strings.TrimSpace(label); label != "" {
			normalized = append(normalized, label)
		}
	}
	return normalized
}
