package journal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedReplacementSurvivesFinalizationErrors(t *testing.T) {
	for _, step := range []string{"sync", "cleanup"} {
		t.Run(step, func(t *testing.T) {
			runsDir := t.TempDir()
			finalDir := recordedRun(t, runsDir, testIdentity(), "original journal")
			backupDir, err := stashSupersededRun(finalDir)
			if err != nil {
				t.Fatal(err)
			}
			stagedDir := recordedRun(t, t.TempDir(), testIdentity(), "replacement journal")
			if err := os.Rename(stagedDir, finalDir); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("injected " + step + " failure")
			syncDir := func(path string) error {
				if path != runsDir {
					t.Fatalf("synced %q, want %q", path, runsDir)
				}
				if step == "sync" {
					return failure
				}
				return nil
			}
			removeAll := func(path string) error {
				if step == "sync" {
					t.Fatal("removed backup before publication durability was confirmed")
				}
				if path != filepath.Dir(backupDir) {
					t.Fatalf("removed %q, want backup container", path)
				}
				return failure
			}
			published, err := finishRunReplacement(finalDir, backupDir, syncDir, removeAll)
			if !published || !errors.Is(err, failure) || !strings.Contains(err.Error(), backupDir) {
				t.Fatalf("publication finalization = %v, %v", published, err)
			}
			if got := readRunInput(t, finalDir); got != "replacement journal" {
				t.Fatalf("published directory = %q", got)
			}
			if got := readRunInput(t, backupDir); got != "original journal" {
				t.Fatalf("retained backup = %q", got)
			}
		})
	}
}

// recordedRun creates and closes a run journal under a fresh runs directory,
// returning the published run directory.
func recordedRun(t *testing.T, runsDir string, id RunIdentity, input string) string {
	t.Helper()
	run, err := Create(runsDir, id, map[string][]byte{"issue.md": []byte(input)}, WithClock(constClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return run.Dir()
}

func readRunInput(t *testing.T, runDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(runDir, dirInputs, "*"))
	if err != nil {
		t.Fatalf("glob run inputs: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("run inputs = %v, want exactly one", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read run input: %v", err)
	}
	return string(data)
}

func assertNoStagingResidue(t *testing.T, runsDir string) {
	t.Helper()
	entries, err := os.ReadDir(RunCreationStagingDir(runsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read staging root: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("replacement left staging residue: %v", names)
	}
}

// TestReplaceRunRestoresSupersededRunWhenPublicationFails is the #3641
// regression: a failed publication must not leave the run id with no journal
// at all. The old ReplaceRun removed finalDir before renaming the stage into
// place, so any rename, permission, or sharing failure destroyed the old
// journal as well as the replacement (whose staging the caller then cleans
// up). The superseded directory must be back at finalDir, intact.
func TestReplaceRunRestoresSupersededRunWhenPublicationFails(t *testing.T) {
	runsDir := t.TempDir()
	finalDir := recordedRun(t, runsDir, testIdentity(), "original journal")

	// A staged directory that does not exist: the publication rename fails
	// after the superseded run has been moved aside.
	stagedDir := filepath.Join(RunCreationStagingDir(runsDir), "missing-stage")

	replaced, err := ReplaceRun(finalDir, stagedDir, nil)
	if err == nil {
		t.Fatal("ReplaceRun succeeded with a missing staged directory")
	}
	if replaced {
		t.Fatal("ReplaceRun reported a publication that failed")
	}
	if !Recorded(finalDir) {
		t.Fatalf("failed publication destroyed the superseded run directory %s", finalDir)
	}
	if got := readRunInput(t, finalDir); got != "original journal" {
		t.Fatalf("restored run input = %q, want %q", got, "original journal")
	}
	assertNoStagingResidue(t, runsDir)
}

// TestReplaceRunPublishesStagedRun certifies the success path is unchanged:
// the staged journal is published at finalDir and the backup of the
// superseded journal is removed rather than left behind.
func TestReplaceRunPublishesStagedRun(t *testing.T) {
	runsDir := t.TempDir()
	finalDir := recordedRun(t, runsDir, testIdentity(), "original journal")

	stageRoot := t.TempDir()
	stagedDir := recordedRun(t, stageRoot, testIdentity(), "replacement journal")

	replaced, err := ReplaceRun(finalDir, stagedDir, nil)
	if err != nil {
		t.Fatalf("ReplaceRun: %v", err)
	}
	if !replaced {
		t.Fatal("ReplaceRun did not publish the staged run")
	}
	if got := readRunInput(t, finalDir); got != "replacement journal" {
		t.Fatalf("published run input = %q, want %q", got, "replacement journal")
	}
	if _, err := os.Stat(stagedDir); !os.IsNotExist(err) {
		t.Fatalf("staged directory still present after publication: %v", err)
	}
	assertNoStagingResidue(t, runsDir)
}

// TestReplaceRunKeepsExistingRun certifies the keep hook still aborts the
// replacement without disturbing either directory — the superseded journal is
// never moved aside when the caller decides to keep it.
func TestReplaceRunKeepsExistingRun(t *testing.T) {
	runsDir := t.TempDir()
	finalDir := recordedRun(t, runsDir, testIdentity(), "original journal")

	stageRoot := t.TempDir()
	stagedDir := recordedRun(t, stageRoot, testIdentity(), "replacement journal")

	replaced, err := ReplaceRun(finalDir, stagedDir, func() (bool, error) { return true, nil })
	if err != nil {
		t.Fatalf("ReplaceRun: %v", err)
	}
	if replaced {
		t.Fatal("ReplaceRun published over a run the caller kept")
	}
	if got := readRunInput(t, finalDir); got != "original journal" {
		t.Fatalf("kept run input = %q, want %q", got, "original journal")
	}
	if !Recorded(stagedDir) {
		t.Fatalf("kept replacement consumed the staged run directory %s", stagedDir)
	}
	assertNoStagingResidue(t, runsDir)
}
