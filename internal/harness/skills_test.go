package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gitexclude"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/workflow"
)

func testSkills() map[string][]workflow.SkillFile {
	return map[string][]workflow.SkillFile{"review": {{Path: "SKILL.md", Content: "skill-only-marker\r\n"}, {Path: "scripts/check.sh", Content: "#!/bin/sh\necho checking\n"}, {Path: ".gitignore", Content: "!SKILL.md\n"}}}
}

func skillExecutor(h apiv1.Harness) *Executor {
	e := &Executor{}
	WithSkills(h, testSkills())(e)
	return e
}

func skillTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := skillGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return string(out)
}

func TestExecutorMaterializesSkillsBeforeInvocation(t *testing.T) {
	for _, harness := range []apiv1.Harness{apiv1.HarnessCopilot, apiv1.HarnessClaudeCode, apiv1.HarnessCodex} {
		t.Run(string(harness), func(t *testing.T) {
			workspace := t.TempDir()
			dir, _ := skillsDirectory(harness)
			calls := 0
			adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
				calls++
				for name, files := range testSkills() {
					for _, file := range files {
						data, err := os.ReadFile(filepath.Join(req.Workspace, dir, name, file.Path))
						if err != nil || string(data) != file.Content {
							return fmt.Errorf("skill %s bytes %q: %w", file.Path, data, err)
						}
					}
				}
				if strings.Contains(renderPrompt(req), "skill-only-marker") {
					return errors.New("skill duplicated into prompt")
				}
				return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			}}
			rec := &fakeRecorder{}
			executor, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "role instructions", WithSkills(harness, testSkills()))
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := executor.Invoke(context.Background(), testEnvelope(workspace)); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(workspace, dir, "review")); !os.IsNotExist(err) {
					t.Fatalf("snapshot survived: %v", err)
				}
			}
			if calls != 2 {
				t.Fatalf("adapter calls %d", calls)
			}
		})
	}
}

func TestSkillsSnapshotStaleConcurrentAndImmutable(t *testing.T) {
	ctx, workspace := context.Background(), t.TempDir()
	packages := testSkills()
	e := &Executor{}
	WithSkills(apiv1.HarnessCopilot, packages)(e)
	packages["review"][0].Content = "mutated source"
	first, err := e.prepareSkills(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(workspace, ".github/skills/review/SKILL.md"))
	if string(data) != "skill-only-marker\r\n" {
		t.Fatalf("source alias: %q", data)
	}
	for _, contender := range []*Executor{e, {skillsHarness: apiv1.HarnessClaudeCode}} {
		if next, err := contender.prepareSkills(ctx, workspace); !errors.Is(err, lock.ErrHeld) {
			if next != nil {
				_ = next.Close()
			}
			t.Fatalf("concurrent snapshot admitted: %v", err)
		}
	}
	// Simulate process death: release descriptors without snapshot cleanup.
	if err := first.held.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.root.Close(); err != nil {
		t.Fatal(err)
	}
	empty := &Executor{skillsHarness: apiv1.HarnessClaudeCode}
	recovered, err := empty.prepareSkills(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".github/skills/review")); !os.IsNotExist(err) {
		t.Fatalf("stale snapshot survived: %v", err)
	}
}

