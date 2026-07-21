// Package configdiff compares active workflow definitions with a canonical set.
package configdiff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Severity classifies whether a difference is allowed operational tuning or
// structural drift.
type Severity string

const (
	Informational Severity = "info"
	Error         Severity = "error"
)

// Difference is one deterministic, name-addressed workflow difference.
type Difference struct {
	Severity    Severity
	Workflow    string
	SubjectKind string
	Subject     string
	Field       string
	Active      string
	Canonical   string
}

type operationalField struct {
	documentedPath string
	diffPath       []string
}

// operationalFields is the single source of truth for allowed instance tuning;
// both OperationalFields and difference classification read this table. A "*"
// matches exactly one trigger identity segment. Trigger presence is enablement:
// adding or removing a trigger is informational, while changing a present
// trigger's non-schedule fields is structural.
var operationalFields = []operationalField{
	{"spec.triggers[] (presence/enablement)", []string{"spec", "triggers", "*"}},
	{"spec.triggers[].schedule", []string{"spec", "triggers", "*", "schedule"}},
	{"spec.readiness.maxConcurrentRuns", []string{"spec", "readiness", "maxConcurrentRuns"}},
	{"spec.readiness.maxRunsPerHour", []string{"spec", "readiness", "maxRunsPerHour"}},
	{"spec.readiness.maxRunsPerDay", []string{"spec", "readiness", "maxRunsPerDay"}},
	{"spec.readiness.maxOpenPRs", []string{"spec", "readiness", "maxOpenPRs"}},
}

// OperationalFields returns the documented operational tuning allowlist.
func OperationalFields() []string {
	fields := make([]string, len(operationalFields))
	for index, field := range operationalFields {
		fields[index] = field.documentedPath
	}
	return fields
}

// Compare returns all workflow differences in deterministic order. Active and
// canonical workflow, task, gate, and trigger ordering does not affect parity.
func Compare(active, canonical []apiv1.Workflow) ([]Difference, error) {
	activeByName, err := indexWorkflows(active)
	if err != nil {
		return nil, fmt.Errorf("index active workflows: %w", err)
	}
	canonicalByName, err := indexWorkflows(canonical)
	if err != nil {
		return nil, fmt.Errorf("index canonical workflows: %w", err)
	}

	names := unionKeys(activeByName, canonicalByName)
	var differences []Difference
	for _, name := range names {
		activeWorkflow, activeOK := activeByName[name]
		canonicalWorkflow, canonicalOK := canonicalByName[name]
		switch {
		case !activeOK:
			differences = append(differences, Difference{
				Severity:  Error,
				Workflow:  name,
				Field:     "<definition>",
				Active:    formatValue(missing{}),
				Canonical: formatValue(canonicalWorkflow),
			})
		case !canonicalOK:
			differences = append(differences, Difference{
				Severity:  Error,
				Workflow:  name,
				Field:     "<definition>",
				Active:    formatValue(activeWorkflow),
				Canonical: formatValue(missing{}),
			})
		default:
			alignTriggers(activeWorkflow, canonicalWorkflow)
			compareValues(&differences, name, nil, activeWorkflow, canonicalWorkflow)
		}
	}
	return differences, nil
}

func indexWorkflows(workflows []apiv1.Workflow) (map[string]map[string]any, error) {
	indexed := make(map[string]map[string]any, len(workflows))
	for _, workflow := range workflows {
		identity := workflow.Spec.Gaggle + "/" + workflow.Name
		if _, exists := indexed[identity]; exists {
			return nil, fmt.Errorf("duplicate workflow %q", identity)
		}
		normalized, err := normalizeWorkflow(workflow)
		if err != nil {
			return nil, fmt.Errorf("normalize workflow %q: %w", identity, err)
		}
		indexed[identity] = normalized
	}
	return indexed, nil
}

