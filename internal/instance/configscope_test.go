package instance

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
)

func TestDescribeInvalidConfigScopeNamesFaultyAndBlockedFiles(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"manifest.yaml":                       "kind: Manifest\nmetadata:\n  name: inst\n",
		"gaggles/g/gaggle.yaml":               "kind: Gaggle\nmetadata:\n  name: g\n",
		"gaggles/g/workflows/good.yaml":       "kind: Workflow\nmetadata:\n  name: good\n---\nkind: Workflow\nmetadata:\n  name: good-two\n",
		"gaggles/g/workflows/bad.yaml":        "kind: Workflow\nmetadata:\n  name: bad\n",
		"gaggles/g/workflows/warned.yaml":     "kind: Workflow\nmetadata:\n  name: warned\n",
		"gaggles/g/workflows/notes/README.md": "not a definition\n",
	}
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	report := &validate.Report{Issues: []validate.Issue{
		{Severity: validate.Error, File: "gaggles/g/workflows/bad.yaml", Message: `state "x" is unreachable from start "a"`},
		{Severity: validate.Error, File: "gaggles/g/workflows/bad.yaml", Message: "second finding"},
		{Severity: validate.Warning, File: "gaggles/g/workflows/warned.yaml", Message: "only a warning"},
		{Severity: validate.Error, Message: "no Manifest object found"},
	}}

	scope, err := DescribeInvalidConfigScope(dir, report)
	if err != nil {
		t.Fatalf("DescribeInvalidConfigScope: %v", err)
	}
	if want := []string{"gaggles/g/workflows/bad.yaml"}; !reflect.DeepEqual(scope.InvalidFiles, want) {
		t.Fatalf("InvalidFiles = %v, want %v", scope.InvalidFiles, want)
	}
	if scope.UnattributedErrors != 1 {
		t.Fatalf("UnattributedErrors = %d, want 1", scope.UnattributedErrors)
	}
	wantBlocked := []ScopedFile{
		{Path: "gaggles/g/gaggle.yaml", Objects: []string{"Gaggle/g"}},
		{Path: "gaggles/g/workflows/good.yaml", Objects: []string{"Workflow/good", "Workflow/good-two"}},
		{Path: "gaggles/g/workflows/warned.yaml", Objects: []string{"Workflow/warned"}},
		{Path: "manifest.yaml", Objects: []string{"Manifest/inst"}},
	}
	if !reflect.DeepEqual(scope.BlockedFiles, wantBlocked) {
		t.Fatalf("BlockedFiles = %+v, want %+v", scope.BlockedFiles, wantBlocked)
	}

	got := strings.Join(scope.Lines(), "\n")
	for _, want := range []string{
		"loaded as one unit (fail closed)",
		"files with errors (1):\n  gaggles/g/workflows/bad.yaml\n  (1 error(s) not attributed to a single file)",
		"files without errors of their own, also not loaded (4):",
		"  gaggles/g/workflows/good.yaml (Workflow/good, Workflow/good-two)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Lines() missing %q:\n%s", want, got)
		}
	}
}
