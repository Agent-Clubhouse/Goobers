package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/validate"
)

func testChildLineage(id RunIdentity) *ChildLineage {
	return &ChildLineage{Gaggle: id.Gaggle, ParentRunID: strings.Repeat("b", 32), ParentWorkflow: "parent", StageOccurrence: "stage/0/visit/1", InvocationKey: "first-child", AcceptanceID: "trigger-" + id.RunID, SourceDigest: Digest([]byte("source")), EnvelopeDigest: Digest([]byte("envelope"))}
}

func childTestIdentity() RunIdentity {
	id := testIdentity()
	id.ConfigGeneration = Digest([]byte("config"))
	id.GooberDigest = Digest([]byte("goobers"))
	id.Child = testChildLineage(id)
	return id
}

func TestChildLineageRejectsInvalidBeforeCreatingJournal(t *testing.T) {
	cases := map[string]func(*RunIdentity){
		"gaggle":       func(id *RunIdentity) { id.Child.Gaggle = "another" },
		"parent":       func(id *RunIdentity) { id.Child.ParentRunID = id.RunID },
		"acceptance":   func(id *RunIdentity) { id.Child.AcceptanceID = "trigger-" + strings.Repeat("c", 32) },
		"source":       func(id *RunIdentity) { id.Child.SourceDigest = "mutable-name" },
		"envelope":     func(id *RunIdentity) { id.Child.EnvelopeDigest = "" },
		"generation":   func(id *RunIdentity) { id.ConfigGeneration = "" },
		"occurrence":   func(id *RunIdentity) { id.Child.StageOccurrence = "stage\n1" },
		"key":          func(id *RunIdentity) { id.Child.InvocationKey = strings.Repeat("x", 257) },
		"continuation": func(id *RunIdentity) { id.ContinuedFromRunID = strings.Repeat("c", 32) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			id := childTestIdentity()
			mutate(&id)
			root := filepath.Join(t.TempDir(), "runs")
			if run, err := Create(root, id, nil); err == nil {
				_ = run.Close()
				t.Fatal("invalid child identity accepted")
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("invalid identity created filesystem state: %v", err)
			}
		})
	}
}

func TestChildLineageRoundTripAndTamper(t *testing.T) {
	id := childTestIdentity()
	root := t.TempDir()
	run, err := Create(root, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, id.RunID)
	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Identity()
	if err != nil || got.Child == nil || *got.Child != *id.Child {
		t.Fatalf("lineage: %+v %v", got.Child, err)
	}
	got.Child.Gaggle = "another"
	raw, err := yaml.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, fileRunYAML), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = reader.Identity(); err == nil {
		t.Fatal("reader accepted cross-gaggle lineage")
	}
}

func TestChildLineageSchemaIsClosed(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	id := childTestIdentity()
	id.Schema = RunSchema
	raw, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["child"].(map[string]any)["source"] = "do not embed authored source"
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.ValidateJSON("journal-run.schema.json", raw); err == nil {
		t.Fatal("schema accepted undeclared child source")
	}
}
