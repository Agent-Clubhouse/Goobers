package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
)

func TestScaffoldTemplatesGolden(t *testing.T) {
	for _, tc := range []struct {
		name     string
		template string
		golden   string
		data     scaffoldTemplateData
	}{
		{"goober", "templates/scaffold/goober.yaml.tmpl", "testdata/scaffold/goober.yaml.golden", scaffoldTemplateData{Name: "reviewer2", Gaggle: "example"}},
		{"instructions", "templates/scaffold/instructions.md.tmpl", "testdata/scaffold/instructions.md.golden", scaffoldTemplateData{Name: "reviewer2", Gaggle: "example"}},
		{"workflow", "templates/scaffold/workflow.yaml.tmpl", "testdata/scaffold/workflow.yaml.golden", scaffoldTemplateData{Name: "my-flow", Gaggle: "example", Goober: "reviewer2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderScaffoldTemplate(tc.template, tc.data)
			if err != nil {
				t.Fatalf("renderScaffoldTemplate: %v", err)
			}
			want, err := os.ReadFile(tc.golden)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if string(got) != string(want) {
				t.Fatalf("rendered template differs from %s\n--- got ---\n%s\n--- want ---\n%s", tc.golden, got, want)
			}
		})
	}
}

func TestScaffoldGooberAndWorkflowValidateClean(t *testing.T) {
	root := initDemo(t)
	gaggleDir := filepath.Join(root, "config", "gaggles", "example")

	code, stdout, stderr := runArgs(t, "scaffold", "goober", "reviewer2", gaggleDir)
	if code != 0 {
		t.Fatalf("scaffold goober: code=%d stderr=%q", code, stderr)
	}
	wantGooberOutput := "created " + filepath.Join(gaggleDir, "goobers", "reviewer2", "goober.yaml") + "\n" +
		"created " + filepath.Join(gaggleDir, "goobers", "reviewer2", "instructions.md") + "\n" +
		"next: goobers validate " + root + "\n"
	if stdout != wantGooberOutput {
		t.Fatalf("scaffold goober stdout = %q, want %q", stdout, wantGooberOutput)
	}

	code, stdout, stderr = runArgs(t, "scaffold", "workflow", "my-flow", root)
	if code != 0 {
		t.Fatalf("scaffold workflow: code=%d stderr=%q", code, stderr)
	}
	workflowPath := filepath.Join(gaggleDir, "workflows", "my-flow.yaml")
	wantWorkflowOutput := "created " + workflowPath + "\n" +
		"next: goobers run my-flow " + root + "\n"
	if stdout != wantWorkflowOutput {
		t.Fatalf("scaffold workflow stdout = %q, want %q", stdout, wantWorkflowOutput)
	}

	for path, golden := range map[string]string{
		filepath.Join(gaggleDir, "goobers", "reviewer2", "goober.yaml"):     "testdata/scaffold/goober.yaml.golden",
		filepath.Join(gaggleDir, "goobers", "reviewer2", "instructions.md"): "testdata/scaffold/instructions.md.golden",
		workflowPath: "testdata/scaffold/workflow.yaml.golden",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read scaffold %s: %v", path, err)
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("read golden %s: %v", golden, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from %s", path, golden)
		}
	}

	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "WARNING") {
		t.Fatalf("validate emitted a warning: %q", stdout)
	}
	if !strings.Contains(stdout, "2 goober(s), 2 workflow(s)") {
		t.Fatalf("validate did not load both scaffolds: %q", stdout)
	}
}

