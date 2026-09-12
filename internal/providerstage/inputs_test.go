package providerstage

import (
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/builtincmd"
)

func TestBacklogQueryInputsDeclareCurrentAndRetiredSchema(t *testing.T) {
	inputs := knownInputSchema(t, "backlog-query", "3.0")
	for _, input := range inputs {
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

// TestBuiltInCommandsHaveInputSchemas is the structural completeness guard:
// the workflow-callable built-in inventory drives the assertion, so adding a
// command fails until its input contract is declared (even when currently
// empty). Unknown external executables remain intentionally outside it.
func TestBuiltInCommandsHaveInputSchemas(t *testing.T) {
	for _, command := range builtincmd.Names() {
		if _, ok := inputSchemas[command]; !ok {
			t.Errorf("workflow-callable built-in %q has no declared input schema", command)
		}
	}
	for command := range inputSchemas {
		if !builtincmd.Known(command) {
			t.Errorf("input schema %q has no workflow-callable built-in command", command)
		}
	}
}

func TestEveryBuiltInSchemaIncludesExecutorInputs(t *testing.T) {
	for _, command := range builtincmd.Names() {
		for _, name := range []string{"maxOutputBytes", "timeout"} {
			if !slices.ContainsFunc(knownInputSchema(t, command, "3.0"), func(input Input) bool { return input.Name == name }) {
				t.Errorf("effective schema for %q lacks executor-wide input %q", command, name)
			}
		}
	}
}

func TestProviderInputSchemasAreWellFormed(t *testing.T) {
	for command := range inputSchemas {
		for _, version := range []string{"2.0", "3.0"} {
			seen := map[string]bool{}
			for _, input := range knownInputSchema(t, command, version) {
				if strings.TrimSpace(input.Name) == "" || input.Type == "" {
					t.Errorf("%s at DSL %s has incomplete input metadata: %+v", command, version, input)
				}
				if seen[input.Name] {
					t.Errorf("%s at DSL %s declares input %q more than once", command, version, input.Name)
				}
				seen[input.Name] = true
				switch input.State {
				case InputCurrent:
					if input.RetiredSince != "" || input.Replacement != "" {
						t.Errorf("current input %s/%s carries retirement metadata", command, input.Name)
					}
				case InputRetired:
					if strings.TrimSpace(input.RetiredSince) == "" || strings.TrimSpace(input.Replacement) == "" {
						t.Errorf("retired input %s/%s lacks actionable metadata", command, input.Name)
					}
				default:
					t.Errorf("input %s/%s has invalid state %q", command, input.Name, input.State)
				}
			}
		}
	}
}

func TestInputSchemaForVersionReturnsCopyAndUnknownCommandIsOpen(t *testing.T) {
	first := knownInputSchema(t, "backlog-query", "3.0")
	first[0].Name = "mutated"
	if got := knownInputSchema(t, "backlog-query", "3.0")[0].Name; got == "mutated" {
		t.Fatal("InputSchemaForVersion returned mutable registry storage")
	}
	if got, known := InputSchemaForVersion("unknown", "3.0"); known || got != nil {
		t.Fatalf("InputSchemaForVersion(unknown) = (%v, %t), want (nil, false)", got, known)
	}
}

func TestInputSchemaForVersionHonorsDSLWindow(t *testing.T) {
	const command = "test-versioned-input"
	inputSchemas[command] = []Input{
		{Name: "baseline", Type: InputString, State: InputCurrent},
		{Name: "v3-only", Type: InputString, State: InputCurrent, SinceDSL: "3.0"},
		{Name: "transitioned", Type: InputString, State: InputCurrent, UntilDSL: "3.0"},
		{Name: "transitioned", Type: InputString, State: InputRetired, SinceDSL: "3.0", RetiredSince: "test", Replacement: "replace it"},
	}
	t.Cleanup(func() { delete(inputSchemas, command) })

	v2 := knownInputSchema(t, command, "2.0")
	if got := inputNames(v2); !slices.Equal(got, []string{"baseline", "maxOutputBytes", "timeout", "transitioned"}) || v2[3].State != InputCurrent {
		t.Fatalf("2.0 inputs = %q", got)
	}
	v3 := knownInputSchema(t, command, "3.0")
	if got := inputNames(v3); !slices.Equal(got, []string{"baseline", "maxOutputBytes", "timeout", "transitioned", "v3-only"}) || v3[3].State != InputRetired {
		t.Fatalf("3.0 inputs = %q", got)
	}
}

func knownInputSchema(t *testing.T, command, dslVersion string) []Input {
	t.Helper()
	inputs, known := InputSchemaForVersion(command, dslVersion)
	if !known {
		t.Fatalf("InputSchemaForVersion(%q, %q) reports unknown command", command, dslVersion)
	}
	return inputs
}

func inputNames(inputs []Input) []string {
	names := make([]string, 0, len(inputs))
	for _, input := range inputs {
		names = append(names, input.Name)
	}
	return names
}
