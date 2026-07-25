package agentkit

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallAllHarnessesIsIdempotent(t *testing.T) {
	bundle := testBundle(t, "v1.2.3", "abc123")
	tests := []struct {
		harness string
		target  string
	}{
		{harness: "copilot", target: ".github/copilot-instructions.md"},
		{harness: "claude", target: "CLAUDE.md"},
		{harness: "generic", target: "AGENTS.md"},
	}
	for _, test := range tests {
		t.Run(test.harness, func(t *testing.T) {
			root := newTestRepository(t)
			repository := openTestRepository(t, root)

			result, err := repository.Install(bundle, test.harness)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Installed || !result.InstructionCreated || result.InstructionPath != test.target {
				t.Fatalf("install result = %+v", result)
			}
			instruction := readTestFile(t, root, test.target)
			if !bytes.Contains(instruction, []byte(".goobers/agent-toolkit/adapters/")) {
				t.Fatalf("instruction reference = %q", instruction)
			}
			report, err := repository.Check(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if report.State != "current" || report.UpdateAvailable {
				t.Fatalf("check report = %+v", report)
			}

			result, err = repository.Install(bundle, test.harness)
			if err != nil {
				t.Fatal(err)
			}
			if result.Installed || result.InstructionCreated {
				t.Fatalf("repeated install was not idempotent: %+v", result)
			}
		})
	}
}

func TestInstallPreservesExistingUserInstruction(t *testing.T) {
	root := newTestRepository(t)
	const userContent = "# Existing instructions\n\nKeep this content.\n"
	writeTestFile(t, root, "CLAUDE.md", []byte(userContent))
	repository := openTestRepository(t, root)

	result, err := repository.Install(testBundle(t, "v1.2.3", "abc123"), "claude")
	if err != nil {
		t.Fatal(err)
	}
	if result.InstructionCreated {
		t.Fatal("existing user instruction was reported as created")
	}
	if got := string(readTestFile(t, root, "CLAUDE.md")); got != userContent {
		t.Fatalf("user instruction changed to %q", got)
	}
}

func TestCheckReportsModifiedMissingAndUpgrade(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	oldBundle := testBundle(t, "v1.2.3", "abc123")
	if _, err := repository.Install(oldBundle, "generic"); err != nil {
		t.Fatal(err)
	}

	modifiedPath := InstalledRoot + "/README.md"
	missingPath := InstalledRoot + "/adapters/claude.md"
	writeTestFile(t, root, modifiedPath, []byte("local edit\n"))
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(missingPath))); err != nil {
		t.Fatal(err)
	}

	report, err := repository.Check(testBundle(t, "v2.0.0", "def456"))
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "upgrade-available" || !report.UpdateAvailable {
		t.Fatalf("check state = %+v", report)
	}
	if !containsString(report.Modified, modifiedPath) || !containsString(report.Missing, missingPath) {
		t.Fatalf("check drift = modified %v, missing %v", report.Modified, report.Missing)
	}
	if report.BundleVersion != BundleVersion ||
		report.SourceBinaryVersion != "v2.0.0" ||
		report.InstalledSourceVersion != "v1.2.3" {
		t.Fatalf("check identities = %+v", report)
	}
}

func TestUpdateRequiresAcknowledgementAndPreservesUserFiles(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	oldBundle := testBundle(t, "v1.2.3", "abc123")
	newBundle := testBundle(t, "v2.0.0", "def456")
	if _, err := repository.Install(oldBundle, "generic"); err != nil {
		t.Fatal(err)
	}

	modifiedPath := InstalledRoot + "/README.md"
	customSkill := InstalledRoot + "/skills/my-skill/SKILL.md"
	writeTestFile(t, root, modifiedPath, []byte("local edit\n"))
	writeTestFile(t, root, customSkill, []byte("# My skill\n"))

	plan, err := repository.PlanUpdate(newBundle)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(plan.ModifiedOwned, modifiedPath) {
		t.Fatalf("modified owned files = %v", plan.ModifiedOwned)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("version update produced no changes")
	}
	if err := repository.ApplyUpdate(plan, false); err == nil || !strings.Contains(err.Error(), "--replace-modified") {
		t.Fatalf("unacknowledged update error = %v", err)
	}
	if got := string(readTestFile(t, root, modifiedPath)); got != "local edit\n" {
		t.Fatalf("dry update changed modified file to %q", got)
	}

	if err := repository.ApplyUpdate(plan, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readTestFile(t, root, modifiedPath), newBundle.Files[modifiedPath].Data) {
		t.Fatal("acknowledged update did not replace modified owned file")
	}
	if got := string(readTestFile(t, root, customSkill)); got != "# My skill\n" {
		t.Fatalf("user-created skill changed to %q", got)
	}
	report, err := repository.Check(newBundle)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "current" {
		t.Fatalf("post-update check = %+v", report)
	}
	repeated, err := repository.PlanUpdate(newBundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.Changes) != 0 {
		t.Fatalf("repeated update changes = %v", repeated.Changes)
	}
}