func TestScaffoldScalarNamesValidateClean(t *testing.T) {
	root := initDemo(t)

	if code, _, stderr := runArgs(t, "scaffold", "goober", "123", root); code != 0 {
		t.Fatalf("scaffold goober: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := runArgs(t, "scaffold", "workflow", "true", root); code != 0 {
		t.Fatalf("scaffold workflow: code=%d stderr=%q", code, stderr)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "WARNING") {
		t.Fatalf("validate emitted a warning: %q", stdout)
	}
}

func TestScaffoldRefusesOverwriteUnlessForced(t *testing.T) {
	root := initDemo(t)
	dir := filepath.Join(root, "config", "gaggles", "example", "goobers", "reviewer2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gooberPath := filepath.Join(dir, "goober.yaml")
	if err := os.WriteFile(gooberPath, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "scaffold", "goober", "reviewer2", root)
	if code != 1 || !strings.Contains(stderr, "refusing to overwrite") {
		t.Fatalf("without force: code=%d stderr=%q", code, stderr)
	}
	got, err := os.ReadFile(gooberPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep me\n" {
		t.Fatalf("existing file was changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "instructions.md")); !os.IsNotExist(err) {
		t.Fatalf("instructions.md was created despite all-file overwrite preflight: %v", err)
	}

	code, _, stderr = runArgs(t, "scaffold", "goober", "reviewer2", "--force", root)
	if code != 0 {
		t.Fatalf("with force: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "instructions.md")); err != nil {
		t.Fatalf("forced scaffold did not create instructions.md: %v", err)
	}
}

func TestScaffoldRejectsInvalidName(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "scaffold", "goober", "../escape", root)
	if code != 2 || !strings.Contains(stderr, "invalid name") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestScaffoldRejectsSymlinkedDestinationDirectory(t *testing.T) {
	root := initDemo(t)
	if code, _, stderr := runArgs(t, "scaffold", "goober", "reviewer2", root); code != 0 {
		t.Fatalf("scaffold goober: code=%d stderr=%q", code, stderr)
	}

	gaggleDir := filepath.Join(root, "config", "gaggles", "example")
	workflowsDir := filepath.Join(gaggleDir, "workflows")
	if err := os.RemoveAll(workflowsDir); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, workflowsDir); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "scaffold", "workflow", "my-flow", "--force", root)
	if code != 1 || !strings.Contains(stderr, "symlinked directory") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(outside, "my-flow.yaml")); !os.IsNotExist(err) {
		t.Fatalf("scaffold escaped through workflows symlink: %v", err)
	}
}

func TestScaffoldedWorkflowRunsEndToEnd(t *testing.T) {
	root := initDemo(t)
	if code, _, stderr := runArgs(t, "scaffold", "goober", "reviewer2", root); code != 0 {
		t.Fatalf("scaffold goober: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := runArgs(t, "scaffold", "workflow", "my-flow", root); code != 0 {
		t.Fatalf("scaffold workflow: code=%d stderr=%q", code, stderr)
	}

	fixtureRepo := newDaemonFixtureRepo(t)
	prevRepo := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil }
	prevAdapter := newAgenticAdapter
	newAgenticAdapter = func(string, map[string]string) harness.Adapter {
		return &harness.FakeAdapter{Act: func(_ context.Context, req harness.RunRequest) error {
			return harness.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Summary: "scaffolded agentic stage completed",
			})
		}}
	}
	t.Cleanup(func() {
		repoCloneURL = prevRepo
		newAgenticAdapter = prevAdapter
	})

	code, stdout, stderr := runArgs(t, "run", "my-flow", root)
	if code != 0 {
		t.Fatalf("run scaffolded workflow: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "phase=completed") {
		t.Fatalf("scaffolded run did not complete: %q", stdout)
	}

	runID := runIDFromRunStdout(t, stdout)
	reader, err := journal.OpenRead(filepath.Join(root, "runs", runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	sawPrepare := false
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Stage == "prepare" {
			sawResultArtifact := false
			for _, artifact := range event.Artifacts {
				if artifact.MediaType == "application/json" {
					sawResultArtifact = true
				}
			}
			sawPrepare = event.Status == string(apiv1.ResultSuccess) &&
				event.Outputs["ready"] == "true" &&
				sawResultArtifact
		}
	}
	if !sawPrepare {
		t.Fatalf("shell stage did not produce its declared resultFile output and artifact: %+v", events)
	}
}
