package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/testgit"
)

const gateRemovalGuardWorkflowBase = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: example
spec:
  gaggle: goobers
  triggers:
    - type: manual
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: build it
      run:
        command: ["true"]
      next: local-ci-gate
  gates:
    - name: local-ci-gate
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: done
        fail: "@abort"
`

// gitRepoWithGateChange builds a temp git repo with workflowYAML committed on
// main at path, then a run branch ("goobers/tutor/run-1") that rewrites it to
// newWorkflowYAML — mirroring the base/run-branch shape gitRepoWithGateChange
// gives gate-removal-guard: a worktree checked out to the run branch, so
// `git show main:path` (base) and the worktree's own file (HEAD) diverge
// exactly the way a real tutor draft-change commit would.
func gitRepoWithGateChange(t *testing.T, path, workflowYAML, newWorkflowYAML string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := testgit.Command(args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "tutor@example.test")
	git("config", "user.name", "tutor")
	if err := os.WriteFile(full, []byte(workflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", "goobers/tutor/run-1")
	if err := os.WriteFile(full, []byte(newWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "tutor change")
	return dir
}

// seedTutorFindingJournal fabricates a run journal whose "analyze" stage
// recorded finding.md as its artifact — the exact shape gate-removal-guard
// reads via findingMetaFromJournal.
func seedTutorFindingJournal(t *testing.T, root, runID, findingMD string) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "tutor", WorkflowDigest: journal.Digest([]byte("workflow")),
		Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	ref, err := run.RecordArtifact("finding.md", []byte(findingMD))
	if err != nil {
		t.Fatalf("record finding.md: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "analyze", Attempt: 1, Status: "success",
		Artifacts: []journal.Ref{ref},
	}); err != nil {
		t.Fatalf("record analyze stage: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

func gateNoiseFinding(subject, independentProof string) string {
	var b strings.Builder
	b.WriteString("---\nkind: gate-never-fails\nsubject: " + subject + "\n")
	if independentProof != "" {
		b.WriteString("independentProof: |\n  " + independentProof + "\n")
	}
	b.WriteString("---\n\n## Finding\n\n" + subject + " never fails.\n")
	return b.String()
}

func TestGateRemovalGuardBlocksRemovalWithoutProof(t *testing.T) {
	root := initDemo(t)
	const runID = "run-1"
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "tutor")
	seedTutorFindingJournal(t, root, runID, gateNoiseFinding("local-ci-gate", ""))

	wt := gitRepoWithGateChange(t, "reference-workflows/gaggles/goobers/workflows/example.yaml",
		gateRemovalGuardWorkflowBase, strings.Replace(gateRemovalGuardWorkflowBase,
			`  gates:
    - name: local-ci-gate
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: done
        fail: "@abort"
`, "  gates: []\n", 1))
	t.Chdir(wt)

	code, _, stderr := runArgs(t, "gate-removal-guard", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (blocked); stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "TUT-A3") || !strings.Contains(stderr, "local-ci-gate") {
		t.Fatalf("stderr = %q, want a TUT-A3 block naming local-ci-gate", stderr)
	}
}

func TestGateRemovalGuardAllowsRemovalWithIndependentProof(t *testing.T) {
	root := initDemo(t)
	const runID = "run-1"
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "tutor")
	seedTutorFindingJournal(t, root, runID, gateNoiseFinding("local-ci-gate",
		"Manual audit (run-482) confirms the underlying binary was removed in #900; the check has been a permanent no-op since."))

	wt := gitRepoWithGateChange(t, "reference-workflows/gaggles/goobers/workflows/example.yaml",
		gateRemovalGuardWorkflowBase, strings.Replace(gateRemovalGuardWorkflowBase,
			`  gates:
    - name: local-ci-gate
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: done
        fail: "@abort"
`, "  gates: []\n", 1))
	t.Chdir(wt)

	code, stdout, stderr := runArgs(t, "gate-removal-guard", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (proof allows removal); stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "removed") {
		t.Fatalf("stdout = %q, want the removed classification", stdout)
	}
	data, err := os.ReadFile(filepath.Join(wt, "gate-edit.json"))
	if err != nil {
		t.Fatalf("read gate-edit.json: %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal gate-edit.json: %v", err)
	}
	if result["gateEdit"] != "removed" || result["subject"] != "local-ci-gate" {
		t.Fatalf("gate-edit.json = %+v, want removed/local-ci-gate", result)
	}
}

func TestGateRemovalGuardBlocksFailBranchRedirectWithoutProof(t *testing.T) {
	root := initDemo(t)
	const runID = "run-1"
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "tutor")
	seedTutorFindingJournal(t, root, runID, gateNoiseFinding("local-ci-gate", ""))

	newYAML := strings.Replace(gateRemovalGuardWorkflowBase, `fail: "@abort"`, "fail: done", 1)
	wt := gitRepoWithGateChange(t, "reference-workflows/gaggles/goobers/workflows/example.yaml", gateRemovalGuardWorkflowBase, newYAML)
	t.Chdir(wt)

	code, _, stderr := runArgs(t, "gate-removal-guard", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (loosened fail branch blocked); stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "loosened") {
		t.Fatalf("stderr = %q, want the loosened classification named", stderr)
	}
}

func TestGateRemovalGuardAllowsTuningWithoutProof(t *testing.T) {
	root := initDemo(t)
	const runID = "run-1"
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "tutor")
	seedTutorFindingJournal(t, root, runID, gateNoiseFinding("local-ci-gate", ""))

	newYAML := strings.Replace(gateRemovalGuardWorkflowBase, "check: status-equals", "check: output-numeric-lte", 1)
	wt := gitRepoWithGateChange(t, "reference-workflows/gaggles/goobers/workflows/example.yaml", gateRemovalGuardWorkflowBase, newYAML)
	t.Chdir(wt)

	code, stdout, stderr := runArgs(t, "gate-removal-guard", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (tuning needs no proof); stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "tuning") {
		t.Fatalf("stdout = %q, want the tuning classification", stdout)
	}
}

func TestGateRemovalGuardNoOpWhenFindingIsNotGateNoise(t *testing.T) {
	root := initDemo(t)
	const runID = "run-1"
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "tutor")
	seedTutorFindingJournal(t, root, runID, "---\nkind: stage-failure-rate\n---\n\n## Finding\n\nunrelated.\n")

	newYAML := strings.Replace(gateRemovalGuardWorkflowBase,
		`  gates:
    - name: local-ci-gate
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: done
        fail: "@abort"
`, "  gates: []\n", 1)
	wt := gitRepoWithGateChange(t, "reference-workflows/gaggles/goobers/workflows/example.yaml", gateRemovalGuardWorkflowBase, newYAML)
	t.Chdir(wt)

	code, stdout, stderr := runArgs(t, "gate-removal-guard", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (non-gate-noise finding is a no-op); stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "not gate-noise") {
		t.Fatalf("stdout = %q, want the pass-through message", stdout)
	}
}

func TestGateRemovalGuardNoOpWhenNoFindingArtifact(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-without-journal")
	t.Setenv("GOOBERS_WORKFLOW", "tutor")

	newYAML := strings.Replace(gateRemovalGuardWorkflowBase, "check: status-equals", "check: output-numeric-lte", 1)
	wt := gitRepoWithGateChange(t, "reference-workflows/gaggles/goobers/workflows/example.yaml", gateRemovalGuardWorkflowBase, newYAML)
	t.Chdir(wt)

	code, _, stderr := runArgs(t, "gate-removal-guard", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (no journal at all is a no-op); stderr = %q", code, stderr)
	}
}
