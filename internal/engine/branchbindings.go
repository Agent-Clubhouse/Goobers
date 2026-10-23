package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
)

// fanOutBindings is the inseparable value/provenance result of an adapter.
// Adapters read frozen workflow state only; retries never refetch a roster.
type fanOutBindings struct {
	Values map[string]any
	Grades map[string]apiv1.Integrity
}

func (r *runJournal) branchBindings(stage string) (fanOutBindings, error) {
	if r.branchWalk == nil || r.branchWalk.bind == nil {
		return fanOutBindings{}, nil
	}
	bound, err := r.branchWalk.bind(stage)
	if err != nil {
		return fanOutBindings{}, err
	}
	if len(bound.Values) != len(bound.Grades) {
		return fanOutBindings{}, fmt.Errorf("engine: branch binding values and grades differ")
	}
	for _, key := range slices.Sorted(maps.Keys(bound.Values)) {
		if !bound.Grades[key].Valid() {
			return fanOutBindings{}, fmt.Errorf("engine: branch binding %q has no valid provenance", key)
		}
		switch key {
		case runner.BranchCompletenessInput, runner.MapCompletenessInput, runner.MapResultsInput:
			return fanOutBindings{}, fmt.Errorf("engine: branch binding %q is host-owned", key)
		}
	}
	// JSON is the invocation value contract. Decode a fresh object per dispatch
	// with exact numbers so an adapter cannot share mutable item state.
	data, err := json.Marshal(bound.Values)
	if err != nil {
		return fanOutBindings{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	bound.Values = nil
	if err := decoder.Decode(&bound.Values); err != nil {
		return fanOutBindings{}, err
	}
	bound.Grades = maps.Clone(bound.Grades)
	return bound, nil
}

func (b fanOutBindings) addGrades(grades map[string]apiv1.Integrity) map[string]apiv1.Integrity {
	if len(b.Grades) == 0 {
		return grades
	}
	if grades == nil {
		grades = make(map[string]apiv1.Integrity)
	}
	maps.Copy(grades, b.Grades)
	return grades
}

func (b fanOutBindings) apply(inputs map[string]any) {
	maps.Copy(inputs, b.Values)
}