func TestUpdateRestoresMissingOwnedFileWithoutAcknowledgement(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	bundle := testBundle(t, "v1.2.3", "abc123")
	if _, err := repository.Install(bundle, "generic"); err != nil {
		t.Fatal(err)
	}
	missingPath := InstalledRoot + "/adapters/copilot.md"
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(missingPath))); err != nil {
		t.Fatal(err)
	}

	plan, err := repository.PlanUpdate(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ModifiedOwned) != 0 {
		t.Fatalf("missing file treated as modified: %v", plan.ModifiedOwned)
	}
	if err := repository.ApplyUpdate(plan, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readTestFile(t, root, missingPath), bundle.Files[missingPath].Data) {
		t.Fatal("missing owned file was not restored")
	}
}

func TestUpdateRefusesUserOwnedCollision(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	oldBundle := testBundle(t, "v1.2.3", "abc123")
	if _, err := repository.Install(oldBundle, "generic"); err != nil {
		t.Fatal(err)
	}

	const collisionPath = InstalledRoot + "/future.md"
	writeTestFile(t, root, collisionPath, []byte("user content\n"))
	newBundle := testBundle(t, "v2.0.0", "def456")
	future := File{Path: collisionPath, Data: []byte("product content\n"), Mode: 0o644}
	newBundle.Files[collisionPath] = future
	newBundle.Manifest.Assets = append(newBundle.Manifest.Assets, Asset{
		Path:   "payload/" + collisionPath,
		SHA256: digest(future.Data),
		Size:   int64(len(future.Data)),
	})
	var err error
	newBundle.ManifestJSON, err = marshalManifest(newBundle.Manifest)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := repository.PlanUpdate(newBundle)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(plan.UserCollisions, collisionPath) {
		t.Fatalf("user collisions = %v", plan.UserCollisions)
	}
	if err := repository.ApplyUpdate(plan, true); err == nil {
		t.Fatal("user-owned collision was overwritten")
	}
	if got := string(readTestFile(t, root, collisionPath)); got != "user content\n" {
		t.Fatalf("collision content changed to %q", got)
	}
}

func TestMissingManifestFailsClosed(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	bundle := testBundle(t, "v1.2.3", "abc123")
	if _, err := repository.Install(bundle, "generic"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(InstalledManifestPath))); err != nil {
		t.Fatal(err)
	}

	report, err := repository.Check(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "missing-manifest" || !report.UpdateAvailable {
		t.Fatalf("missing manifest report = %+v", report)
	}
	if _, err := repository.PlanUpdate(bundle); err == nil {
		t.Fatal("update accepted a missing manifest")
	}
	if _, err := repository.Install(bundle, "generic"); err == nil {
		t.Fatal("install claimed an existing manifestless product root")
	}
}

func TestModifiedManifestRequiresAcknowledgement(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	bundle := testBundle(t, "v1.2.3", "abc123")
	if _, err := repository.Install(bundle, "generic"); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, filepath.FromSlash(InstalledManifestPath))
	manifest := readTestFile(t, root, InstalledManifestPath)
	if err := os.WriteFile(manifestPath, append([]byte("\n"), manifest...), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := repository.Check(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "modified" || !containsString(report.Modified, InstalledManifestPath) {
		t.Fatalf("modified manifest report = %+v", report)
	}
	plan, err := repository.PlanUpdate(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(plan.ModifiedOwned, InstalledManifestPath) {
		t.Fatalf("modified owned files = %v", plan.ModifiedOwned)
	}
	if err := repository.ApplyUpdate(plan, false); err == nil {
		t.Fatal("modified manifest was replaced without acknowledgement")
	}
	if err := repository.ApplyUpdate(plan, true); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryRejectsTraversalAndSymlinkEscapes(t *testing.T) {
	root := newTestRepository(t)
	traversal := root + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(root)
	if _, err := OpenRepository(traversal); err == nil {
		t.Fatal("parent traversal target was accepted")
	}

	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(root, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := OpenRepository(link); err == nil {
		t.Fatal("symbolic-link repository target was accepted")
	}

	escapeRoot := newTestRepository(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(escapeRoot, ".goobers")); err != nil {
		t.Fatal(err)
	}
	escapeRepository := openTestRepository(t, escapeRoot)
	if _, err := escapeRepository.Install(testBundle(t, "v1.2.3", "abc123"), "generic"); err == nil {
		t.Fatal("install traversed a symbolic-link product root")
	}
}

func TestCheckRejectsOwnedFileSymlink(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	bundle := testBundle(t, "v1.2.3", "abc123")
	if _, err := repository.Install(bundle, "generic"); err != nil {
		t.Fatal(err)
	}
	ownedPath := filepath.Join(root, filepath.FromSlash(InstalledRoot+"/README.md"))
	if err := os.Remove(ownedPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ownedPath); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := repository.Check(bundle); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("owned-file symlink error = %v", err)
	}
}

func TestInstallDoesNotFollowPredictableTemporarySymlink(t *testing.T) {
	root := newTestRepository(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "AGENTS.md.tmp")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}

	repository := openTestRepository(t, root)
	if _, err := repository.Install(testBundle(t, "v1.2.3", "abc123"), "generic"); err != nil {
		t.Fatal(err)
	}
	if got := string(readTestFile(t, filepath.Dir(outside), filepath.Base(outside))); got != "outside\n" {
		t.Fatalf("outside file changed to %q", got)
	}
}

func newTestRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func openTestRepository(t *testing.T, root string) *Repository {
	t.Helper()
	repository, err := OpenRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func writeTestFile(t *testing.T, root, relative string, data []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, root, relative string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
