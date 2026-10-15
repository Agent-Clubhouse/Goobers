package childworkflow

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParentAuthoringCatalogCarriesConstructGuide pins the construct-selection
// guide (plan-driven-dynamic-fan-out.md §3) the catalog projects, against a
// golden, and its availability under each preview policy.
func TestParentAuthoringCatalogCarriesConstructGuide(t *testing.T) {
	for _, preview := range []bool{false, true} {
		a, kit := authoringFixture()
		a.Admission.AllowPreviewFeatures = preview
		if err := AttachParentAuthoringSkill(kit, a); err != nil {
			t.Fatal(err)
		}
		catalog, _ := catalogFromKit(t, kit)
		if catalog.PreviewFeatures != preview || len(catalog.Constructs) != 6 {
			t.Fatalf("preview=%v: catalog guide = %+v", preview, catalog)
		}
		for _, row := range catalog.Constructs {
			want := !strings.HasPrefix(row.Construct, "child workflow") && (!row.Preview || preview)
			if row.Available != want || (row.Unavailable == "") == !want {
				t.Errorf("preview=%v: %s available=%v (%q), want %v", preview, row.Construct, row.Available, row.Unavailable, want)
			}
		}
		if !preview {
			continue
		}
		got, err := json.MarshalIndent(catalog.Constructs, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		path := filepath.Join("testdata", "authoring-constructs.golden.json")
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("construct guide differs from %s; regenerate with UPDATE_GOLDEN=1 go test ./internal/childworkflow -run TestParentAuthoringCatalogCarriesConstructGuide\n%s", path, got)
		}
	}
}

func TestParentAuthoringSkillCarriesConstructGuide(t *testing.T) {
	for _, want := range []string{
		"## Choose the cheapest construct",
		"| One path out of several | Gate `branches` |",
		"| A fixed set of branches, all run | Static `parallel` |",
		"Selective `parallel` (`selection`, `optional: true`)",
		"| The same task over a data-determined list | `map` |",
		"| A composition nobody can know until run time | Child workflow |",
		"| Skip the whole fan-out | Gate before the parallel or map |",
		"`previewFeatures` is true",
	} {
		if !strings.Contains(parentAuthoringSkill, want) {
			t.Errorf("authoring skill omits %q", want)
		}
	}
}
