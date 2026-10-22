package fanout

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestFreezeSelectionNormalizesOptionalBranchesInDeclarationOrder(t *testing.T) {
	def, source := selectionFixture()
	data := []byte(`{"schemaVersion":1,"branches":[{"name":"optional-b","reason":"needed"},{"name":"required","reason":"ignored"},{"name":"optional-a"}]}`)
	source.Digest, source.Size = journal.Digest(data), int64(len(data))
	got, err := FreezeSelection(def, source, data, MaxSourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source.Task != "plan" || got.Source.Slot != "selection" ||
		got.Source.Visit != 5 || got.Source.Attempt != 2 ||
		got.SourceDigest != source.Digest || got.Integrity != apiv1.IntegrityMaintainer {
		t.Fatalf("source metadata = %+v", got)
	}
	if strings.Join(got.Selected, ",") != "optional-a,optional-b" {
		t.Fatalf("selected = %v, want declaration order", got.Selected)
	}
	if len(got.Reasons) != 1 || got.Reasons["optional-b"] != "needed" {
		t.Fatalf("reasons = %v", got.Reasons)
	}
}

func TestFreezeSelectionRejectsInvalidSourceOrDocument(t *testing.T) {
	def, source := selectionFixture()
	valid := `{"schemaVersion":1,"branches":[{"name":"optional-a"}]}`
	tests := map[string]struct {
		data   string
		mutate func(*apiv1.Parallel, *journal.FanOutSourceRef)
	}{
		"no declaration": {data: valid, mutate: func(def *apiv1.Parallel, _ *journal.FanOutSourceRef) { def.Selection = nil }},
		"source":         {data: valid, mutate: func(_ *apiv1.Parallel, source *journal.FanOutSourceRef) { source.Task = "other" }},
		"visit":          {data: valid, mutate: func(_ *apiv1.Parallel, source *journal.FanOutSourceRef) { source.Visit = 0 }},
		"attempt overflow": {data: valid, mutate: func(_ *apiv1.Parallel, source *journal.FanOutSourceRef) {
			source.Attempt = int(^uint32(0)>>1) + 1
		}},
		"media type": {data: valid, mutate: func(_ *apiv1.Parallel, source *journal.FanOutSourceRef) { source.MediaType = "text/plain" }},
		"size":       {data: valid, mutate: func(_ *apiv1.Parallel, source *journal.FanOutSourceRef) { source.Size++ }},
		"digest": {data: valid, mutate: func(_ *apiv1.Parallel, source *journal.FanOutSourceRef) {
			source.Digest = journal.Digest([]byte("other"))
		}},
		"unknown":      {data: `{"schemaVersion":1,"branches":[{"name":"missing"}]}`},
		"duplicate":    {data: `{"schemaVersion":1,"branches":[{"name":"optional-a"},{"name":"optional-a"}]}`},
		"reason bound": {data: `{"schemaVersion":1,"branches":[{"name":"optional-a","reason":"` + strings.Repeat("x", MaxSelectionReasonBytes+1) + `"}]}`},
		"extra field":  {data: `{"schemaVersion":1,"branches":[],"extra":true}`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gotDef, gotSource := def, source
			data := []byte(tc.data)
			gotSource.Digest, gotSource.Size = journal.Digest(data), int64(len(data))
			if tc.mutate != nil {
				tc.mutate(&gotDef, &gotSource)
			}
			if _, err := FreezeSelection(gotDef, gotSource, data, MaxSourceBytes); err == nil {
				t.Fatal("FreezeSelection accepted invalid input")
			}
		})
	}
}

func TestFreezeSelectionDefaultsUnknownIntegrityToDerived(t *testing.T) {
	def, source := selectionFixture()
	data := []byte(`{"schemaVersion":1,"branches":[]}`)
	source.Digest, source.Size, source.Integrity = journal.Digest(data), int64(len(data)), ""
	got, err := FreezeSelection(def, source, data, MaxSourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got.Integrity != apiv1.IntegrityDerived || got.Selected == nil || len(got.Selected) != 0 {
		t.Fatalf("selection = %+v", got)
	}
}

func selectionFixture() (apiv1.Parallel, journal.FanOutSourceRef) {
	def := apiv1.Parallel{
		Name: "review", FailurePolicy: apiv1.BranchContinueOnError, Join: "collate",
		Selection: &apiv1.ParallelSelection{Source: apiv1.FanOutSource{Task: "plan", Slot: "selection"}},
		Branches: []apiv1.Branch{
			{Name: "required", Start: "required"},
			{Name: "optional-a", Start: "a", Optional: true},
			{Name: "optional-b", Start: "b", Optional: true},
		},
	}
	source := journal.FanOutSourceRef{
		Task: "plan", Slot: "selection", Visit: 5, Attempt: 2,
		Integrity: apiv1.IntegrityMaintainer, Path: "artifacts/selection.json",
		MediaType: SourceMediaType,
	}
	return def, source
}
