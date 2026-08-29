package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// hangingGitOnPath puts a `git` that never exits ahead of the real one on
// PATH, standing in for #3644's failure mode: a diff that hydrates blobs from
// the remote and wedges on an unreachable remote or a stalled credential
// helper. `exec sleep` (not a child process) so the context's kill actually
// reaps the process and closes the captured stdout pipe.
func hangingGitOnPath(t *testing.T) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep binary to build a hanging git stub: %v", err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexec " + sleep + " 600\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write hanging git stub: %v", err)
	}
	t.Setenv("PATH", dir)
}

// TestRecordUnpushedDiffBoundsCaptureAfterCancellation is #3644: the capture
// keeps outliving its attempt's cancellation, but it may no longer run
// unbounded — a git read that never returns must be abandoned so
// terminalization and workspace teardown can proceed, with a diagnostic that
// names the branch still holding the work.
func TestRecordUnpushedDiffBoundsCaptureAfterCancellation(t *testing.T) {
	instanceRoot := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	wt, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: newFixtureRepo(t),
		RunID:   "run-3644",
		BaseRef: "main",
		Branch:  "goobers/impl/run-3644",
	})
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "impl.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("write work: %v", err)
	}
	runGit(t, wt.Path, "add", "-A")
	runGit(t, wt.Path, "commit", "-m", "implement the backlog item")

	runsDir := filepath.Join(instanceRoot, "runs")
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: "run-3644"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	t.Cleanup(func() { _ = jr.Close() })

	previousTimeout := unpushedDiffCaptureTimeout
	unpushedDiffCaptureTimeout = 200 * time.Millisecond
	t.Cleanup(func() { unpushedDiffCaptureTimeout = previousTimeout })
	hangingGitOnPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the stalled-run watchdog already cancelled this attempt

	var r *Runner
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.recordUnpushedDiff(
			ctx, jr, newExecutors(Config{}, nil, nil),
			StartInput{RunID: "run-3644", RepoRef: apiv1.RepoRef{Branch: "main"}},
			apiv1.Task{Name: "implement"},
			&stageWorkspace{path: wt.Path, worktree: wt}, 1, journal.AttemptPolicy,
		)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("recordUnpushedDiff did not return: the post-cancellation capture is unbounded")
	}

	events := readRunEvents(t, runsDir, "run-3644")
	var message string
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "unpushed_diff_record_failed" {
			message = event.Error.Message
		}
	}
	if message == "" {
		t.Fatalf("no unpushed_diff_record_failed event journaled; events = %+v", events)
	}
	for _, want := range []string{"exceeded its", "goobers/impl/run-3644", "recovery"} {
		if !strings.Contains(message, want) {
			t.Fatalf("diagnostic %q does not mention %q", message, want)
		}
	}
}

// TestUnpushedDiffCaptureFailureLeavesOtherFailuresAlone keeps the bound's
// diagnostic scoped to bound overruns: an ordinary capture failure is
// journaled verbatim, unannotated.
func TestUnpushedDiffCaptureFailureLeavesOtherFailuresAlone(t *testing.T) {
	cause := os.ErrPermission
	got := unpushedDiffCaptureFailure(context.Background(), cause, "goobers/impl/run-3644")
	if got != cause {
		t.Fatalf("unpushedDiffCaptureFailure(non-deadline) = %v, want the cause unchanged", got)
	}
}