func normalizeWorkflow(workflow apiv1.Workflow) (map[string]any, error) {
	data, err := json.Marshal(workflow)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var normalized map[string]any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, err
	}

	spec, ok := normalized["spec"].(map[string]any)
	if !ok {
		return nil, errorsAt("spec", normalized["spec"])
	}
	for _, field := range []string{"tasks", "gates"} {
		if err := normalizeNamedObjects(spec, field); err != nil {
			return nil, err
		}
	}
	if err := normalizeTriggers(spec); err != nil {
		return nil, err
	}
	return normalized, nil
}

func alignTriggers(active, canonical map[string]any) {
	activeTriggers := workflowTriggers(active)
	canonicalTriggers := workflowTriggers(canonical)
	activeGroups := groupTriggers(activeTriggers)
	canonicalGroups := groupTriggers(canonicalTriggers)

	types := make(map[string]struct{}, len(activeGroups)+len(canonicalGroups))
	for triggerType := range activeGroups {
		types[triggerType] = struct{}{}
	}
	for triggerType := range canonicalGroups {
		types[triggerType] = struct{}{}
	}
	sortedTypes := make([]string, 0, len(types))
	for triggerType := range types {
		sortedTypes = append(sortedTypes, triggerType)
	}
	sort.Strings(sortedTypes)

	alignedActive := make(map[string]any, len(activeTriggers))
	alignedCanonical := make(map[string]any, len(canonicalTriggers))
	for _, triggerType := range sortedTypes {
		activeDefinitions := activeGroups[triggerType]
		canonicalDefinitions := canonicalGroups[triggerType]
		matchedActive := make([]bool, len(activeDefinitions))
		matchedCanonical := make([]bool, len(canonicalDefinitions))
		index := 0

		for canonicalIndex, canonicalDefinition := range canonicalDefinitions {
			canonicalKey := triggerStructuralKey(canonicalDefinition)
			for activeIndex, activeDefinition := range activeDefinitions {
				if matchedActive[activeIndex] || triggerStructuralKey(activeDefinition) != canonicalKey {
					continue
				}
				key := fmt.Sprintf("%s[%d]", triggerType, index)
				alignedActive[key] = activeDefinition
				alignedCanonical[key] = canonicalDefinition
				matchedActive[activeIndex] = true
				matchedCanonical[canonicalIndex] = true
				index++
				break
			}
		}

		remainingActive := unmatchedTriggers(activeDefinitions, matchedActive)
		remainingCanonical := unmatchedTriggers(canonicalDefinitions, matchedCanonical)
		paired := min(len(remainingActive), len(remainingCanonical))
		for pairIndex := 0; pairIndex < paired; pairIndex++ {
			key := fmt.Sprintf("%s[%d]", triggerType, index)
			alignedActive[key] = remainingActive[pairIndex]
			alignedCanonical[key] = remainingCanonical[pairIndex]
			index++
		}
		for _, definition := range remainingActive[paired:] {
			alignedActive[fmt.Sprintf("%s[%d]", triggerType, index)] = definition
			index++
		}
		for _, definition := range remainingCanonical[paired:] {
			alignedCanonical[fmt.Sprintf("%s[%d]", triggerType, index)] = definition
			index++
		}
	}

	workflowSpec(active)["triggers"] = alignedActive
	workflowSpec(canonical)["triggers"] = alignedCanonical
}

func workflowTriggers(workflow map[string]any) map[string]any {
	triggers, _ := workflowSpec(workflow)["triggers"].(map[string]any)
	return triggers
}

func workflowSpec(workflow map[string]any) map[string]any {
	spec, _ := workflow["spec"].(map[string]any)
	return spec
}

func groupTriggers(triggers map[string]any) map[string][]map[string]any {
	groups := map[string][]map[string]any{}
	for _, raw := range triggers {
		trigger, _ := raw.(map[string]any)
		triggerType, _ := trigger["type"].(string)
		groups[triggerType] = append(groups[triggerType], trigger)
	}
	for _, definitions := range groups {
		sort.Slice(definitions, func(i, j int) bool {
			return formatValue(definitions[i]) < formatValue(definitions[j])
		})
	}
	return groups
}

