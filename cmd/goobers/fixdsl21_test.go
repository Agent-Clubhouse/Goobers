package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/dslmigrate"
)

func initFixDSL21Instance(t *testing.T, authorized bool) (root, path, original string) {
	t.Helper()
	root, path = initFixTestInstance(t)
	original = strings.Replace(fixTestWorkflowYAML, `dslVersion: "1.4"`, `dslVersion: "2.0"`, 1)
	original = strings.Replace(original, `command: ["goobers", "ci-poll"]`, `command: ["goobers", "pr-comment-watch"]`, 1)
	actions := "      policyActions: [label-issue] # original policy\n"
	if authorized {
		actions = "      policyActions: [label-issue, comment-on-issue] # original policy\n"
	}
	original = strings.Replace(original, "      capabilities:\n", actions+"      capabilities:\n", 1)
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	return root, path, original
}

func TestFixDSL21PreviewRefusalWriteRollback(t *testing.T) {
	root, path, original := initFixDSL21Instance(t, false)
	for _, flags := range [][]string{{"fix", "--to", "2.1", root}, {"fix", "--to", "2.1", "--write", root}} {
		code, stdout, stderr := runArgs(t, flags...)
		if (len(flags) == 4 && code != 0) || (len(flags) == 5 && code != 1) {
			t.Fatalf("code=%d out=%s err=%s", code, stdout, stderr)
		}
		for _, want := range []string{"POLICY ADD", "comment-on-issue", "PREVIEW", "+dslVersion:"} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("missing %q: %s", want, stdout)
			}
		}
		current, _ := os.ReadFile(path)
		if string(current) != original {
			t.Fatal("preview/refusal modified original")
		}
		if _, err := os.Stat(filepath.Join(root, dslmigrate.BackupName)); !os.IsNotExist(err) {
			t.Fatal("refusal created backup")
		}
	}
	code, stdout, stderr := runArgs(t, "fix", "--to", "2.1", "--write", "--allow-policy-expansion", root)
	if code != 0 {
		t.Fatalf("write: %d %s %s", code, stdout, stderr)
	}
	current, _ := os.ReadFile(path)
	if !strings.Contains(string(current), `dslVersion: "2.1"`) || strings.Count(string(current), "comment-on-issue") != 1 {
		t.Fatal(string(current))
	}
	code, stdout, stderr = runArgs(t, "fix", "--to", "2.1", "--write", root)
	if code != 0 || !strings.Contains(stdout, "already at the target") {
		t.Fatalf("idempotence: %d %s %s", code, stdout, stderr)
	}
	code, stdout, stderr = runArgs(t, "fix", "--rollback", root)
	if code != 0 {
		t.Fatalf("rollback: %d %s %s", code, stdout, stderr)
	}
	current, _ = os.ReadFile(path)
	if string(current) != original {
		t.Fatal("production rollback did not restore exact original")
	}
}

func TestFixDSL21AlreadyAuthorizedNeedsNoPolicyAcknowledgment(t *testing.T) {
	root, path, original := initFixDSL21Instance(t, true)
	code, stdout, stderr := runArgs(t, "fix", "--to", "2.1", "--write", root)
	if code != 0 || strings.Contains(stdout, "POLICY ADD") {
		t.Fatalf("write: %d %s %s", code, stdout, stderr)
	}
	current, _ := os.ReadFile(path)
	if string(current) != strings.Replace(original, `dslVersion: "2.0"`, `dslVersion: "2.1"`, 1) {
		t.Fatal("authorized workflow changed beyond pin")
	}
}

func TestFixDSL21RejectsConflictingFlags(t *testing.T) {
	for _, args := range [][]string{
		{"fix", "--rollback", "--write"}, {"fix", "--rollback", "--to", "2.1"},
		{"fix", "--allow-policy-expansion", "--to", "2.1"}, {"fix", "--allow-policy-expansion", "--to", "3.0", "--write"},
	} {
		if code, _, _ := runArgs(t, args...); code != 2 {
			t.Fatalf("%v: code=%d", args, code)
		}
	}
}
