package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestInitQuickstartConfigSourceJSONGoldens(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
		check func(*testing.T, string)
	}{
		{name: "empty"},
		{
			name: "partial",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if _, err := instance.SeedQuickstartConfigSource(root); err != nil {
					t.Fatalf("seed partial fixture: %v", err)
				}
				if err := os.Remove(filepath.Join(root, instance.GuidedSourceInstanceFile)); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(filepath.Join(root, "gaggles")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "populated",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if _, err := instance.SeedQuickstartConfigSource(root); err != nil {
					t.Fatalf("seed populated fixture: %v", err)
				}
				if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("keep me\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, root string) {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(root, "README.md"))
				if err != nil || string(data) != "keep me\n" {
					t.Fatalf("unmanaged file changed: data=%q err=%v", data, err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "config-source")
			if test.setup != nil {
				test.setup(t, root)
			}

			code, stdout, stderr := runArgs(
				t,
				"init",
				"--template=quickstart",
				"--source-tree",
				root,
				"--json",
			)
			if code != 0 || stderr != "" {
				t.Fatalf("init source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if test.check != nil {
				test.check(t, root)
			}

			var envelope configSourceActionEnvelope
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("decode result: %v\n%s", err, stdout)
			}
			envelope.Path = "$SOURCE"
			envelope.NextCommand = strings.ReplaceAll(envelope.NextCommand, absolutePath(root), "$SOURCE")
			var normalized bytes.Buffer
			if err := encodeIndentedJSON(&normalized, envelope); err != nil {
				t.Fatal(err)
			}
			assertGoldenFile(
				t,
				filepath.Join("testdata", "init-template-source", test.name+".golden.json"),
				normalized.String(),
			)

			assertQuickstartSourceValid(t, root)
		})
	}
}

func TestInitQuickstartConfigSourceRejectsConflictingManagedFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config-source")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "manifest.yaml")
	if err := os.WriteFile(manifest, []byte("user-owned\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(
		t,
		"init",
		"--template=quickstart",
		"--source-tree",
		root,
		"--json",
	)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "manifest.yaml differs from the quickstart template") {
		t.Fatalf("init source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	data, err := os.ReadFile(manifest)
	if err != nil || string(data) != "user-owned\n" {
		t.Fatalf("conflicting manifest changed: data=%q err=%v", data, err)
	}
}

func TestInitQuickstartConfigSourceRejectsSemanticallyInvalidPopulatedDestination(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config-source")
	if _, err := instance.SeedQuickstartConfigSource(root); err != nil {
		t.Fatalf("seed populated fixture: %v", err)
	}
	workflowPath := filepath.Join(root, "gaggles", "example", "workflows", "quickstart.yaml")
	workflow, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	invalidWorkflow := strings.Replace(string(workflow), "name: quickstart", "name: invalid-command", 1)
	invalidWorkflow = strings.Replace(invalidWorkflow, `"backlog-query"`, `"missing-command"`, 1)
	if err := os.WriteFile(
		filepath.Join(root, "gaggles", "example", "workflows", "invalid-command.yaml"),
		[]byte(invalidWorkflow),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(
		t,
		"init",
		"--template=quickstart",
		"--source-tree",
		root,
		"--json",
	)
	if code != 1 || stdout != "" ||
		!strings.Contains(stderr, `"code": "COMMAND001"`) ||
		!strings.Contains(stderr, `unknown goobers verb \"missing-command\"`) {
		t.Fatalf("init source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestInitQuickstartConfigSourceQuotesNextCommandPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config $HOME ' $(touch-pwned) `touch-pwned`")
	code, stdout, stderr := runArgs(
		t,
		"init",
		"--template=quickstart",
		"--source-tree",
		root,
		"--json",
	)
	if code != 0 || stderr != "" {
		t.Fatalf("init source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var envelope configSourceActionEnvelope
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode result: %v\n%s", err, stdout)
	}
	abs := absolutePath(root)
	quotedAbs := "'" + strings.ReplaceAll(abs, "'", `'"'"'`) + "'"
	want := "goobers validate --source-tree --json " + quotedAbs
	if envelope.NextCommand != want {
		t.Fatalf("nextCommand = %q, want %q", envelope.NextCommand, want)
	}
}

func TestInitQuickstartConfigSourceFlagValidation(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{
			args: []string{"init", "--source-tree", "source"},
			want: "--source-tree requires --template=quickstart",
		},
		{
			args: []string{"init", "--json"},
			want: "--json is supported by init only with --source-tree",
		},
		{
			args: []string{"init", "--template=quickstart", "--source-tree", "source", "instance"},
			want: "--source-tree supplies the destination",
		},
	}
	for _, test := range tests {
		code, stdout, stderr := runArgs(t, test.args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, test.want) {
			t.Errorf("args=%v code=%d stdout=%q stderr=%q, want %q", test.args, code, stdout, stderr, test.want)
		}
	}
}

func assertQuickstartSourceValid(t *testing.T, root string) {
	t.Helper()
	code, stdout, stderr := runArgs(t, "validate", "--source-tree", "--json", root)
	if code != 0 || stderr != "" {
		t.Fatalf("validate source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var result struct {
		OK       bool              `json:"ok"`
		Findings []json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode validation result: %v\n%s", err, stdout)
	}
	if !result.OK || len(result.Findings) != 0 {
		t.Fatalf("validation result = %s", stdout)
	}
}
