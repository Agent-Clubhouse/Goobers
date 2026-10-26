package supportmatrix

import "testing"

func TestDSL21PreviewDoesNotRewriteStableHistory(t *testing.T) {
	preview, ok := GetDSL().Lookup(V21DSLVersion)
	if !ok || preview.Level != LevelPreview || len(preview.History) != 1 || preview.History[0].SinceVersion != "v0.6.0" {
		t.Fatalf("DSL 2.1 = %+v, %v", preview, ok)
	}
	for _, release := range []string{"v0.4.0", "v0.5.0"} {
		matrix, err := GetDSLForRelease(release)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := matrix.Lookup(V21DSLVersion); ok {
			t.Fatalf("%s retroactively declares 2.1", release)
		}
	}
	matrix, err := GetDSLForRelease("v0.6.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := matrix.Lookup(V21DSLVersion); !ok {
		t.Fatal("v0.6.0 omits 2.1")
	}
	if err := ValidateSupportPolicyForRelease(matrix, "v0.6.0"); err != nil {
		t.Fatal(err)
	}
	if got, ok := matrix.NewestSupported(); !ok || got != V2DSLVersion {
		t.Fatalf("default = %q, %v", got, ok)
	}
}
