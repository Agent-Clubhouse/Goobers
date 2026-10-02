package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestSkillsRejectNonregularManifest(t *testing.T) {
	for _, kind := range []string{"dangling-symlink", "existing-symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			workspace := t.TempDir()
			state := filepath.Join(workspace, skillStateDir)
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(workspace, skillManifestPath)
			target := filepath.Join(workspace, "unowned.json")
			original := `{"ID":"unowned","Paths":[".github/skills/review"]}`
			if kind == "existing-symlink" {
				if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "directory" {
				if err := os.Mkdir(manifest, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink("../../unowned.json", manifest); err != nil {
				t.Skip(err)
			}
			if _, err := skillExecutor(apiv1.HarnessCopilot).prepareSkills(context.Background(), workspace); err == nil {
				t.Fatal("nonregular manifest accepted")
			}
			if _, err := os.Lstat(manifest); err != nil {
				t.Fatalf("unowned manifest leaf changed: %v", err)
			}
			data, err := os.ReadFile(target)
			if kind == "existing-symlink" {
				if err != nil || string(data) != original {
					t.Fatalf("symlink target changed: %q, %v", data, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("unowned target created: %q, %v", data, err)
			}
		})
	}
}

func TestSkillsManifestWriteRejectsDanglingSymlink(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, skillStateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	snapshot := &skillSnapshot{root: root, workspace: workspace}
	if err := snapshot.recover(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../unowned.json", filepath.Join(workspace, skillManifestPath)); err != nil {
		t.Skip(err)
	}
	if err := snapshot.writeManifest(skillManifest{ID: "test", Paths: []string{".github/skills/review"}}); err == nil {
		t.Fatal("manifest write followed symlink introduced after recovery")
	}
	if _, err := os.Stat(filepath.Join(workspace, "unowned.json")); !os.IsNotExist(err) {
		t.Fatalf("unowned target created: %v", err)
	}
}

func TestSkillsRejectAbsentTrackedAncestors(t *testing.T) {
	for _, harness := range []apiv1.Harness{apiv1.HarnessCopilot, apiv1.HarnessClaudeCode, apiv1.HarnessCodex} {
		directory, _ := skillsDirectory(harness)
		for _, ancestor := range []string{strings.Split(directory, "/")[0], directory, ".goobers", skillStateDir} {
			for _, kind := range []string{"file", "symlink"} {
				t.Run(string(harness)+"/"+ancestor+"/"+kind, func(t *testing.T) {
					workspace := t.TempDir()
					before := absentTrackedSkillAncestor(t, workspace, ancestor, kind)
					if _, err := skillExecutor(harness).prepareSkills(context.Background(), workspace); err == nil {
						t.Fatal("tracked ancestor accepted")
					}
					assertTrackedSkillAncestorPreserved(t, workspace, ancestor, before)
				})
			}
		}
	}
}

func TestSkillsStaleCleanupRejectsTrackedAncestors(t *testing.T) {
	for _, ancestor := range []string{".github", ".github/skills"} {
		for _, kind := range []string{"file", "symlink"} {
			t.Run(ancestor+"/"+kind, func(t *testing.T) {
				workspace := t.TempDir()
				before := absentTrackedSkillAncestor(t, workspace, ancestor, kind)
				if err := os.MkdirAll(filepath.Join(workspace, skillStateDir), 0o700); err != nil {
					t.Fatal(err)
				}
				manifest := `{"ID":"stale","Paths":[".github/skills/review"],"Git":true}`
				if err := os.WriteFile(filepath.Join(workspace, skillManifestPath), []byte(manifest), 0o600); err != nil {
					t.Fatal(err)
				}
				empty := &Executor{skillsHarness: apiv1.HarnessClaudeCode}
				if _, err := empty.prepareSkills(context.Background(), workspace); err == nil {
					t.Fatal("stale cleanup accepted tracked ancestor")
				}
				assertTrackedSkillAncestorPreserved(t, workspace, ancestor, before)
				data, err := os.ReadFile(filepath.Join(workspace, skillManifestPath))
				if err != nil || string(data) != manifest {
					t.Fatalf("stale manifest changed on refusal: %q, %v", data, err)
				}
			})
		}
	}
}

func TestSkillsRejectMixedCaseTrackedAncestors(t *testing.T) {
	for _, ancestor := range []string{".GitHub", ".github/Skills", ".Goobers", ".goobers/Skills"} {
		for _, kind := range []string{"file", "symlink"} {
			t.Run(ancestor+"/"+kind, func(t *testing.T) {
				workspace := t.TempDir()
				before := absentTrackedSkillAncestor(t, workspace, ancestor, kind)
				// Enforce conservative alias protection even when Git is configured
				// case-sensitively or the test runs on a case-sensitive filesystem.
				skillTestGit(t, workspace, "config", "core.ignoreCase", "false")
				if _, err := skillExecutor(apiv1.HarnessCopilot).prepareSkills(context.Background(), workspace); err == nil {
					t.Fatal("mixed-case tracked ancestor accepted")
				}
				assertTrackedSkillAncestorPreserved(t, workspace, ancestor, before)
				if _, err := os.Lstat(filepath.Join(workspace, strings.ToLower(ancestor))); !os.IsNotExist(err) {
					t.Fatalf("lowercase alias was recreated: %v", err)
				}
			})
		}
	}
}

func TestSkillsStaleCleanupPreservesMixedCaseTrackedPackage(t *testing.T) {
	workspace := t.TempDir()
	skillTestGit(t, workspace, "init")
	tracked := ".GitHub/Skills/Review/SKILL.md"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(workspace, tracked)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, tracked), []byte("repository skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	skillTestGit(t, workspace, "add", "--", tracked)
	skillTestGit(t, workspace, "update-index", "--skip-worktree", "--", tracked)
	before := skillTestGit(t, workspace, "ls-files", "--stage", "-v")
	if err := os.MkdirAll(filepath.Join(workspace, skillStateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"ID":"stale","Paths":[".github/skills/review"],"Git":true}`
	if err := os.WriteFile(filepath.Join(workspace, skillManifestPath), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := &Executor{skillsHarness: apiv1.HarnessClaudeCode}
	if _, err := empty.prepareSkills(context.Background(), workspace); err == nil {
		t.Fatal("stale cleanup accepted mixed-case tracked package")
	}
	for file, want := range map[string]string{tracked: "repository skill", skillManifestPath: manifest} {
		data, err := os.ReadFile(filepath.Join(workspace, file))
		if err != nil || string(data) != want {
			t.Fatalf("cleanup modified %s: %q, %v", file, data, err)
		}
	}
	if after := skillTestGit(t, workspace, "ls-files", "--stage", "-v"); after != before {
		t.Fatalf("index or flags changed: %q => %q", before, after)
	}
}

func TestSkillsCleanupRefusesNewTrackedPathsBeforeDeletingAnything(t *testing.T) {
	for _, tracked := range []string{skillManifestPath, ".github/skills/review/SKILL.md"} {
		t.Run(tracked, func(t *testing.T) {
			workspace := t.TempDir()
			skillTestGit(t, workspace, "init")
			snapshot, err := skillExecutor(apiv1.HarnessCopilot).prepareSkills(context.Background(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			skillTestGit(t, workspace, "add", "--force", "--", tracked)
			skillTestGit(t, workspace, "update-index", "--skip-worktree", "--", tracked)
			before := skillTestGit(t, workspace, "ls-files", "--stage", "-v")
			if err := snapshot.Close(); err == nil {
				t.Fatal("cleanup accepted a newly tracked snapshot path")
			}
			for _, preserved := range []string{skillManifestPath, ".github/skills/review/SKILL.md"} {
				if _, err := os.Stat(filepath.Join(workspace, preserved)); err != nil {
					t.Fatalf("cleanup deleted %s before refusing: %v", preserved, err)
				}
			}
			if after := skillTestGit(t, workspace, "ls-files", "--stage", "-v"); after != before {
				t.Fatalf("cleanup changed index or flags: %q => %q", before, after)
			}
		})
	}
}

func absentTrackedSkillAncestor(t *testing.T, workspace, ancestor, kind string) string {
	t.Helper()
	skillTestGit(t, workspace, "init")
	target := filepath.Join(workspace, ancestor)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if kind == "symlink" {
		if err := os.Symlink("unowned", target); err != nil {
			t.Skip(err)
		}
	} else if err := os.WriteFile(target, []byte("repository content"), 0o600); err != nil {
		t.Fatal(err)
	}
	skillTestGit(t, workspace, "add", "--", ancestor)
	skillTestGit(t, workspace, "update-index", "--skip-worktree", "--", ancestor)
	before := skillTestGit(t, workspace, "ls-files", "--stage", "-v")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	return before
}

func assertTrackedSkillAncestorPreserved(t *testing.T, workspace, ancestor, before string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(workspace, ancestor)); !os.IsNotExist(err) {
		t.Fatalf("absent tracked ancestor was recreated: %v", err)
	}
	if after := skillTestGit(t, workspace, "ls-files", "--stage", "-v"); after != before {
		t.Fatalf("index or flags changed: %q => %q", before, after)
	}
	if _, err := os.Lstat(filepath.Join(workspace, filepath.Dir(ancestor), "unowned")); !os.IsNotExist(err) {
		t.Fatalf("unowned symlink target changed: %v", err)
	}
}
