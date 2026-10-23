package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runForTest(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestHelpGolden(t *testing.T) {
	code, stdout, stderr := runForTest("--help")
	if code != 0 || stderr != "" {
		t.Fatalf("--help: code=%d stderr=%q", code, stderr)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "help.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(want) {
		t.Fatalf("--help drifted:\n%s", stdout)
	}
	for _, role := range []string{"api", "controller", "worker", "migrate"} {
		if !strings.Contains(stdout, role) {
			t.Errorf("--help omits role %q", role)
		}
	}
}

func TestDatabaseURLSourcesAndParseErrorsRedactPasswords(t *testing.T) {
	const password = "do-not-log-this-password"
	t.Setenv("GOOBERS_DATABASE_URL", "postgres://user:"+password+"@[invalid/goobers")
	t.Setenv("GOOBERS_DATABASE_DIRECT_URL", "postgres://localhost/goobers")
	code, _, stderr := runForTest("api")
	if code != 1 {
		t.Fatalf("api code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stderr, password) {
		t.Fatalf("api parse error exposed password: %q", stderr)
	}

	urlFile := filepath.Join(t.TempDir(), "database-url")
	if err := os.WriteFile(urlFile, []byte("postgres://file-secret@localhost/goobers\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := databaseURL("", urlFile, "UNSET_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	if value != "postgres://file-secret@localhost/goobers" {
		t.Fatalf("file URL = %q", value)
	}
}

func TestGeneratedDocs(t *testing.T) {
	root := t.TempDir()
	if err := writeDocs(root); err != nil {
		t.Fatal(err)
	}
	for relative, want := range renderDocs() {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		if string(got) != want {
			t.Errorf("%s differs from rendered docs", relative)
		}
	}
	if _, _, stderr := runForTest("__generate-docs"); !strings.Contains(stderr, "usage:") {
		t.Fatalf("missing generator usage: %q", stderr)
	}
}

func TestCommittedDocsUpToDate(t *testing.T) {
	root := filepath.Join("..", "..", "docs")
	for relative, want := range renderDocs() {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read %s: %v; regenerate with `make docs`", relative, err)
		}
		if string(got) != want {
			t.Fatalf("%s is stale; regenerate with `make docs`", relative)
		}
	}
}

func TestCompletionCommand(t *testing.T) {
	code, stdout, stderr := runForTest("completion", "bash")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "goobers-cloud") {
		t.Fatalf("completion bash: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
