package engine

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

// TestStartInputVersionPinsEveryStartSpecField is the structural guard for one
// recurring bug class: a starter resolves a run's policy, puts it on the
// StartSpec, and the registry silently drops it on the way into RunInput, so
// the run pins a zero value nothing downstream ever re-reads.
//
// It has bitten three times — GateGooberCapabilities (#294/#3528, every gate
// resolving to no reviewer grants), RunControls (#3820, every run pinned to
// the built-in 45m/3 defaults) and now the backlog-query claim partition
// (#3873, a run claiming the sibling instance's items) and the goober digest
// (#3876, a run whose identity could not name the goober it walked). Each was
// a field that existed on one side of this function and not the other.
//
// The test is deliberately reflective rather than a field-by-field literal
// comparison: StartSpec and RunInput name these fields identically, so a NEW
// StartSpec field that StartInputVersion forgets to copy fails here the day it
// is added, without anyone remembering to extend an assertion list.
func TestStartInputVersionPinsEveryStartSpecField(t *testing.T) {
	// Every field non-zero, so "the registry dropped it" is distinguishable
	// from "the field happens to be zero on both sides".
	spec := StartSpec{
		RunID:                     "run-1",
		Gaggle:                    "web",
		RepoRef:                   apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:                      &apiv1.BacklogItem{ID: "42", Title: "an item"},
		TriggerRef:                "issue:42",
		TriggerKind:               "item",
		BranchNamespace:           "goobers/",
		GateGooberCapabilities:    map[string][]string{"reviewer": {"repo:read"}},
		LiveJournal:               true,
		Placements:                []PinnedPlacement{{Stage: "only", Self: true}},
		RunControls:               apiv1.RunControls{MaxRepasses: 7, MaxRunDuration: "6h", StalledRunTimeout: "90m"},
		BacklogQueryAssignedTo:    "goobersbot",
		BacklogQueryRequireLabels: "goobers:cloud",
		GooberDigest:              "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0",
		HITL:                      &HITLPolicy{Enabled: true, WaitSeconds: 3600, Actors: []string{"ops"}},
	}
	specValue := reflect.ValueOf(spec)
	for i := 0; i < specValue.NumField(); i++ {
		field := specValue.Type().Field(i)
		if specValue.Field(i).IsZero() {
			t.Fatalf("StartSpec field %s is zero in this fixture — populate it, or the guard cannot tell a dropped field from an unset one", field.Name)
		}
	}

	reg := NewRegistry()
	if _, err := reg.RegisterDefinition(startSpecPinDefinition()); err != nil {
		t.Fatalf("register definition: %v", err)
	}
	in, err := reg.StartInputVersion("pin", 1, spec)
	if err != nil {
		t.Fatalf("StartInputVersion: %v", err)
	}

	inputValue := reflect.ValueOf(in)
	for i := 0; i < specValue.NumField(); i++ {
		field := specValue.Type().Field(i)
		pinned := inputValue.FieldByName(field.Name)
		if !pinned.IsValid() {
			t.Fatalf("RunInput has no field named %s; StartSpec's %s must be pinned into the run input under some name — "+
				"if it is deliberately renamed, teach this guard about the mapping rather than deleting it", field.Name, field.Name)
		}
		if !reflect.DeepEqual(pinned.Interface(), specValue.Field(i).Interface()) {
			t.Fatalf("StartInputVersion dropped or altered StartSpec.%s: RunInput.%s = %#v, want %#v",
				field.Name, field.Name, pinned.Interface(), specValue.Field(i).Interface())
		}
	}
}

func startSpecPinDefinition() wf.Definition {
	return wf.Definition{
		Name:    "pin",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "only",
			Tasks: []apiv1.Task{{
				Name: "only",
				Type: apiv1.TaskDeterministic,
				Goal: "pin the start input",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			}},
		},
	}
}
