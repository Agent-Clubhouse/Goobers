package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixDSL3MigratesSharedGaggleRequirements(t *testing.T) {
	root, workflowPath := initFixTestInstance(t)
	replaceInFile(t, workflowPath, `dslVersion: "1.4"`, `dslVersion: "2.0"`)
	replaceInFile(t, workflowPath, "metadata:\n", "metadata:\n  annotations:\n    goobers.dev/allow-preview-features: \"true\"\n")
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	appendToFile(t, gagglePath, "  requiredCapabilities: [os=darwin, xcode]\n")
	before, err := os.ReadFile(gagglePath)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "fix", "--to", "3.0", root)
	if code != 0 || !strings.Contains(stdout, "FIX Gaggle/example") || !strings.Contains(stdout, "+    os: macOS") {
		t.Fatalf("dry run %d: %s %s", code, stdout, stderr)
	}
	unchanged, _ := os.ReadFile(gagglePath)
	if string(unchanged) != string(before) {
		t.Fatal("dry run changed gaggle")
	}
	code, stdout, stderr = runArgs(t, "fix", "--to", "3.0", "--write", root)
	if code != 0 {
		t.Fatalf("write %d: %s %s", code, stdout, stderr)
	}
	after, _ := os.ReadFile(gagglePath)
	if strings.Contains(string(after), "dslVersion:") || strings.Contains(string(after), "requiredCapabilities:") || !strings.Contains(string(after), "capabilities: [xcode]") {
		t.Fatalf("gaggle = %s", after)
	}
	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("migrated validation %d: %s %s", code, stdout, stderr)
	}
	code, stdout, stderr = runArgs(t, "fix", "--to", "3.0", "--write", root)
	if code != 0 || !strings.Contains(stdout, "nothing to migrate") {
		t.Fatalf("repeat %d: %s %s", code, stdout, stderr)
	}
}

func TestFixDSL3RefusedGaggleDoesNotUpgradeWorkflows(t *testing.T) {
	root, workflowPath := initFixTestInstance(t)
	replaceInFile(t, workflowPath, `dslVersion: "1.4"`, `dslVersion: "2.0"`)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	appendToFile(t, gagglePath, "  requiredCapabilities: [os=freebsd]\n")
	before, _ := os.ReadFile(workflowPath)
	code, stdout, stderr := runArgs(t, "fix", "--to", "3.0", "--write", root)
	if code != 1 || !strings.Contains(stdout, "FIX Gaggle/example") {
		t.Fatalf("refusal %d: %s %s", code, stdout, stderr)
	}
	after, _ := os.ReadFile(workflowPath)
	if string(after) != string(before) {
		t.Fatal("workflow changed despite refused shared floor")
	}
}

func TestFixDSL3FailedWorkflowLeavesSharedGaggleUnchanged(t *testing.T) {
	root, _ := initFixTestInstance(t) // 1.4 cannot jump directly to 3.0.
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	appendToFile(t, gagglePath, "  requiredCapabilities: [go]\n")
	followerPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "follower.yaml")
	follower := strings.ReplaceAll(strings.ReplaceAll(fixTestWorkflowYAML, "default-implement", "follower"), `dslVersion: "1.4"`, `dslVersion: "2.0"`)
	if err := os.WriteFile(followerPath, []byte(follower), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(gagglePath)
	code, stdout, stderr := runArgs(t, "fix", "--to", "3.0", "--write", root)
	if code != 1 || !strings.Contains(stdout, "shared requirements left unchanged") {
		t.Fatalf("refusal %d: %s %s", code, stdout, stderr)
	}
	migrated, _ := os.ReadFile(followerPath)
	if !strings.Contains(string(migrated), `dslVersion: "3.0"`) {
		t.Fatal("an independent workflow migration was skipped")
	}
	after, _ := os.ReadFile(gagglePath)
	if string(after) != string(before) {
		t.Fatal("shared floor changed while legacy workflow remains")
	}
}
