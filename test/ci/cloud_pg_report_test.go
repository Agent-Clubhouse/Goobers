package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, root, file, body string) {
	t.Helper()
	full := filepath.Join(root, file)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pgReportTree has, per package, untagged unit tests and PostgreSQL-tagged
// tests: alpha (TestAlphaPG), objstore (TestPG01..TestPG13, like #7834's
// metadata suite), and invariants (TestSchema).
func pgReportTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, root, "internal/cloud/alpha/unit_test.go", "package alpha\n\nimport \"testing\"\n\nfunc TestAlphaUnit(t *testing.T) {}\n")
	writeTestFile(t, root, "internal/cloud/alpha/pg_test.go", "//go:build cloudintegration\n\npackage alpha\n\nimport \"testing\"\n\nfunc TestAlphaPG(t *testing.T) {}\nfunc helper(t *testing.T) {}\n")
	var objstore strings.Builder
	objstore.WriteString("//go:build cloudintegration\n\npackage objstore_test\n\nimport \"testing\"\n\n")
	for i := 1; i <= 13; i++ {
		fmt.Fprintf(&objstore, "func TestPG%02d(t *testing.T) {}\n", i)
	}
	writeTestFile(t, root, "internal/cloud/objstore/pg_test.go", objstore.String())
	writeTestFile(t, root, "internal/cloud/objstore/unit_test.go", "package objstore\n\nimport \"testing\"\n\nfunc TestObjUnit(t *testing.T) {}\n")
	writeTestFile(t, root, "internal/cloud/gamma/unit_test.go", "package gamma\n\nimport \"testing\"\n\nfunc TestGamma(t *testing.T) {}\n")
	writeTestFile(t, root, "test/ha/invariants/schema_test.go", "//go:build integration && cloudpg\n\npackage invariants\n\nimport \"testing\"\n\nfunc TestSchema(t *testing.T) {}\n")
	return root
}

func event(action, pkg, test string) string {
	if !strings.Contains(pkg, "/") {
		pkg = "internal/cloud/" + pkg
	}
	return `{"Action":"` + action + `","Package":"github.com/goobers/goobers/` + pkg + `","Test":"` + test + `"}` + "\n"
}

func objstoreRun(passed, skipped int) string {
	var stream strings.Builder
	for i := 1; i <= 13; i++ {
		switch {
		case i <= passed:
			stream.WriteString(event("pass", "objstore", fmt.Sprintf("TestPG%02d", i)))
		case i <= passed+skipped:
			stream.WriteString(event("skip", "objstore", fmt.Sprintf("TestPG%02d", i)))
		}
	}
	return stream.String()
}

func TestCloudPGReportRequiresTaggedTestsToPassUnskipped(t *testing.T) {
	root := pgReportTree(t)
	healthy := event("pass", "alpha", "TestAlphaPG") + objstoreRun(13, 0) + event("pass", "test/ha/invariants", "TestSchema")
	for name, tc := range map[string]struct {
		events string
		code   int
		want   string
	}{
		"all tagged tests pass": {healthy + event("pass", "alpha", "TestAlphaUnit"), 0, "3 PostgreSQL test packages"},
		"untagged pass only": {event("pass", "alpha", "TestAlphaUnit") + objstoreRun(13, 0) + event("pass", "test/ha/invariants", "TestSchema"),
			1, "alpha: no PostgreSQL-tagged test passed"},
		"tagged 1 pass + 12 skips": {event("pass", "alpha", "TestAlphaPG") + objstoreRun(1, 12) + event("pass", "objstore", "TestObjUnit") + event("pass", "test/ha/invariants", "TestSchema"),
			1, "objstore: PostgreSQL-tagged tests skipped (TestPG02"},
		"invariants skipped": {event("pass", "alpha", "TestAlphaPG") + objstoreRun(13, 0) + event("skip", "test/ha/invariants", "TestSchema"),
			1, "test/ha/invariants: PostgreSQL-tagged tests skipped (TestSchema)"},
		"package missing":   {event("pass", "alpha", "TestAlphaPG") + event("pass", "test/ha/invariants", "TestSchema"), 1, "objstore: did not run"},
		"subtest pass only": {event("pass", "alpha", "TestAlphaPG/sub") + objstoreRun(13, 0) + event("pass", "test/ha/invariants", "TestSchema"), 1, "alpha: no PostgreSQL-tagged test passed"},
		"build failed":      {event("fail", "alpha", "") + objstoreRun(13, 0) + event("pass", "test/ha/invariants", "TestSchema"), 1, "alpha: failed to build"},
	} {
		t.Run(name, func(t *testing.T) {
			report := filepath.Join(t.TempDir(), "report.json")
			if err := os.WriteFile(report, []byte("not json\n"+tc.events), 0o644); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := runCloudPGReport(report, root, &stdout, &stderr); code != tc.code ||
				!strings.Contains(stdout.String()+stderr.String(), tc.want) {
				t.Fatalf("code %d, output %q; want code %d containing %q", code, stdout.String()+stderr.String(), tc.code, tc.want)
			}
		})
	}
}

func TestCloudPGReportCollectsOnlyTaggedTopLevelTests(t *testing.T) {
	tagged, err := cloudIntegrationPackages(pgReportTree(t))
	if err != nil {
		t.Fatal(err)
	}
	alpha := tagged["github.com/goobers/goobers/internal/cloud/alpha"]
	if len(alpha) != 1 || alpha[0] != "TestAlphaPG" {
		t.Fatalf("alpha tagged tests = %v, want only TestAlphaPG (not the untagged test or helper)", alpha)
	}
	if _, ok := tagged["github.com/goobers/goobers/internal/cloud/gamma"]; ok {
		t.Fatal("a package with only untagged tests was required")
	}
	if got := len(tagged["github.com/goobers/goobers/internal/cloud/objstore"]); got != 13 {
		t.Fatalf("objstore tagged tests = %d, want 13", got)
	}
}
