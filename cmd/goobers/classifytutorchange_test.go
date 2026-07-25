package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const classifyWorkflowBase = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
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
      next: gate-a
  gates:
    - name: gate-a
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: done
        fail: "@abort"
`

// gitRepoWithFileChange builds a temp git repo with path committed as
// baseContent on main, then a run branch that rewrites it to newContent —
// mirroring the worktree gate-removal-guard/classify-tutor-change stages run
// against (see gitRepoWithGateChange in gateremovalguard_test.go).
func gitRepoWithFileChange(t *testing.T, path, baseContent, newContent string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
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
	if err := os.WriteFile(full, []byte(baseContent), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", "goobers/tutor/run-1")
	if err := os.WriteFile(full, []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "tutor change")
	return dir
}

func readClassification(t *testing.T, dir string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "tutor-change-class.json"))
	if err != nil {
		t.Fatalf("read tutor-change-class.json: %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal tutor-change-class.json: %v", err)
	}
	return result
}

func TestClassifyTutorChangeStructureForTopologyChange(t *testing.T) {
	root := initDemo(t)
	newYAML := strings.Replace(classifyWorkflowBase,
		`  gates:
    - name: gate-a
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: done
        fail: "@abort"
`, "  gates: []\n", 1)
	wt := gitRepoWithFileChange(t, "selfhost/gaggles/goobers/workflows/example.yaml", classifyWorkflowBase, newYAML)
	t.Chdir(wt)

	code, stdout, stderr := runArgs(t, "classify-tutor-change", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "structure") {
		t.Fatalf("stdout = %q, want structure classification", stdout)
	}
	result := readClassification(t, wt)
	if result["category"] != "structure" || result["requiresSignoff"] != "true" {
		t.Fatalf("result = %+v, want structure/true", result)
	}
}

func TestClassifyTutorChangeGateTuneForGateFieldChange(t *testing.T) {
	root := initDemo(t)
	newYAML := strings.Replace(classifyWorkflowBase, "check: status-equals", "check: output-numeric-lte", 1)
	wt := gitRepoWithFileChange(t, "selfhost/gaggles/goobers/workflows/example.yaml", classifyWorkflowBase, newYAML)
	t.Chdir(wt)

	code, stdout, stderr := runArgs(t, "classify-tutor-change", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "gate-tune") {
		t.Fatalf("stdout = %q, want gate-tune classification", stdout)
	}
	result := readClassification(t, wt)
	if result["category"] != "gate-tune" || result["requiresSignoff"] != "false" {
		t.Fatalf("result = %+v, want gate-tune/false", result)
	}
}

func TestClassifyTutorChangePersonaForInstructionsChange(t *testing.T) {
	root := initDemo(t)
	wt := gitRepoWithFileChange(t, "selfhost/gaggles/goobers/goobers/coder/instructions.md", "old prose\n", "new prose\n")
	t.Chdir(wt)

	code, stdout, stderr := runArgs(t, "classify-tutor-change", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "persona") {
		t.Fatalf("stdout = %q, want persona classification", stdout)
	}
	result := readClassification(t, wt)
	if result["category"] != "persona" || result["requiresSignoff"] != "false" {
		t.Fatalf("result = %+v, want persona/false", result)
	}
}

func TestClassifyTutorChangeStructureForSkillsChange(t *testing.T) {
	root := initDemo(t)
	baseGoober := `apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: coder
spec:
  gaggle: goobers
  role: coder
  instructions: instructions.md
  skills:
    - review-basics
