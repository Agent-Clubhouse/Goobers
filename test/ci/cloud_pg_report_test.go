package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloudPGReportRequiresPassingTestsInEveryIntegrationPackage(t *testing.T) {
	root := t.TempDir()
	for dir, body := range map[string]string{
		"internal/cloud/alpha/pg_test.go":   "//go:build cloudintegration\n\npackage alpha\n",
		"internal/cloud/beta/pg_test.go":    "//go:build cloudintegration\n\npackage beta\n",
		"internal/cloud/gamma/unit_test.go": "package gamma\n",
	} {
		file := filepath.Join(root, dir)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	event := func(action, pkg, test string) string {
		return `{"Action":"` + action + `","Package":"github.com/goobers/goobers/internal/cloud/` + pkg + `","Test":"` + test + `"}` + "\n"
	}
	for name, tc := range map[string]struct {
		events string
		code   int
		want   string
	}{
		"all ran":      {event("pass", "alpha", "TestA") + event("pass", "beta", "TestB"), 0, "2 cloudintegration packages"},
		"skips only":   {event("pass", "alpha", "TestA") + event("skip", "beta", "TestB"), 1, "beta: no test passed (1 skipped"},
		"missing tag":  {event("pass", "alpha", "TestA"), 1, "beta: did not run"},
		"subtest only": {event("pass", "alpha", "TestA") + event("pass", "beta", "TestB/sub") + event("skip", "beta", "TestB"), 1, "beta: no test passed"},
		"build failed": {event("pass", "alpha", "TestA") + event("fail", "beta", ""), 1, "beta: failed to build"},
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
