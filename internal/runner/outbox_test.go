package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

type oversizedOutboxDeterministic struct {
	rec   ArtifactRecorder
	calls int
}

type repassOutboxDeterministic struct{ calls int }

func (d *repassOutboxDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.calls++
	data := []byte(`{"execution":` + strconv.Itoa(d.calls) + `}`)
	path := filepath.Join(env.Workspace, "evidence", "failure.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "validation evidence"}, nil
}

type repassThenPassGate struct{ calls int }

func (g *repassThenPassGate) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	g.calls++
	if g.calls == 1 {
		return gate.OutcomeFail, nil
	}
	return gate.OutcomePass, nil
}

func (d *oversizedOutboxDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.calls++
	if strings.HasSuffix(env.TaskID, ":after") {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	evidence := filepath.Join(env.Workspace, "evidence")
	if err := os.MkdirAll(evidence, 0o755); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	for name, size := range map[string]int64{
		"largest.trx": 40 << 20,
		"second.trx":  30 << 20,
	} {
		file, err := os.OpenFile(filepath.Join(evidence, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		err = file.Truncate(size)
		closeErr := file.Close()
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		if closeErr != nil {
			return apiv1.ResultEnvelope{}, closeErr
		}
	}
	ref, err := d.rec.RecordArtifact("comparison-result.json", []byte(`{"verdict":"failed"}`))
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Summary: "comparison failed",
		Error:   &apiv1.ErrorInfo{Code: "comparison_failed", Message: "51 shared failures"},
		Outputs: map[string]interface{}{"verdict": "failed"},
		Artifacts: []apiv1.ArtifactPointer{{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size, Integrity: ref.Integrity,
		}},
		Metrics: map[string]float64{"exitCode": 1},
	}, nil
}

func writeWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", full, err)
	}
}

func TestCollectOutboxFilesSingleFile(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "report.json", `{"ok":true}`)

	files, err := collectOutboxFiles(root, []string{"report.json"})
	if err != nil {
		t.Fatalf("collectOutboxFiles: %v", err)
	}
	if len(files) != 1 || files[0].RelPath != "report.json" || string(files[0].Data) != `{"ok":true}` {
		t.Fatalf("files = %+v, want one report.json entry", files)
	}
}

