package vcurrent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestGoldenCompiledMachineDigests(t *testing.T) {
	goldenDir := filepath.Join("testdata", "golden")
	raw, err := os.ReadFile(filepath.Join(goldenDir, "digests.json"))
	if err != nil {
		t.Fatalf("read golden digests: %v", err)
	}
	var digests map[string]string
	if err := json.Unmarshal(raw, &digests); err != nil {
		t.Fatalf("decode golden digests: %v", err)
	}
	fixtures, err := filepath.Glob(filepath.Join(goldenDir, "*.yaml"))
	if err != nil {
		t.Fatalf("list golden fixtures: %v", err)
	}
	if len(fixtures) != len(digests) {
		t.Fatalf("golden fixture count = %d, digest count = %d", len(fixtures), len(digests))
	}
	for _, fixture := range fixtures {
		if _, ok := digests[filepath.Base(fixture)]; !ok {
			t.Fatalf("fixture %q has no frozen digest", filepath.Base(fixture))
		}
	}

	for file, want := range digests {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(goldenDir, file))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var parsed apiv1.Workflow
			if err := yaml.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			machine, err := Compile(Definition{
				Name:       parsed.Name,
				Version:    1,
				DSLVersion: parsed.DSLVersion,
				Spec:       parsed.Spec,
			}, WithPreviewFeatures(true))
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if got := machine.Digest(); got != want {
				t.Fatalf("compiled digest drift:\n got  %s\n want %s", got, want)
			}
		})
	}
}
