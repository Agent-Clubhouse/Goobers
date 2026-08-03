package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	goobersassets "github.com/goobers/goobers"
	"github.com/goobers/goobers/internal/agentkit"
	"github.com/goobers/goobers/internal/testgit"
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
	runAgentKitTestGit(t, root, "add", "--", agentkit.InstalledManifestPath)
	runAgentKitTestGit(t, root, "commit", "--quiet", "-m", "install agent toolkit")

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
	if !strings.Contains(stdout, "Added the claude adapter reference to existing CLAUDE.md") ||
		!strings.Contains(stdout, "Starter prompts:") ||
		!strings.Contains(stdout, "Goobers instance at <instance-path>") ||
		!strings.Contains(stdout, "goobers agent-kit check") ||
		!strings.Contains(stdout, "goobers agent-kit update --write") {
		t.Fatalf("install stdout = %q", stdout)
	}
	got, err := os.ReadFile(instruction)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), userContent) ||
		!strings.Contains(string(got), ".goobers/agent-toolkit/adapters/claude.md") {
		t.Fatalf("user instruction and managed reference = %q", got)
	}
}

func TestAgentKitCLIRendersAndRepairsPermissionDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable permission bits are not supported on Windows")
	}

	root := cliAgentKitRepository(t)
	code, _, stderr := runArgs(t, "agent-kit", "install", root)
	if code != 0 {
		t.Fatalf("install: code=%d stderr=%q", code, stderr)
	}
	const executable = ".goobers/agent-toolkit/config-examples/gaggles/acme-web/scripts/check-todos.sh"
	fullPath := filepath.Join(root, filepath.FromSlash(executable))
	if err := os.Chmod(fullPath, 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "agent-kit", "check", root)
	if code != 1 || stderr != "" || !strings.Contains(stdout, executable) {
		t.Fatalf("check: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runArgs(t, "agent-kit", "update", root)
	if code != 0 || stderr != "" ||
		!strings.Contains(stdout, "old mode 0644\nnew mode 0755") ||
		!strings.Contains(stdout, "--write --replace-modified") {
		t.Fatalf("dry-run update: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, _, stderr = runArgs(t, "agent-kit", "update", "--write", root)
	if code != 1 || !strings.Contains(stderr, "--replace-modified") {
		t.Fatalf("unacknowledged update: code=%d stderr=%q", code, stderr)
	}
	code, _, stderr = runArgs(t, "agent-kit", "update", "--write", "--replace-modified", root)
	if code != 0 || stderr != "" {
		t.Fatalf("write update: code=%d stderr=%q", code, stderr)
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("updated executable mode = %04o", info.Mode().Perm())
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
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runAgentKitTestGit(t, root, "init", "--quiet")
	return root
}

func runAgentKitTestGit(t *testing.T, root string, args ...string) {
	t.Helper()
	gitArgs := []string{
		"-C", root,
		"-c", "core.hooksPath=/dev/null",
		"-c", "commit.gpgSign=false",
		"-c", "user.name=Agent Kit Test",
		"-c", "user.email=agent-kit@example.invalid",
	}
	command := testgit.Command(append(gitArgs, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
