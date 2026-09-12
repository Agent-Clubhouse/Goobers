package providerstage

import (
	"slices"
	"strings"
	"testing"
)

func TestBacklogQueryInputsDeclareCurrentAndRetiredSchema(t *testing.T) {
	inputs := Inputs("backlog-query")
	wantNames := []string{
		"assignedTo", "contestedFileMinPRs", "curation", "deprioritizeContestedFiles",
		"excludeLabels", "fieldOrder", "fieldPredicate", "filterParkLabels",
		"labelPredicate", "leaseDuration", "maxItems", "parkLabels", "requireLabels",
		"respectAssignee", "resultFile", "resweepInterval", "resweepMaxItems",
		"resweepReadyLabel", "selectionPriority", "staleAfterDays", "staleAutoClose",
		"trustLabel",
	}
	gotNames := make([]string, 0, len(inputs))
	for _, input := range inputs {
		gotNames = append(gotNames, input.Name)
		if input.Type == "" {
			t.Errorf("input %q has no type", input.Name)
		}
		if input.State != InputCurrent && input.State != InputRetired {
			t.Errorf("input %q has invalid state %q", input.Name, input.State)
		}
		if input.State == InputRetired &&
			(strings.TrimSpace(input.RetiredSince) == "" || strings.TrimSpace(input.Replacement) == "") {
			t.Errorf("retired input %q lacks retirement or replacement metadata", input.Name)
		}
	}
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("backlog-query input names = %q, want %q", gotNames, wantNames)
	}

	index := slices.IndexFunc(inputs, func(input Input) bool { return input.Name == "resweepInterval" })
	if index < 0 {
		t.Fatal("resweepInterval is missing from backlog-query input schema")
	}
	retired := inputs[index]
	if retired.State != InputRetired || retired.Type != InputDuration ||
		retired.RetiredSince != "2026-09-08" || !strings.Contains(retired.Replacement, "--claim --resweep") {
		t.Fatalf("resweepInterval metadata = %+v", retired)
	}
}

func TestInputsReturnsCopyAndUnknownCommandIsOpen(t *testing.T) {
	first := Inputs("backlog-query")
	first[0].Name = "mutated"
	if got := Inputs("backlog-query")[0].Name; got == "mutated" {
		t.Fatal("Inputs returned mutable registry storage")
	}
	if got := Inputs("unknown"); got != nil {
		t.Fatalf("Inputs(unknown) = %v, want nil", got)
	}
}

func TestInputsForVersionHonorsDSLWindow(t *testing.T) {
	const command = "test-versioned-input"
	inputSchemas[command] = []Input{
		{Name: "baseline", Type: InputString, State: InputCurrent},
		{Name: "v3-only", Type: InputString, State: InputCurrent, SinceDSL: "3.0"},
		{Name: "transitioned", Type: InputString, State: InputCurrent, UntilDSL: "3.0"},
		{Name: "transitioned", Type: InputString, State: InputRetired, SinceDSL: "3.0", RetiredSince: "test", Replacement: "replace it"},
	}
	t.Cleanup(func() { delete(inputSchemas, command) })

	v2 := InputsForVersion(command, "2.0")
	if got := inputNames(v2); !slices.Equal(got, []string{"baseline", "transitioned"}) || v2[1].State != InputCurrent {
		t.Fatalf("2.0 inputs = %q", got)
	}
	v3 := InputsForVersion(command, "3.0")
	if got := inputNames(v3); !slices.Equal(got, []string{"baseline", "transitioned", "v3-only"}) || v3[1].State != InputRetired {
		t.Fatalf("3.0 inputs = %q", got)
	}
}

func inputNames(inputs []Input) []string {
	names := make([]string, 0, len(inputs))
	for _, input := range inputs {
		names = append(names, input.Name)
	}
	return names
}
