package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	goobersassets "github.com/goobers/goobers"
	"github.com/goobers/goobers/internal/agentkit"
	"github.com/goobers/goobers/internal/version"
)

func TestAgentKitCLIInstallCheckAndUpdate(t *testing.T) {
	root := cliAgentKitRepository(t)
	oldBundle, err := agentkit.Build(goobersassets.AgentToolkitAssets, "v0.9.0", "old123")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := agentkit.OpenRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Install(oldBundle, "generic"); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "agent-kit", "check", root)
	if code != 1 || stderr != "" {
		t.Fatalf("check: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"state: upgrade-available",
		"bundle version: " + agentkit.BundleVersion,
		"source binary version: " + version.Version,
		"installed source version: v0.9.0",
		"update available: yes",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("check stdout missing %q:\n%s", want, stdout)
		}
	}

	releasePath := filepath.Join(root, ".goobers", "agent-toolkit", "release.json")
	before, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runArgs(t, "agent-kit", "update", root)
	if code != 0 || stderr != "" {
		t.Fatalf("dry-run update: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "diff --goobers") || !strings.Contains(stdout, "--- a/") || !strings.Contains(stdout, "+++ b/") {
		t.Fatalf("update did not render a reviewable diff:\n%s", stdout)
	}
	afterDryRun, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterDryRun) != string(before) {
		t.Fatal("default update wrote files")
	}

	code, stdout, stderr = runArgs(t, "agent-kit", "update", "--write", "--replace-modified", root)
	if code != 0 || stderr != "" {
		t.Fatalf("write update: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Updated agent toolkit") {
		t.Fatalf("write update stdout = %q", stdout)
	}
	code, stdout, stderr = runArgs(t, "agent-kit", "check", root)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "state: current") || !strings.Contains(stdout, "update available: no") {
		t.Fatalf("post-update check: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runArgs(t, "agent-kit", "update", root)
	if code != 0 || stderr != "" || stdout != "No agent toolkit changes.\n" {
		t.Fatalf("repeated update: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestAgentKitCLIRequiresModifiedFileAcknowledgement(t *testing.T) {
	root := cliAgentKitRepository(t)
	code, _, stderr := runArgs(t, "agent-kit", "install", "--harness", "copilot", root)
	if code != 0 {
		t.Fatalf("install: code=%d stderr=%q", code, stderr)
	}
	readme := filepath.Join(root, ".goobers", "agent-toolkit", "README.md")
	if err := os.WriteFile(readme, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "agent-kit", "update", "--write", root)
	if code != 1 || !strings.Contains(stderr, "--replace-modified") || !strings.Contains(stdout, "-local edit") {
		t.Fatalf("unacknowledged update: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if got, err := os.ReadFile(readme); err != nil || string(got) != "local edit\n" {
		t.Fatalf("modified file changed: data=%q err=%v", got, err)
	}

	code, _, stderr = runArgs(t, "agent-kit", "update", "--write", "--replace-modified", root)
	if code != 0 {
		t.Fatalf("acknowledged update: code=%d stderr=%q", code, stderr)
	}
}

func TestAgentKitCLIInstallPreservesUserInstructions(t *testing.T) {
	root := cliAgentKitRepository(t)
	instruction := filepath.Join(root, "CLAUDE.md")
	const userContent = "# User-owned\n"
	if err := os.WriteFile(instruction, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "agent-kit", "install", "--harness", "claude", root)
	if code != 0 || stderr != "" {
		t.Fatalf("install: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Preserved existing CLAUDE.md") || !strings.Contains(stdout, "Next steps:") {
		t.Fatalf("install stdout = %q", stdout)
	}
	got, err := os.ReadFile(instruction)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != userContent {
		t.Fatalf("user instruction changed to %q", got)
	}
}

func TestAgentKitCLIRejectsUnsafeTargetAndFlags(t *testing.T) {
	root := cliAgentKitRepository(t)
	code, _, stderr := runArgs(t, "agent-kit", "install", "--harness", "invalid", root)
	if code != 2 || !strings.Contains(stderr, "unsupported harness") {
		t.Fatalf("invalid harness: code=%d stderr=%q", code, stderr)
	}
	traversal := root + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(root)
	code, _, stderr = runArgs(t, "agent-kit", "check", traversal)
	if code != 1 || !strings.Contains(stderr, "parent traversal") {
		t.Fatalf("traversal: code=%d stderr=%q", code, stderr)
	}
	code, _, stderr = runArgs(t, "agent-kit", "update", "--replace-modified", root)
	if code != 2 || !strings.Contains(stderr, "Usage:") {
		t.Fatalf("replace without write: code=%d stderr=%q", code, stderr)
	}
}

func cliAgentKitRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}
