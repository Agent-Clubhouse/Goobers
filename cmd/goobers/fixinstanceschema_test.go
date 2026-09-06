package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const runnersWithoutSchemaVersion = `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runners:
  - name: self
    host: self
    provides:
      capabilities: [go@1.26]
`

func writeInstanceRoot(t *testing.T, body string) (root, path string) {
	t.Helper()
	root = t.TempDir()
	path = filepath.Join(root, "instance.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func TestFixInstanceSchemaDryRunPrintsDiffWithoutWriting(t *testing.T) {
	root, path := writeInstanceRoot(t, runnersWithoutSchemaVersion)

	code, stdout, stderr := runArgs(t, "fix", "--instance-schema", root)
	if code != 0 {
		t.Fatalf("fix --instance-schema: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{"FIX instance.yaml", "dry run — pass --write to apply", "+schemaVersion: 2"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("output missing %q:\n%s", want, stdout)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != runnersWithoutSchemaVersion {
		t.Fatalf("dry run modified the file:\n%s", after)
	}
}

func TestFixInstanceSchemaWriteAddsTheLine(t *testing.T) {
	root, path := writeInstanceRoot(t, runnersWithoutSchemaVersion)

	code, stdout, stderr := runArgs(t, "fix", "--instance-schema", "--write", root)
	if code != 0 {
		t.Fatalf("fix --instance-schema --write: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "(written)") {
		t.Fatalf("output does not report the write:\n%s", stdout)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "\nschemaVersion: 2\n") {
		t.Fatalf("schemaVersion line not written:\n%s", after)
	}
	// Rerunning must be a no-op rather than a second insertion — the remedy is
	// something an operator may run across many instance roots at once.
	code, stdout, _ = runArgs(t, "fix", "--instance-schema", "--write", root)
	if code != 0 || !strings.Contains(stdout, "no change needed") {
		t.Fatalf("second run was not a no-op: code=%d stdout=%q", code, stdout)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(after) {
		t.Fatalf("second run changed the file:\n%s", again)
	}
}

func TestFixInstanceSchemaRefusesCombinedWithTo(t *testing.T) {
	root, _ := writeInstanceRoot(t, runnersWithoutSchemaVersion)

	code, _, stderr := runArgs(t, "fix", "--instance-schema", "--to", "2.0", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2 for combining the two remedies", code)
	}
	if !strings.Contains(stderr, "run one at a time") {
		t.Fatalf("stderr does not explain the conflict: %q", stderr)
	}
}

func TestFixInstanceSchemaReportsAnExplicitWrongVersion(t *testing.T) {
	root, path := writeInstanceRoot(t, strings.Replace(runnersWithoutSchemaVersion,
		"kind: Instance\n", "kind: Instance\nschemaVersion: 1\n", 1))

	code, stdout, _ := runArgs(t, "fix", "--instance-schema", "--write", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 for a present-but-wrong schemaVersion", code)
	}
	if !strings.Contains(stdout, "not a missing line") {
		t.Fatalf("output does not explain the refusal:\n%s", stdout)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "schemaVersion: 1") {
		t.Fatalf("the operator's explicit value was rewritten:\n%s", after)
	}
}