func triggerStructuralKey(trigger map[string]any) string {
	structural := make(map[string]any, len(trigger))
	for key, value := range trigger {
		if key != "schedule" {
			structural[key] = value
		}
	}
	return formatValue(structural)
}

func unmatchedTriggers(definitions []map[string]any, matched []bool) []map[string]any {
	var unmatched []map[string]any
	for index, definition := range definitions {
		if !matched[index] {
			unmatched = append(unmatched, definition)
		}
	}
	return unmatched
}

func normalizeNamedObjects(spec map[string]any, field string) error {
	raw, ok := spec[field]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return errorsAt("spec."+field, raw)
	}
	objects := make(map[string]any, len(list))
	for _, item := range list {
		object, ok := item.(map[string]any)
		if !ok {
			return errorsAt("spec."+field+"[]", item)
		}
		name, ok := object["name"].(string)
		if !ok || name == "" {
			return errorsAt("spec."+field+"[].name", object["name"])
		}
		if _, exists := objects[name]; exists {
			return fmt.Errorf("spec.%s contains duplicate name %q", field, name)
		}
		if field == "tasks" {
			sortStringField(object, "capabilities")
			sortStringField(object, "requiredCapabilities")
			sortStringField(object, "expectedOutputs")
		}
		if field == "gates" {
			if human, ok := object["human"].(map[string]any); ok {
				sortStringField(human, "approvers")
			}
		}
		objects[name] = object
	}
	spec[field] = objects
	return nil
}

func normalizeTriggers(spec map[string]any) error {
	raw, ok := spec["triggers"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return errorsAt("spec.triggers", raw)
	}
	grouped := map[string][]map[string]any{}
	for _, item := range list {
		trigger, ok := item.(map[string]any)
		if !ok {
			return errorsAt("spec.triggers[]", item)
		}
		triggerType, ok := trigger["type"].(string)
		if !ok || triggerType == "" {
			return errorsAt("spec.triggers[].type", trigger["type"])
		}
		sortStringField(trigger, "events")
		grouped[triggerType] = append(grouped[triggerType], trigger)
	}
	triggers := make(map[string]any, len(list))
	for triggerType, definitions := range grouped {
		sort.Slice(definitions, func(i, j int) bool {
			return formatValue(definitions[i]) < formatValue(definitions[j])
		})
		for index, trigger := range definitions {
			triggers[fmt.Sprintf("%s[%d]", triggerType, index)] = trigger
		}
	}
	spec["triggers"] = triggers
	return nil
}

func sortStringField(object map[string]any, field string) {
	raw, ok := object[field]
	if !ok {
		return
	}
	list, ok := raw.([]any)
	if !ok {
		return
	}
	sort.Slice(list, func(i, j int) bool {
		left, leftOK := list[i].(string)
		right, rightOK := list[j].(string)
		return leftOK && rightOK && left < right
	})
}

func errorsAt(path string, value any) error {
	return fmt.Errorf("%s has unexpected value %s", path, formatValue(value))
}

