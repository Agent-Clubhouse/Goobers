package validate

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestWorkbenchSourceSchemaAndRuntimeScope(t *testing.T) {
	var document map[string]any
	err := json.Unmarshal([]byte(`{"apiVersion":"goobers.dev/v1alpha1","kind":"Gaggle","metadata":{"name":"own"},"spec":{"project":{"provider":"github","owner":"acme","name":"code"},"backlog":{"provider":"github","project":"acme/items"},"isolation":{"namespace":"own"},"workbench":{"schemaVersion":"sources/v1","sources":[{"name":"backlog","kind":"backlog","objectives":{"labels":["objective"]}},{"name":"strategy","kind":"documents","repository":{"provider":"github","owner":"acme","name":"code"},"paths":["strategy.md"],"writes":{"fields":["title","description"],"relationships":["references"]}}]}}}`), &document)
	if err != nil {
		t.Fatal(err)
	}
	sources := document["spec"].(map[string]any)["workbench"].(map[string]any)["sources"].([]any)
	writes := sources[1].(map[string]any)["writes"].(map[string]any)
	writes["metadata"] = []string{"assign-objective"}
	raw, _ := json.Marshal(document)
	if err = newV(t).ValidateJSON("gaggle.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	var g apiv1.Gaggle
	if err = json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	ix := newIndex()
	ix.gaggles[g.Name] = g
	r := &Report{}
	ix.checkWorkbenchSources(r)
	if len(r.Issues) != 0 {
		t.Fatal(r.Issues)
	}
	g.Spec.Workbench.Sources[1].Repository.Name = "foreign"
	ix.gaggles[g.Name] = g
	r = &Report{}
	ix.checkWorkbenchSources(r)
	if len(r.Issues) != 1 || r.Issues[0].Code != errorWorkbenchSources {
		t.Fatal(r.Issues)
	}
	for _, invalid := range [][]string{{"unknown"}, {"assign-objective", "assign-objective"}, {"assign-objective", "aliases", "unknown"}} {
		writes["metadata"] = invalid
		candidate, _ := json.Marshal(document)
		if err := newV(t).ValidateJSON("gaggle.schema.json", candidate); err == nil {
			t.Fatalf("invalid metadata operations accepted: %v", invalid)
		}
	}
	writes["metadata"] = []string{"assign-objective"}
	cases := map[string]func(map[string]any){
		"branch":                   func(s map[string]any) { s["branch"] = "unreviewed" },
		"credential":               func(s map[string]any) { s["credentialRef"] = "automation" },
		"gaggle":                   func(s map[string]any) { s["gaggleId"] = "foreign" },
		"path on backlog":          func(s map[string]any) { s["paths"] = []string{"strategy.md"} },
		"empty objective selector": func(s map[string]any) { s["objectives"] = map[string]any{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			original := sources[0]
			copy := map[string]any{}
			for k, v := range original.(map[string]any) {
				copy[k] = v
			}
			mutate(copy)
			sources[0] = copy
			defer func() { sources[0] = original }()
			candidate, _ := json.Marshal(document)
			if err := newV(t).ValidateJSON("gaggle.schema.json", candidate); err == nil {
				t.Fatal("invalid authority/source field accepted")
			}
		})
	}
}