`
	newGoober := strings.Replace(baseGoober, "- review-basics\n", "- review-basics\n    - new-skill\n", 1)
	wt := gitRepoWithFileChange(t, "selfhost/gaggles/goobers/goobers/coder/goober.yaml", baseGoober, newGoober)
	t.Chdir(wt)

	code, stdout, stderr := runArgs(t, "classify-tutor-change", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "structure") {
		t.Fatalf("stdout = %q, want structure classification", stdout)
	}
	result := readClassification(t, wt)
	if result["category"] != "structure" || result["requiresSignoff"] != "true" {
		t.Fatalf("result = %+v, want structure/true", result)
	}
}

func TestClassifyTutorChangeStructureForUnrecognizedFile(t *testing.T) {
	root := initDemo(t)
	wt := gitRepoWithFileChange(t, "selfhost/gaggles/goobers/some-other-file.txt", "old\n", "new\n")
	t.Chdir(wt)

	code, stdout, stderr := runArgs(t, "classify-tutor-change", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "structure") {
		t.Fatalf("stdout = %q, want structure classification for an unrecognized file kind", stdout)
	}
}

// TestLabelTutorChangeCategory covers TUT-A6's labeling half: structure gets
// the sign-off label and never the informational persona/gate-tune ones, and
// a repass that reclassifies swaps the label rather than accumulating.
func TestLabelTutorChangeCategory(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	t.Run("structure requires signoff", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(30, "tutor pr")
		provider := server.newGitHubProvider("token")
		if err := labelTutorChangeCategory(context.Background(), provider, repo, 30, "structure", true); err != nil {
			t.Fatalf("labelTutorChangeCategory: %v", err)
		}
		if !issueHasLabel(server, 30, tutorSignoffRequiredLabel) {
			t.Fatal("expected tutor:needs-signoff to be applied")
		}
		if issueHasLabel(server, 30, tutorPersonaLabel) || issueHasLabel(server, 30, tutorGateTuneLabel) {
			t.Fatal("no informational label should coexist with the sign-off label")
		}
	})

	t.Run("gate-tune gets the informational label, no signoff", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(31, "tutor pr")
		provider := server.newGitHubProvider("token")
		if err := labelTutorChangeCategory(context.Background(), provider, repo, 31, "gate-tune", false); err != nil {
			t.Fatalf("labelTutorChangeCategory: %v", err)
		}
		if !issueHasLabel(server, 31, tutorGateTuneLabel) {
			t.Fatal("expected tutor:gate-tune-category to be applied")
		}
		if issueHasLabel(server, 31, tutorSignoffRequiredLabel) {
			t.Fatal("tutor:needs-signoff must not be applied for gate-tune")
		}
	})

	t.Run("persona gets the informational label, no signoff", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(32, "tutor pr")
		provider := server.newGitHubProvider("token")
		if err := labelTutorChangeCategory(context.Background(), provider, repo, 32, "persona", false); err != nil {
			t.Fatalf("labelTutorChangeCategory: %v", err)
		}
		if !issueHasLabel(server, 32, tutorPersonaLabel) {
			t.Fatal("expected tutor:persona to be applied")
		}
		if issueHasLabel(server, 32, tutorSignoffRequiredLabel) {
			t.Fatal("tutor:needs-signoff must not be applied for persona")
		}
	})

	t.Run("reclassification swaps the label", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(33, "tutor pr")
		provider := server.newGitHubProvider("token")
		if err := labelTutorChangeCategory(context.Background(), provider, repo, 33, "structure", true); err != nil {
			t.Fatalf("labelTutorChangeCategory (structure): %v", err)
		}
		if err := labelTutorChangeCategory(context.Background(), provider, repo, 33, "persona", false); err != nil {
			t.Fatalf("labelTutorChangeCategory (persona): %v", err)
		}
		if issueHasLabel(server, 33, tutorSignoffRequiredLabel) {
			t.Fatal("tutor:needs-signoff should have been cleared on reclassification to persona")
		}
		if !issueHasLabel(server, 33, tutorPersonaLabel) {
			t.Fatal("expected tutor:persona after reclassification")
		}
	})
}

// TestTutorChangeClassificationFromJournal covers the read-back half: open-pr
// recovers classify-tutor-change's classification straight from the
// journal's stage.finished Outputs.
func TestTutorChangeClassificationFromJournal(t *testing.T) {
	root := initDemo(t)
	const runID = "run-1"

	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "tutor", WorkflowDigest: journal.Digest([]byte("workflow")),
		Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "classify-tutor-change", Attempt: 1, Status: "success",
		Outputs: map[string]any{"category": "structure", "requiresSignoff": "true"},
	}); err != nil {
		t.Fatalf("record classify-tutor-change stage: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	category, requiresSignoff := tutorChangeClassificationFromJournal(root, runID)
	if category != "structure" || requiresSignoff != "true" {
		t.Fatalf("category, requiresSignoff = %q, %q; want structure, true", category, requiresSignoff)
	}
}
