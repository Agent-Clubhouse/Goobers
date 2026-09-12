package providerstage

import (
	"cmp"
	"slices"
)

// InputType describes the value shape a built-in provider stage reads from
// Task.Inputs. It is metadata for authoring and validation; runtime parsing
// remains the final defensive boundary for values that arrive in an envelope.
type InputType string

const (
	InputString     InputType = "string"
	InputBoolean    InputType = "boolean"
	InputInteger    InputType = "integer"
	InputDuration   InputType = "duration"
	InputStringList InputType = "string-list"
	InputPath       InputType = "path"
)

// InputState records whether a declared provider-stage input can still be
// used. Retired inputs remain in the registry permanently so configuration
// validation can give an upgrade instruction instead of allowing a runtime
// failure.
type InputState string

const (
	InputCurrent InputState = "current"
	InputRetired InputState = "retired"
)

// Input describes one workflow-supplied input consumed by a built-in provider
// stage. SinceDSL and UntilDSL are optional DSL-version bounds for the
// configuration surface. RetiredSince is a release or date recording a
// retirement that applies to every supported DSL version; Replacement must
// explain how an author should migrate.
type Input struct {
	Name         string
	Type         InputType
	State        InputState
	SinceDSL     string
	UntilDSL     string
	RetiredSince string
	Replacement  string
}

// inputSchemas is intentionally additive. Unknown inputs remain permitted:
// this registry starts with backlog-query, whose runtime-only retirement
// motivated it, and can grow alongside other provider-stage contracts without
// turning an incomplete inventory into a breaking closed schema.
var inputSchemas = map[string][]Input{
	"backlog-query": {
		{Name: "assignedTo", Type: InputString, State: InputCurrent},
		{Name: "contestedFileMinPRs", Type: InputInteger, State: InputCurrent},
		{Name: "curation", Type: InputBoolean, State: InputCurrent},
		{Name: "deprioritizeContestedFiles", Type: InputBoolean, State: InputCurrent},
		{Name: "excludeLabels", Type: InputStringList, State: InputCurrent},
		{Name: "fieldOrder", Type: InputString, State: InputCurrent},
		{Name: "fieldPredicate", Type: InputString, State: InputCurrent},
		{Name: "filterParkLabels", Type: InputBoolean, State: InputCurrent},
		{Name: "labelPredicate", Type: InputString, State: InputCurrent},
		{Name: "leaseDuration", Type: InputDuration, State: InputCurrent},
		{Name: "maxItems", Type: InputInteger, State: InputCurrent},
		{Name: "parkLabels", Type: InputStringList, State: InputCurrent},
		{Name: "requireLabels", Type: InputStringList, State: InputCurrent},
		{Name: "respectAssignee", Type: InputBoolean, State: InputCurrent},
		{Name: "resultFile", Type: InputPath, State: InputCurrent},
		{
			Name:         "resweepInterval",
			Type:         InputDuration,
			State:        InputRetired,
			RetiredSince: "2026-09-08",
			Replacement:  "configure schedule and readiness on a separate workflow using backlog-query --claim --resweep",
		},
		{Name: "resweepMaxItems", Type: InputInteger, State: InputCurrent},
		{Name: "resweepReadyLabel", Type: InputString, State: InputCurrent},
		{Name: "selectionPriority", Type: InputStringList, State: InputCurrent},
		{Name: "staleAfterDays", Type: InputInteger, State: InputCurrent},
		{Name: "staleAutoClose", Type: InputBoolean, State: InputCurrent},
		{Name: "trustLabel", Type: InputString, State: InputCurrent},
	},
}

// Inputs returns the declared workflow-input schema for command. The result is
// a copy and is sorted by name so validation and authoring output stay stable.
func Inputs(command string) []Input {
	inputs := append([]Input(nil), inputSchemas[command]...)
	slices.SortFunc(inputs, func(a, b Input) int { return cmp.Compare(a.Name, b.Name) })
	return inputs
}

// InputsForVersion resolves command's declared inputs at one DSL version.
// Unbounded entries apply to every version; bounded entries use the same
// inclusive-since/exclusive-until semantics as capability requirements.
func InputsForVersion(command, dslVersion string) []Input {
	var inputs []Input
	for _, input := range inputSchemas[command] {
		if activeAt(dslVersion, input.SinceDSL, input.UntilDSL) {
			inputs = append(inputs, input)
		}
	}
	slices.SortFunc(inputs, func(a, b Input) int { return cmp.Compare(a.Name, b.Name) })
	return inputs
}