func TestCollectOutboxFilesDirectoryRecursion(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "reports/a.txt", "a")
	writeWorkspaceFile(t, root, "reports/nested/b.txt", "b")

	files, err := collectOutboxFiles(root, []string{"reports"})
	if err != nil {
		t.Fatalf("collectOutboxFiles: %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.RelPath] = string(f.Data)
	}
	want := map[string]string{"reports/a.txt": "a", "reports/nested/b.txt": "b"}
	if len(got) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("file %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestCollectOutboxFilesIncludesHiddenDirectories(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "reports/.debug/log.txt", "debug")
	writeWorkspaceFile(t, root, "reports/visible.txt", "visible")

	files, err := collectOutboxFiles(root, []string{"reports"})
	if err != nil {
		t.Fatalf("collectOutboxFiles: %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.RelPath] = string(f.Data)
	}
	if got["reports/.debug/log.txt"] != "debug" {
		t.Fatalf("hidden outbox file missing: %+v", got)
	}
	if got["reports/visible.txt"] != "visible" {
		t.Fatalf("visible outbox file missing: %+v", got)
	}
}

func TestCollectOutboxFilesMissingPathIsSkipped(t *testing.T) {
	root := t.TempDir()

	files, err := collectOutboxFiles(root, []string{"does-not-exist.txt"})
	if err != nil {
		t.Fatalf("collectOutboxFiles: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("files = %+v, want none for a missing declared path", files)
	}
}

func TestCollectOutboxFilesRejectsLexicalEscape(t *testing.T) {
	root := t.TempDir()

	for _, rel := range []string{"../secret.txt", "/etc/passwd", "a/../../b"} {
		if _, err := collectOutboxFiles(root, []string{rel}); err == nil {
			t.Fatalf("collectOutboxFiles(%q) succeeded, want a path-escape error", rel)
		}
	}
}

func TestCollectOutboxFilesRejectsSymlinkEscapeForDeclaredFile(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("host secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(root, "escape-link")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	if _, err := collectOutboxFiles(root, []string{"escape-link"}); err == nil {
		t.Fatal("collectOutboxFiles(escape-link) succeeded, want a symlink-escape error")
	}
}

func TestCollectOutboxFilesSkipsSymlinkedSubdirectoryDuringWalk(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeWorkspaceFile(t, outside, "leaked.txt", "host secret")
	writeWorkspaceFile(t, root, "reports/kept.txt", "kept")

	link := filepath.Join(root, "reports", "escape-dir")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	files, err := collectOutboxFiles(root, []string{"reports"})
	if err != nil {
		t.Fatalf("collectOutboxFiles: %v", err)
	}
	for _, f := range files {
		if strings.Contains(f.RelPath, "leaked") {
			t.Fatalf("collected a file through a symlinked subdirectory: %+v", f)
		}
	}
	var sawKept bool
	for _, f := range files {
		if f.RelPath == "reports/kept.txt" {
			sawKept = true
		}
	}
	if !sawKept {
		t.Fatalf("expected reports/kept.txt among collected files, got %+v", files)
	}
}

func TestCollectOutboxFilesSkipsSymlinkedFileDuringWalk(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("host secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	writeWorkspaceFile(t, root, "reports/kept.txt", "kept")
	link := filepath.Join(root, "reports", "linked.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	files, err := collectOutboxFiles(root, []string{"reports"})
	if err != nil {
		t.Fatalf("collectOutboxFiles: %v", err)
	}
	for _, f := range files {
		if f.RelPath == "reports/linked.txt" {
			t.Fatalf("collected a symlinked file during directory walk: %+v", f)
		}
	}
}

func TestCollectOutboxFilesEnforcesAggregateByteLimit(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("x", journal.MaxOutboxBytesPerAttempt/2+1)
	writeWorkspaceFile(t, root, "a.bin", big)
	writeWorkspaceFile(t, root, "b.bin", big)

	_, err := collectOutboxFiles(root, []string{"a.bin", "b.bin"})
	if err == nil {
		t.Fatal("collectOutboxFiles over the aggregate byte limit succeeded, want an error")
	}
	for _, want := range []string{
		"67108866 bytes across 2 files",
		"67108864-byte aggregate limit",
		"a.bin (33554433 bytes)",
		"b.bin (33554433 bytes)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestCollectOutboxFilesEnforcesFileCountLimit(t *testing.T) {
	root := t.TempDir()
	declared := make([]string, 0, journal.MaxOutboxFilesPerAttempt+1)
	for i := 0; i < journal.MaxOutboxFilesPerAttempt+1; i++ {
		rel := filepath.ToSlash(filepath.Join("many", "f"+strconv.Itoa(i)+".txt"))
		writeWorkspaceFile(t, root, rel, "x")
		declared = append(declared, rel)
	}

	if _, err := collectOutboxFiles(root, declared); err == nil {
		t.Fatal("collectOutboxFiles over the file-count limit succeeded, want an error")
	}
}

// TestRunnerExportOutboxEndToEnd exercises the (*Runner).exportOutbox wrapper
// dispatchTask calls: workspace files in, journal-recorded artifacts out,
// through the real *journal.Run (satisfying executionJournal) rather than a
// mock, so the wiring between the collector and journal.ExportOutbox is
// covered, not just each half in isolation.
func TestRunnerExportOutboxEndToEnd(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "report.json", `{"ok":true}`)

	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: "run-outbox-e2e"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}

	t.Cleanup(func() { _ = jr.Close() })

	var r *Runner
	task := apiv1.Task{Name: "build", Outbox: []string{"report.json"}}
	if err := r.exportOutbox(jr, root, task, 1, journal.AttemptPolicy); err != nil {
		t.Fatalf("exportOutbox: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(runsDir, "run-outbox-e2e", "artifacts", "outbox", "build", "attempt-1", "occurrence-*", "report.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("exported outbox paths = %v, %v; want one immutable occurrence", matches, err)
	}
}

func TestRunnerExportOutboxMirrorsDurableCopy(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "reports/report.json", `{"token":"scrubbed"}`)
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: "run-outbox-mirror"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	t.Cleanup(func() { _ = jr.Close() })

	mirror := t.TempDir()
	task := apiv1.Task{
		Name:             "build",
		Outbox:           []string{"reports/report.json"},
		OutboxMirrorPath: mirror,
	}
	var r *Runner
	if err := r.exportOutbox(jr, root, task, 2, journal.AttemptPolicy); err != nil {
		t.Fatalf("exportOutbox: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(mirror, "run-outbox-mirror", "build", "attempt-2", "occurrence-*", "reports", "report.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("mirrored outbox paths = %v, %v; want one immutable occurrence", matches, err)
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read mirrored outbox: %v", err)
	}
	if string(got) != `{"token":"scrubbed"}` {
		t.Fatalf("mirrored content = %q", got)
	}
}

// TestRunnerGateRepassPreservesEveryOutboxOccurrence is #5218's end-to-end
// regression. A gate re-enters the same task, whose attempt counter restarts
// at 1, and both fixed-name exports must remain readable at their own digest.
func TestRunnerGateRepassPreservesEveryOutboxOccurrence(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "acme-web", Start: "validate",
		Tasks: []apiv1.Task{{
			Name: "validate", Type: apiv1.TaskDeterministic, Goal: "write evidence",
			Workspace: apiv1.WorkspaceRepo,
			Run:       &apiv1.DeterministicRun{Command: []string{"validate"}},
			Outbox:    []string{"evidence/failure.json"}, Next: "review",
		}},
		Gates: []apiv1.Gate{{
			Name: "review", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "scripted"},
			Branches: map[string]string{
				gate.OutcomePass: workflow.TerminalComplete,
				gate.OutcomeFail: "validate",
			},
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "outbox-repass", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	deterministic := &repassOutboxDeterministic{}
	evaluator := &repassThenPassGate{}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return deterministic, nil
	}, evaluator)
	const runID = "run-outbox-repass"
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatalf("Start = %+v, %v; want completed after one repass", result, err)
	}
	if deterministic.calls != 2 || evaluator.calls != 2 {
		t.Fatalf("calls = task:%d gate:%d, want 2/2", deterministic.calls, evaluator.calls)
	}

	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var refs []journal.Ref
	for _, event := range events {
		if event.Type == journal.EventArtifactRecorded && event.Stage == "validate" && event.Name == "outbox/evidence/failure.json" && event.Ref != nil {
			if event.Attempt != 1 {
				t.Fatalf("gate-repass export attempt = %d, want 1", event.Attempt)
			}
			refs = append(refs, *event.Ref)
		}
	}
	if len(refs) != 2 || refs[0].Path == refs[1].Path {
		t.Fatalf("outbox refs = %+v, want two distinct occurrence paths", refs)
	}
	for i, ref := range refs {
		data, err := os.ReadFile(filepath.Join(runsDir, runID, filepath.FromSlash(ref.Path)))
		if err != nil {
			t.Fatal(err)
		}
		want := `{"execution":` + strconv.Itoa(i+1) + `}`
		if string(data) != want || journal.Digest(data) != ref.Digest {
			t.Fatalf("historical ref %d = path:%q data:%q digest:%q, want data:%q digest:%q", i+1, ref.Path, data, journal.Digest(data), want, ref.Digest)
		}
	}
}

func TestMirrorOutboxRejectsRelativeRoot(t *testing.T) {
	if err := mirrorOutbox(t.TempDir(), "relative/path", nil); err == nil {
		t.Fatal("mirrorOutbox accepted a relative root")
	}
}

func TestMakeContainedDirRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if _, err := makeContainedDir(root, filepath.Join("escape", "nested")); err == nil {
		t.Fatal("makeContainedDir followed a symlink outside the mirror root")
	}
	if _, err := os.Stat(filepath.Join(outside, "nested")); !os.IsNotExist(err) {
		t.Fatalf("escape created an outside directory, stat err = %v", err)
	}
}

// TestRunnerExportOutboxPropagatesPathEscape confirms a declared path that
// escapes the workspace fails the exportOutbox call closed rather than
// silently skipping the offending entry — the class of gap the #1552 prior
// escalation flagged.
func TestRunnerExportOutboxPropagatesPathEscape(t *testing.T) {
	root := t.TempDir()
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: "run-outbox-escape"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	t.Cleanup(func() { _ = jr.Close() })

	var r *Runner
	task := apiv1.Task{Name: "build", Outbox: []string{"../../etc/passwd"}}
	if err := r.exportOutbox(jr, root, task, 1, journal.AttemptPolicy); err == nil {
		t.Fatal("exportOutbox with an escaping declared path succeeded, want an error")
	}
}

// TestRunnerExportOutboxNoOpWithoutDeclaration confirms a task that declares
// no Outbox paths never touches the journal.
func TestRunnerExportOutboxNoOpWithoutDeclaration(t *testing.T) {
	root := t.TempDir()
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: "run-outbox-noop"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	t.Cleanup(func() { _ = jr.Close() })

	var r *Runner
	task := apiv1.Task{Name: "build"}
	if err := r.exportOutbox(jr, root, task, 1, journal.AttemptPolicy); err != nil {
		t.Fatalf("exportOutbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runsDir, "run-outbox-noop", "artifacts", "outbox")); !os.IsNotExist(err) {
		t.Fatalf("expected no outbox directory, stat err = %v", err)
	}
}

func TestOversizedOutboxPreservesCommandOutcomeAndFailsWorkflow(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    "validate",
		Tasks: []apiv1.Task{
			{
				Name: "validate", Type: apiv1.TaskDeterministic, Goal: "compare",
				Run:    &apiv1.DeterministicRun{Command: []string{"compare"}},
				Outbox: []string{"evidence"}, ContinueOnError: true, Next: "after",
			},
			{Name: "after", Type: apiv1.TaskDeterministic, Goal: "must not run", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "outbox-failure", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}

	var executor *oversizedOutboxDeterministic
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		executor = &oversizedOutboxDeterministic{rec: rec}
		return executor, nil
	}, nil)
	const runID = "run-outbox-too-large"
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseFailed || result.FailureCode != outboxExportFailureCode {
		t.Fatalf("result = %+v, want terminal outbox export failure", result)
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1; continueOnError must not advance after missing evidence", executor.calls)
	}

	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var annotation, finished *journal.Event
	for i := range events {
		event := &events[i]
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == outboxExportFailureAnnotation {
			annotation = event
		}
		if event.Type == journal.EventStageFinished && event.Stage == "validate" {
			finished = event
		}
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "executor_error" {
			t.Fatalf("post-command export failure was misreported as executor_error: %+v", event)
		}
	}
	if annotation == nil {
		t.Fatal("missing command/outbox outcome annotation")
	}
	if annotation.Stage != "validate" || annotation.Attempt != 1 || annotation.Runner["commandStatus"] != "failure" ||
		annotation.Runner["commandErrorCode"] != "comparison_failed" || annotation.Runner["outboxEvidenceState"] != "missing" {
		t.Fatalf("annotation = %+v", annotation)
	}
	diagnosticJSON, err := json.Marshal(annotation.Runner)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"outboxAggregateBytes":73400320`,
		`"outboxAggregateByteLimit":67108864`,
		`"path":"evidence/largest.trx","size":41943040`,
		`"commandArtifactCount":1`,
		`"commandExitCode":1`,
	} {
		if !strings.Contains(string(diagnosticJSON), want) {
			t.Errorf("annotation JSON %s missing %s", diagnosticJSON, want)
		}
	}
	if finished == nil || finished.Status != string(apiv1.ResultFailure) || finished.Error == nil ||
		finished.Error.Code != outboxExportFailureCode || len(finished.Artifacts) != 1 || finished.Outputs["verdict"] != "failed" {
		t.Fatalf("stage.finished = %+v, want export failure plus preserved command evidence", finished)
	}
	if _, err := os.Stat(filepath.Join(runsDir, runID, "artifacts", "outbox", "validate")); !os.IsNotExist(err) {
		t.Fatalf("oversized outbox was partially published: %v", err)
	}
}
