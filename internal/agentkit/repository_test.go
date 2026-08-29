package agentkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
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

func TestInstallAddsReferenceToExistingUserInstructions(t *testing.T) {
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
			const userContent = "# Existing instructions\n\nKeep this content.\n"
			writeTestFile(t, root, test.target, []byte(userContent))
			repository := openTestRepository(t, root)
			bundle := testBundle(t, "v1.2.3", "abc123")

			result, err := repository.Install(bundle, test.harness)
			if err != nil {
				t.Fatal(err)
			}
			if result.InstructionCreated || !result.InstructionUpdated {
				t.Fatalf("existing instruction result = %+v", result)
			}
			got := readTestFile(t, root, test.target)
			if !bytes.HasPrefix(got, []byte(userContent)) {
				t.Fatalf("user instruction prefix changed to %q", got)
			}
			if bytes.Count(got, []byte(instructionReferenceBegin)) != 1 ||
				bytes.Count(got, []byte(result.Reference)) != 1 {
				t.Fatalf("managed instruction reference = %q", got)
			}

			repeated, err := repository.Install(bundle, test.harness)
			if err != nil {
				t.Fatal(err)
			}
			if repeated.InstructionCreated || repeated.InstructionUpdated {
				t.Fatalf("repeated install changed instructions: %+v", repeated)
			}
			if after := readTestFile(t, root, test.target); !bytes.Equal(after, got) {
				t.Fatalf("repeated install changed instruction to %q", after)
			}
		})
	}
}