func TestSkillsRejectUnsafePathsAndCollisions(t *testing.T) {
	for _, unsafe := range []string{"../outside", "/absolute", "a/../../outside", `a\outside`, "C:/outside", "a\nb", "a./file", ".git/config"} {
		t.Run(unsafe, func(t *testing.T) {
			e := &Executor{skillsHarness: apiv1.HarnessCopilot, skills: map[string][]workflow.SkillFile{"review": {{Path: unsafe}}}}
			if _, err := e.prepareSkills(context.Background(), t.TempDir()); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	for _, prefix := range []string{".github", ".github/skills", ".goobers"} {
		t.Run("symlink-"+prefix, func(t *testing.T) {
			workspace, outside := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(workspace, prefix)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(workspace, prefix)); err != nil {
				t.Skip(err)
			}
			if _, err := skillExecutor(apiv1.HarnessCopilot).prepareSkills(context.Background(), workspace); err == nil {
				t.Fatal("symlink accepted")
			}
			entries, _ := os.ReadDir(outside)
			if len(entries) != 0 {
				t.Fatalf("outside modified: %v", entries)
			}
		})
	}
	workspace := t.TempDir()
	target := filepath.Join(workspace, ".github/skills/review")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("repository skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := skillExecutor(apiv1.HarnessCopilot).prepareSkills(context.Background(), workspace); err == nil {
		t.Fatal("repository collision accepted")
	}
	data, _ := os.ReadFile(filepath.Join(target, "SKILL.md"))
	if string(data) != "repository skill" {
		t.Fatalf("repository skill overwritten: %q", data)
	}
}

func TestSkillsGitExclusionsAndLinkedWorktrees(t *testing.T) {
	ctx, firstDir := context.Background(), t.TempDir()
	skillTestGit(t, firstDir, "init")
	skillTestGit(t, firstDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
	secondDir := filepath.Join(t.TempDir(), "worktree")
	skillTestGit(t, firstDir, "worktree", "add", "--detach", secondDir)
	if err := gitexclude.Ensure(ctx, firstDir, gitexclude.Pattern{Line: "/.goobers/"}); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(firstDir, ".git/info/exclude")
	original, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	first, err := skillExecutor(apiv1.HarnessCopilot).prepareSkills(ctx, firstDir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := skillExecutor(apiv1.HarnessCopilot).prepareSkills(ctx, secondDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if status := skillTestGit(t, secondDir, "status", "--porcelain"); status != "" {
		t.Fatalf("second snapshot visible: %s", status)
	}
	skillTestGit(t, secondDir, "add", "-A")
	if staged := skillTestGit(t, secondDir, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("staged skills: %s", staged)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(exclude)
	if string(restored) != string(original) {
		t.Fatalf("exclusions changed: %q => %q", original, restored)
	}
}

func TestSkillsPreserveUnrelatedTrackedSkillsAndIndexFlags(t *testing.T) {
	ctx, workspace := context.Background(), t.TempDir()
	skillTestGit(t, workspace, "init")
	other := filepath.Join(workspace, ".github/skills/other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "SKILL.md"), []byte("repository"), 0o600); err != nil {
		t.Fatal(err)
	}
	skillTestGit(t, workspace, "add", ".github")
	skillTestGit(t, workspace, "update-index", "--skip-worktree", ".github/skills/other/SKILL.md")
	before := skillTestGit(t, workspace, "ls-files", "-v")
	snapshot, err := skillExecutor(apiv1.HarnessCopilot).prepareSkills(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if after := skillTestGit(t, workspace, "ls-files", "-v"); after != before {
		t.Fatalf("index flags changed: %s => %s", before, after)
	}
}

func TestSkillsRollbackFailedWriteAndRecoverStaleExclusions(t *testing.T) {
	ctx, workspace := context.Background(), t.TempDir()
	skillTestGit(t, workspace, "init")
	if err := gitexclude.Ensure(ctx, workspace, gitexclude.Pattern{Line: "/.goobers/"}); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(workspace, ".git/info/exclude")
	original, _ := os.ReadFile(exclude)
	e := &Executor{skillsHarness: apiv1.HarnessCopilot, skills: map[string][]workflow.SkillFile{"review": {{Path: "file", Content: "first"}, {Path: "file/nested", Content: "impossible"}}}}
	if _, err := e.prepareSkills(ctx, workspace); err == nil {
		t.Fatal("file/directory collision accepted")
	}
	if _, err := os.Stat(filepath.Join(workspace, ".github/skills/review")); !os.IsNotExist(err) {
		t.Fatalf("partial snapshot survived: %v", err)
	}
	first, err := skillExecutor(apiv1.HarnessCopilot).prepareSkills(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.held.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.root.Close(); err != nil {
		t.Fatal(err)
	}
	empty := &Executor{skillsHarness: apiv1.HarnessCopilot}
	recovered, err := empty.prepareSkills(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(exclude)
	if string(restored) != string(original) {
		t.Fatalf("stale exclusion leaked: %q", restored)
	}
}

func TestSkillsRejectAbsentTrackedPackageWithoutChangingFlags(t *testing.T) {
	ctx, workspace := context.Background(), t.TempDir()
	skillTestGit(t, workspace, "init")
	target := filepath.Join(workspace, ".github/skills/review")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("repository"), 0o600); err != nil {
		t.Fatal(err)
	}
	skillTestGit(t, workspace, "add", ".github")
	skillTestGit(t, workspace, "update-index", "--assume-unchanged", ".github/skills/review/SKILL.md")
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	before := skillTestGit(t, workspace, "ls-files", "-v")
	if _, err := skillExecutor(apiv1.HarnessCopilot).prepareSkills(ctx, workspace); err == nil {
		t.Fatal("absent tracked package overwritten")
	}
	if after := skillTestGit(t, workspace, "ls-files", "-v"); after != before {
		t.Fatalf("flags changed: %s", after)
	}
}

func TestSkillsRejectUnsafeStaleManifest(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, skillStateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, skillManifestPath), []byte(`{"ID":"stale","Paths":["../outside"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := skillExecutor(apiv1.HarnessCopilot).prepareSkills(context.Background(), workspace); err == nil {
		t.Fatal("unsafe stale manifest accepted")
	}
}
