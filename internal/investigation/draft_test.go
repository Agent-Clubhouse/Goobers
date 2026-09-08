package investigation

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/journal"
)

func draftFixture(t *testing.T) (map[string]any, []apiv1.ContextPointer, fixtureReader, Evidence) {
	t.Helper()
	evidence, original, reader := evidenceFixture()
	stages := map[string][]artifactset.Entry{}
	byPath := map[string]string{}
	for _, cp := range original {
		stage := strings.Split(cp.Name, ".artifact[")[0]
		byPath[cp.Artifact.Path] = stage
		stages[stage] = append(stages[stage], artifactset.Entry{Name: path.Base(cp.Artifact.Path), Artifact: *cp.Artifact})
	}
	var pointers []apiv1.ContextPointer
	for stage, entries := range stages {
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		for i := range entries {
			entries[i].Slot = i + 1
			pointers = append(pointers, apiv1.ContextPointer{Name: fmt.Sprintf("%s.artifact[%d]", stage, i+1), Artifact: &entries[i].Artifact})
		}
		data, err := json.Marshal(artifactset.Index{SchemaVersion: artifactset.SchemaVersion, Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
		p := apiv1.ArtifactPointer{Path: "artifacts/" + stage + "-index", Digest: apiv1.Digest(data), Size: int64(len(data)), MediaType: "application/json"}
		reader[p.Digest] = data
		pointers = append(pointers, apiv1.ContextPointer{Name: stage + ".artifact[0]", Artifact: &p})
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	var draft map[string]any
	if err := json.Unmarshal(data, &draft); err != nil {
		t.Fatal(err)
	}
	var convert func(any) any
	convert = func(value any) any {
		switch v := value.(type) {
		case map[string]any:
			if p, ok := v["path"].(string); ok && byPath[p] != "" {
				return map[string]any{"producerStage": byPath[p], "name": path.Base(p)}
			}
			for key, child := range v {
				v[key] = convert(child)
			}
		case []any:
			for i, child := range v {
				v[i] = convert(child)
			}
		}
		return value
	}
	convert(draft)
	draft["schemaVersion"] = DraftSchemaVersion
	return draft, pointers, reader, evidence
}

func TestPrepareDraftResolvesRunnerPointers(t *testing.T) {
	draft, pointers, reader, expected := draftFixture(t)
	data, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := PrepareDraft(context.Background(), data, pointers, reader, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	var got Evidence
	if err := json.Unmarshal(clean, &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != SchemaVersion || got.Reproduction.Harness != expected.Reproduction.Harness || got.Diagnosis.Evidence[0].Artifact != expected.Diagnosis.Evidence[0].Artifact || got.Validation.Result != expected.Validation.Result {
		t.Fatalf("canonical evidence lost verified pointers: %+v", got)
	}
}

func TestPrepareDraftRejectsUntrustedReferences(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any, []apiv1.ContextPointer, fixtureReader){
		"predicted pointer": func(d map[string]any, p []apiv1.ContextPointer, _ fixtureReader) {
			d["reproduction"].(map[string]any)["harness"] = p[0].Artifact
		},
		"unknown name": func(d map[string]any, _ []apiv1.ContextPointer, _ fixtureReader) {
			d["reproduction"].(map[string]any)["harness"].(map[string]any)["name"] = "absent"
		},
		"unknown producer": func(d map[string]any, _ []apiv1.ContextPointer, _ fixtureReader) {
			d["reproduction"].(map[string]any)["harness"].(map[string]any)["producerStage"] = "foreign"
		},
		"extra field": func(d map[string]any, _ []apiv1.ContextPointer, _ fixtureReader) { d["unknown"] = true },
		"tampered payload": func(_ map[string]any, p []apiv1.ContextPointer, r fixtureReader) {
			r[p[0].Artifact.Digest] = []byte("tampered")
		},
		"cross run": func(_ map[string]any, p []apiv1.ContextPointer, _ fixtureReader) { p[0].RunID = "foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			draft, pointers, reader, _ := draftFixture(t)
			mutate(draft, pointers, reader)
			data, err := json.Marshal(draft)
			if err != nil {
				t.Fatal(err)
			}
			if clean, err := PrepareDraft(context.Background(), data, pointers, reader, journal.NewRegistryScrubber()); err == nil || clean != nil {
				t.Fatalf("invalid draft produced evidence: %s, %v", clean, err)
			}
		})
	}
}