func unionKeys(left, right map[string]map[string]any) []string {
	keys := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func compareValues(differences *[]Difference, workflow string, path []string, active, canonical any) {
	if reflect.DeepEqual(active, canonical) {
		return
	}

	activeMap, activeMapOK := active.(map[string]any)
	canonicalMap, canonicalMapOK := canonical.(map[string]any)
	if activeMapOK && canonicalMapOK {
		keys := make(map[string]struct{}, len(activeMap)+len(canonicalMap))
		for key := range activeMap {
			keys[key] = struct{}{}
		}
		for key := range canonicalMap {
			keys[key] = struct{}{}
		}
		sorted := make([]string, 0, len(keys))
		for key := range keys {
			sorted = append(sorted, key)
		}
		sort.Strings(sorted)
		for _, key := range sorted {
			activeValue, activeOK := activeMap[key]
			canonicalValue, canonicalOK := canonicalMap[key]
			nextPath := appendPath(path, key)
			switch {
			case !activeOK:
				compareMissingMap(differences, workflow, nextPath, false, canonicalValue)
			case !canonicalOK:
				compareMissingMap(differences, workflow, nextPath, true, activeValue)
			default:
				compareValues(differences, workflow, nextPath, activeValue, canonicalValue)
			}
		}
		return
	}

	activeList, activeListOK := active.([]any)
	canonicalList, canonicalListOK := canonical.([]any)
	if activeListOK && canonicalListOK {
		maxLength := max(len(activeList), len(canonicalList))
		for index := 0; index < maxLength; index++ {
			nextPath := appendPath(path, fmt.Sprintf("[%d]", index))
			switch {
			case index >= len(activeList):
				recordDifference(differences, workflow, nextPath, missing{}, canonicalList[index])
			case index >= len(canonicalList):
				recordDifference(differences, workflow, nextPath, activeList[index], missing{})
			default:
				compareValues(differences, workflow, nextPath, activeList[index], canonicalList[index])
			}
		}
		return
	}

	recordDifference(differences, workflow, path, active, canonical)
}

func compareMissingMap(differences *[]Difference, workflow string, path []string, activePresent bool, present any) {
	presentMap, isMap := present.(map[string]any)
	if isMap && !isNamedDefinition(path) {
		if activePresent {
			compareValues(differences, workflow, path, presentMap, map[string]any{})
		} else {
			compareValues(differences, workflow, path, map[string]any{}, presentMap)
		}
		return
	}
	if activePresent {
		recordDifference(differences, workflow, path, present, missing{})
	} else {
		recordDifference(differences, workflow, path, missing{}, present)
	}
}

func isNamedDefinition(path []string) bool {
	if len(path) != 3 || path[0] != "spec" {
		return false
	}
	switch path[1] {
	case "tasks", "gates", "triggers":
		return true
	default:
		return false
	}
}

func appendPath(path []string, part string) []string {
	next := make([]string, len(path)+1)
	copy(next, path)
	next[len(path)] = part
	return next
}

func recordDifference(differences *[]Difference, workflow string, path []string, active, canonical any) {
	subjectKind, subject, field := describePath(path)
	severity := Error
	if isOperational(path) {
		severity = Informational
	}
	*differences = append(*differences, Difference{
		Severity:    severity,
		Workflow:    workflow,
		SubjectKind: subjectKind,
		Subject:     subject,
		Field:       field,
		Active:      formatValue(active),
		Canonical:   formatValue(canonical),
	})
}

func describePath(path []string) (subjectKind, subject, field string) {
	if len(path) >= 3 && path[0] == "spec" {
		switch path[1] {
		case "tasks":
			return "task", path[2], joinedField(path[3:])
		case "gates":
			return "gate", path[2], joinedField(path[3:])
		case "triggers":
			return "trigger", path[2], joinedField(path[3:])
		}
		return "", "", joinedField(path[1:])
	}
	return "", "", joinedField(path)
}

func joinedField(path []string) string {
	if len(path) == 0 {
		return "<definition>"
	}
	var out strings.Builder
	for index, part := range path {
		if strings.HasPrefix(part, "[") {
			out.WriteString(part)
			continue
		}
		if index > 0 {
			out.WriteByte('.')
		}
		out.WriteString(part)
	}
	return out.String()
}

func isOperational(path []string) bool {
	for _, field := range operationalFields {
		if len(path) != len(field.diffPath) {
			continue
		}
		matches := true
		for index, part := range field.diffPath {
			if part != "*" && part != path[index] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

type missing struct{}

func formatValue(value any) string {
	if _, ok := value.(missing); ok {
		return "<missing>"
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("<unrenderable: %v>", err)
	}
	return string(data)
}
