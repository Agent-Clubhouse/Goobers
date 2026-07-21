package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestConfigDiffOperationalTuningExitsSuccessfully(t *testing.T) {
	root, canonical := configDiffFixture(t)
	workflow := filepath.Join(root, "config", "gaggles", "acme-web", "workflows", "implementation.yaml")
	replaceConfigDiffFixture(t, workflow,
		`schedule: "3,18,33,48 * * * *"`, `schedule: "0 * * * *"`)
	replaceConfigDiffFixture(t, workflow, "maxConcurrentRuns: 1", "maxConcurrentRuns: 4")
	nomination := filepath.Join(root, "config", "gaggles", "acme-web", "workflows", "work-nomination.yaml")
	replaceConfigDiffFixture(t, nomination,
		"    - type: schedule\n      schedule: \"43 7 * * *\"",
		"    - type: manual")

	code, stdout, stderr := runArgs(t, "config", "diff", "--against", canonical, root)
	if code != 0 {
		t.Fatalf("config diff: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Count(stdout, "INFO ") != 4 || strings.Contains(stdout, "ERROR ") {
		t.Fatalf("config diff output = %q, want four informational differences only", stdout)
	}
	if !strings.Contains(stdout, `field="schedule"`) ||
		!strings.Contains(stdout, `field="readiness.maxConcurrentRuns"`) ||
		!strings.Contains(stdout, `trigger="manual[0]" field="<definition>"`) {
		t.Fatalf("config diff output does not identify tuning fields: %q", stdout)
	}
}

func TestConfigDiffMissingResultFileIsStructural(t *testing.T) {
	root, canonical := configDiffFixture(t)
	workflow := filepath.Join(root, "config", "gaggles", "acme-web", "workflows", "implementation.yaml")
	replaceConfigDiffFixture(t, workflow, `        resultFile: "claimed-item.json"`+"\n", "")

	code, stdout, stderr := runArgs(t, "config", "diff", "--against", canonical, root)
	if code != 1 {
		t.Fatalf("config diff: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		`ERROR workflow="acme-web/implementation" task="query-backlog"`,
		`field="inputs.resultFile"`,
		`active=<missing>`,
		`canonical="claimed-item.json"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("config diff output missing %q: %s", want, stdout)
		}
	}
}

func TestConfigDiffOutputIsDeterministic(t *testing.T) {
	root, canonical := configDiffFixture(t)
	workflow := filepath.Join(root, "config", "gaggles", "acme-web", "workflows", "implementation.yaml")
	replaceConfigDiffFixture(t, workflow, `"goobers", "backlog-query", "--claim"`, `"goobers", "backlog-query", "--claim", "--all"`)

	firstCode, firstStdout, firstStderr := runArgs(t, "config", "diff", "--against", canonical, root)
	secondCode, secondStdout, secondStderr := runArgs(t, "config", "diff", "--against", canonical, root)
	if firstCode != secondCode || firstStdout != secondStdout || firstStderr != secondStderr {
		t.Fatalf("config diff output changed between runs:\nfirst=(%d, %q, %q)\nsecond=(%d, %q, %q)",
			firstCode, firstStdout, firstStderr, secondCode, secondStdout, secondStderr)
	}
}

func configDiffFixture(t *testing.T) (root, canonical string) {
	t.Helper()
	temp := t.TempDir()
	root = filepath.Join(temp, "instance")
	canonical = filepath.Join(temp, "canonical")
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(root, "config"), os.DirFS("../../config-examples")); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(canonical, os.DirFS("../../config-examples")); err != nil {
		t.Fatal(err)
	}
	if err := instance.WriteConfig(filepath.Join(root, instance.ConfigFileName), &instance.Config{
		APIVersion: instance.ConfigAPIVersion,
		Kind:       instance.ConfigKind,
	}); err != nil {
		t.Fatal(err)
	}
	return root, canonical
}

func replaceConfigDiffFixture(t *testing.T, path, old, replacement string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), old) != 1 {
		t.Fatalf("%s contains %q %d times, want exactly once", path, old, strings.Count(string(data), old))
	}
	data = []byte(strings.Replace(string(data), old, replacement, 1))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