func TestCheckReportsModifiedMissingAndUpgrade(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	oldBundle := testBundle(t, "v1.2.3", "abc123")
	if _, err := repository.Install(oldBundle, "generic"); err != nil {
		t.Fatal(err)
	}
	commitTestManifest(t, root)

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
	commitTestManifest(t, root)

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

func TestUpdateRepairsExecutableModeDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable permission bits are not supported on Windows")
	}

	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	bundle := testBundle(t, "v1.2.3", "abc123")
	if _, err := repository.Install(bundle, "generic"); err != nil {
		t.Fatal(err)
	}
	const executablePath = InstalledRoot + "/config-examples/gaggles/acme-web/scripts/check-todos.sh"
	fullPath := filepath.Join(root, filepath.FromSlash(executablePath))
	if err := os.Chmod(fullPath, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := repository.Check(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "modified" || !containsString(report.Modified, executablePath) {
		t.Fatalf("permission drift check = %+v", report)
	}
	plan, err := repository.PlanUpdate(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(plan.ModifiedOwned, executablePath) {
		t.Fatalf("permission drift modified files = %v", plan.ModifiedOwned)
	}
	var modeChange *Change
	for i := range plan.Changes {
		if plan.Changes[i].Path == executablePath {
			modeChange = &plan.Changes[i]
			break
		}
	}
	if modeChange == nil ||
		!bytes.Equal(modeChange.Old, modeChange.New) ||
		modeChange.OldMode.Perm() != 0o644 ||
		modeChange.Mode.Perm() != 0o755 {
		t.Fatalf("permission drift change = %+v", modeChange)
	}
	if err := repository.ApplyUpdate(plan, false); err == nil {
		t.Fatal("permission drift was repaired without acknowledgement")
	}
	if err := repository.ApplyUpdate(plan, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("updated executable mode = %04o", info.Mode().Perm())
	}
	report, err = repository.Check(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "current" {
		t.Fatalf("post-repair check = %+v", report)
	}
}

func TestCleanUpgradeDoesNotRequireModifiedAcknowledgement(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	oldBundle := testBundle(t, "v1.2.3", "abc123")
	newBundle := testBundle(t, "v2.0.0", "def456")
	if _, err := repository.Install(oldBundle, "generic"); err != nil {
		t.Fatal(err)
	}
	commitTestManifest(t, root)

	report, err := repository.Check(newBundle)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "upgrade-available" || containsString(report.Modified, InstalledManifestPath) {
		t.Fatalf("clean upgrade check = %+v", report)
	}
	plan, err := repository.PlanUpdate(newBundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ModifiedOwned) != 0 {
		t.Fatalf("clean upgrade requires modified acknowledgement: %v", plan.ModifiedOwned)
	}
	if err := repository.ApplyUpdate(plan, false); err != nil {
		t.Fatal(err)
	}
	report, err = repository.Check(newBundle)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "current" {
		t.Fatalf("post-upgrade check = %+v", report)
	}
}

func TestCleanUpgradeDeletesObsoleteOwnedAsset(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	oldBundle := testBundle(t, "v1.2.3", "abc123")
	obsoletePath := InstalledRoot + "/obsolete.md"
	obsolete := File{Path: obsoletePath, Data: []byte("obsolete product asset\n"), Mode: 0o644}
	oldBundle.Files[obsoletePath] = obsolete
	oldBundle.Manifest.Assets = append(oldBundle.Manifest.Assets, Asset{
		Path:   "payload/" + obsoletePath,
		SHA256: digest(obsolete.Data),
		Size:   int64(len(obsolete.Data)),
		Mode:   formatAssetMode(obsolete.Mode),
	})
	var err error
	oldBundle.ManifestJSON, err = marshalManifest(oldBundle.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Install(oldBundle, "generic"); err != nil {
		t.Fatal(err)
	}
	commitTestManifest(t, root)

	newBundle := testBundle(t, "v2.0.0", "def456")
	plan, err := repository.PlanUpdate(newBundle)
	if err != nil {
		t.Fatal(err)
	}
	var foundDelete bool
	for _, change := range plan.Changes {
		if change.Path == obsoletePath && change.Kind == ChangeDelete {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Fatalf("obsolete asset deletion missing from changes: %+v", plan.Changes)
	}
	if len(plan.ModifiedOwned) != 0 {
		t.Fatalf("clean obsolete asset requires modified acknowledgement: %v", plan.ModifiedOwned)
	}
	if err := repository.ApplyUpdate(plan, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(obsoletePath))); !os.IsNotExist(err) {
		t.Fatalf("obsolete asset still exists: %v", err)
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

func TestInterruptedMissingOwnedFileRepairResumes(t *testing.T) {
	for _, repairApplied := range []bool{false, true} {
		t.Run(fmt.Sprintf("repair-applied-%t", repairApplied), func(t *testing.T) {
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
			if err := repository.writeUpdateTransaction(plan.Changes); err != nil {
				t.Fatal(err)
			}
			if repairApplied {
				foundRepair := false
				for _, change := range plan.Changes {
					if change.Path == missingPath {
						if err := repository.applyUpdateChange(change); err != nil {
							t.Fatal(err)
						}
						foundRepair = true
						break
					}
				}
				if !foundRepair {
					t.Fatal("missing owned file repair was not planned")
				}
			}

			retry, err := repository.PlanUpdate(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.ApplyUpdate(retry, false); err != nil {
				t.Fatal(err)
			}
			if got := readTestFile(t, root, missingPath); !bytes.Equal(got, bundle.Files[missingPath].Data) {
				t.Fatal("interrupted missing owned file repair was not completed")
			}
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(updateTransactionPath))); !os.IsNotExist(err) {
				t.Fatalf("completed update transaction still exists: %v", err)
			}
		})
	}
}

func TestUpdateRefusesUserOwnedCollision(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	oldBundle := testBundle(t, "v1.2.3", "abc123")
	if _, err := repository.Install(oldBundle, "generic"); err != nil {
		t.Fatal(err)
	}
	commitTestManifest(t, root)

	const collisionPath = InstalledRoot + "/future.md"
	writeTestFile(t, root, collisionPath, []byte("user content\n"))
	newBundle := testBundle(t, "v2.0.0", "def456")
	future := File{Path: collisionPath, Data: []byte("product content\n"), Mode: 0o644}
	newBundle.Files[collisionPath] = future
	newBundle.Manifest.Assets = append(newBundle.Manifest.Assets, Asset{
		Path:   "payload/" + collisionPath,
		SHA256: digest(future.Data),
		Size:   int64(len(future.Data)),
		Mode:   formatAssetMode(future.Mode),
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

func TestManifestDriftCannotClaimUserSkill(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{name: "asset inventory", mutate: func(*Manifest) {}},
		{
			name: "producer identity and asset inventory",
			mutate: func(manifest *Manifest) {
				manifest.Producer = Producer{Version: "v9.9.9", Commit: "forged"}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newTestRepository(t)
			repository := openTestRepository(t, root)
			bundle := testBundle(t, "v1.2.3", "abc123")
			if _, err := repository.Install(bundle, "generic"); err != nil {
				t.Fatal(err)
			}
			commitTestManifest(t, root)

			customSkill := InstalledRoot + "/skills/my-skill/SKILL.md"
			customContent := []byte("# My skill\n")
			writeTestFile(t, root, customSkill, customContent)
			manifest := bundle.Manifest
			test.mutate(&manifest)
			manifest.Assets = append(manifest.Assets, Asset{
				Path:   "payload/" + customSkill,
				SHA256: digest(customContent),
				Size:   int64(len(customContent)),
				Mode:   formatAssetMode(0o644),
			})
			manifestData, err := marshalManifest(manifest)
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, root, InstalledManifestPath, manifestData)

			report, err := repository.Check(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if !containsString(report.Modified, InstalledManifestPath) {
				t.Fatalf("manifest drift not reported as modified: %+v", report)
			}
			plan, err := repository.PlanUpdate(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if !containsString(plan.ModifiedOwned, InstalledManifestPath) {
				t.Fatalf("manifest drift not acknowledged: %v", plan.ModifiedOwned)
			}
			for _, change := range plan.Changes {
				if change.Path == customSkill {
					t.Fatalf("user-created skill was treated as an owned change: %+v", change)
				}
			}
			if err := repository.ApplyUpdate(plan, false); err == nil {
				t.Fatal("manifest drift was replaced without acknowledgement")
			}
			if err := repository.ApplyUpdate(plan, true); err != nil {
				t.Fatal(err)
			}
			if got := readTestFile(t, root, customSkill); !bytes.Equal(got, customContent) {
				t.Fatalf("user-created skill changed to %q", got)
			}
		})
	}
}

func TestInterruptedUpdateRecoversWithoutUserCollision(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	oldBundle := testBundle(t, "v1.2.3", "abc123")
	newBundle := testBundle(t, "v2.0.0", "def456")
	addedPath := InstalledRoot + "/future.md"
	addedFile := File{Path: addedPath, Data: []byte("new product asset\n"), Mode: 0o644}
	newBundle.Files[addedPath] = addedFile
	newBundle.Manifest.Assets = append(newBundle.Manifest.Assets, Asset{
		Path:   "payload/" + addedPath,
		SHA256: digest(addedFile.Data),
		Size:   int64(len(addedFile.Data)),
		Mode:   formatAssetMode(addedFile.Mode),
	})
	var err error
	newBundle.ManifestJSON, err = marshalManifest(newBundle.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Install(oldBundle, "generic"); err != nil {
		t.Fatal(err)
	}
	commitTestManifest(t, root)
	customSkill := InstalledRoot + "/skills/my-skill/SKILL.md"
	writeTestFile(t, root, customSkill, []byte("# My skill\n"))

	plan, err := repository.PlanUpdate(newBundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.writeUpdateTransaction(plan.Changes); err != nil {
		t.Fatal(err)
	}
	appliedAdd := false
	for _, change := range plan.Changes {
		if change.Path == addedPath {
			if err := repository.applyUpdateChange(change); err != nil {
				t.Fatal(err)
			}
			appliedAdd = true
			break
		}
	}
	if !appliedAdd {
		t.Fatal("update plan did not add the recovery fixture")
	}

	releasePath := InstalledRoot + "/release.json"
	releaseBeforePlan := readTestFile(t, root, releasePath)
	retry, err := repository.PlanUpdate(newBundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Changes) == 0 || len(retry.UserCollisions) != 0 {
		t.Fatalf("interrupted update plan = %+v", retry)
	}
	if got := readTestFile(t, root, releasePath); !bytes.Equal(got, releaseBeforePlan) {
		t.Fatalf("planning resumed interrupted update: got %q, want %q", got, releaseBeforePlan)
	}
	if err := repository.ApplyUpdate(retry, false); err != nil {
		t.Fatal(err)
	}
	if got := string(readTestFile(t, root, customSkill)); got != "# My skill\n" {
		t.Fatalf("resumed update changed user skill to %q", got)
	}
	report, err := repository.Check(newBundle)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "current" {
		t.Fatalf("recovered update check = %+v", report)
	}
}

func TestReadOnlyOperationsNeverResumeUntrustedUpdateTransaction(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	bundle := testBundle(t, "v1.2.3", "abc123")
	if _, err := repository.Install(bundle, "generic"); err != nil {
		t.Fatal(err)
	}
	customSkill := InstalledRoot + "/skills/my-skill/SKILL.md"
	customContent := []byte("# My skill\n")
	writeTestFile(t, root, customSkill, customContent)
	transactionData, err := json.Marshal(updateTransaction{
		Version: updateTransactionVersion,
		Changes: []Change{{
			Path: customSkill,
			Kind: ChangeDelete,
			Old:  customContent,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, updateTransactionPath, append(transactionData, '\n'))

	report, err := repository.Check(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "current" {
		t.Fatalf("check report = %+v", report)
	}
	if got := readTestFile(t, root, customSkill); !bytes.Equal(got, customContent) {
		t.Fatalf("check applied untrusted transaction: %q", got)
	}
	if _, err := repository.PlanUpdate(bundle); err == nil {
		t.Fatal("dry-run planning accepted an untrusted transaction")
	}
	if got := readTestFile(t, root, customSkill); !bytes.Equal(got, customContent) {
		t.Fatalf("planning applied untrusted transaction: %q", got)
	}
}

func TestInterruptedUpdateRejectsInventedSourceManifest(t *testing.T) {
	root := newTestRepository(t)
	repository := openTestRepository(t, root)
	currentBundle := testBundle(t, "v2.0.0", "def456")
	if _, err := repository.Install(currentBundle, "generic"); err != nil {
		t.Fatal(err)
	}
	customSkill := InstalledRoot + "/skills/my-skill/SKILL.md"
	customContent := []byte("# My skill\n")
	writeTestFile(t, root, customSkill, customContent)

	inventedSource := testBundle(t, "v1.2.3", "abc123")
	inventedSource.Manifest.Assets = append(inventedSource.Manifest.Assets, Asset{
		Path:   "payload/" + customSkill,
		SHA256: digest(customContent),
		Size:   int64(len(customContent)),
		Mode:   formatAssetMode(0o644),
	})
	inventedManifest, err := marshalManifest(inventedSource.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	transactionData, err := json.Marshal(updateTransaction{
		Version: updateTransactionVersion,
		Changes: []Change{
			{
				Path: customSkill,
				Kind: ChangeDelete,
				Old:  customContent,
			},
			{
				Path: InstalledManifestPath,
				Kind: ChangeModify,
				Old:  inventedManifest,
				New:  currentBundle.ManifestJSON,
				Mode: 0o644,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, updateTransactionPath, append(transactionData, '\n'))

	if _, err := repository.PlanUpdate(currentBundle); err == nil {
		t.Fatal("interrupted update trusted an invented source ownership manifest")
	}
	if got := readTestFile(t, root, customSkill); !bytes.Equal(got, customContent) {
		t.Fatalf("invented source manifest deleted user skill: %q", got)
	}
}

func TestRepositoryRejectsTraversalAndSymlinkEscapes(t *testing.T) {
	root := newTestRepository(t)
	traversal := root + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(root)
	if _, err := OpenRepository(traversal); err == nil {
		t.Fatal("parent traversal target was accepted")
	}

	link := filepath.Join(resolvedTestTempDir(t), "repo-link")
	if err := os.Symlink(root, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := OpenRepository(link); err == nil {
		t.Fatal("symbolic-link repository target was accepted")
	}

	intermediateRoot := resolvedTestTempDir(t)
	intermediateLink := filepath.Join(intermediateRoot, "link")
	if err := os.Symlink(filepath.Dir(root), intermediateLink); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRepository(filepath.Join(intermediateLink, filepath.Base(root))); err == nil {
		t.Fatal("repository target with an intermediate symbolic link was accepted")
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
	root := resolvedTestTempDir(t)
	runTestGit(t, root, "init", "--quiet")
	return root
}

func resolvedTestTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func commitTestManifest(t *testing.T, root string) {
	t.Helper()
	runTestGit(t, root, "add", "--", InstalledManifestPath)
	runTestGit(t, root, "commit", "--quiet", "-m", "install agent toolkit")
}

func runTestGit(t *testing.T, root string, args ...string) {
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
