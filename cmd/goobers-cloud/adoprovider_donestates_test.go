package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestStageGaggleBacklogStampsCoverEveryGaggle(t *testing.T) {
	strict := &apiv1.BacklogDoneStates{Categories: []apiv1.BacklogStateCategory{"Completed"}}
	gaggles := []apiv1.Gaggle{
		{Spec: apiv1.GaggleSpec{Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "example-backlog", Labels: []string{"team-a"}, DoneStates: strict, FieldPredicate: "not delivered"}}},
		{},
	}
	gaggles[0].Name, gaggles[1].Name = "ado", "plain"
	stamps, err := stageGaggleBacklogStamps(gaggles)
	if err != nil {
		t.Fatal(err)
	}
	if stamps["plain"] != "{}" {
		t.Fatalf("stamp for a gaggle without settings = %q, want %q", stamps["plain"], "{}")
	}
	var decoded runtimewiring.StageBacklogSettings
	if err := json.Unmarshal([]byte(stamps["ado"]), &decoded); err != nil {
		t.Fatal(err)
	}
	want := runtimewiring.StageBacklogSettings{Provider: apiv1.ProviderADO, Project: "example-backlog", Labels: []string{"team-a"}, DoneStates: strict}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded stamp = %+v, want %+v", decoded, want)
	}
	if strings.Contains(stamps["ado"], "not delivered") {
		t.Fatalf("stamp %q carries backlog configuration a stage does not apply", stamps["ado"])
	}
}
