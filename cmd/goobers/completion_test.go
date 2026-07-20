package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestCompletionScriptsGolden(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, "completion", shell)
			if code != 0 {
				t.Fatalf("completion %s: code = %d, stderr = %q", shell, code, stderr)
			}
			path := filepath.Join("testdata", "completion."+shell+".golden")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(stdout), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if stdout != string(want) {
				t.Fatalf("completion %s differs from %s; regenerate with UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestCompletionScriptsGolden", shell, path)
			}
			if stderr != "" {
				t.Fatalf("completion %s stderr = %q, want empty", shell, stderr)
			}
		})
	}
}

func TestCompletionRejectsUnknownShell(t *testing.T) {
	code, stdout, stderr := runArgs(t, "completion", "powershell")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if stderr == "" {
		t.Fatal("stderr is empty, want usage")
	}
}

func TestCompletionCandidatesFromNestedInstance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "workcopies", "repo", "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	if got, want := completionCandidates("workflows", nested), []string{"default-implement"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("workflow candidates = %v, want %v", got, want)
	}

	started := time.Date(2026, time.July, 16, 10, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(root)
	createListRun(t, layout.RunsDir(), "older-run", started)
	createListRun(t, layout.RunsDir(), "newer-run", started.Add(time.Minute))
	writeStatusRunWithPhase(t, root, "escalated-run", "implementation", "goobers", started.Add(2*time.Minute), journal.PhaseEscalated)
	createCompletionRunWithPhase(t, layout.ForGaggle("goobers").RunsDir(), "scoped-escalated-run", started.Add(3*time.Minute), journal.PhaseEscalated)
	if got, want := completionCandidates("runs", nested), []string{"scoped-escalated-run", "escalated-run", "newer-run", "older-run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("run candidates = %v, want %v", got, want)
	}
	if got, want := completionCandidates("escalations", nested), []string{"scoped-escalated-run", "escalated-run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("escalation candidates = %v, want %v", got, want)
	}
}

func createCompletionRunWithPhase(
	t *testing.T,
	runsDir, runID string,
	startedAt time.Time,
	phase journal.RunPhase,
) {
	t.Helper()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:     runID,
		Workflow:  "implementation",
		Gaggle:    "goobers",
		StartedAt: startedAt,
	}, nil)
	if err != nil {
		t.Fatalf("create completion fixture run: %v", err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		t.Fatalf("finish completion fixture run: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close completion fixture run: %v", err)
	}
}

func TestCompletionCandidatesAreBoundedNewestFirst(t *testing.T) {
	started := time.Date(2026, time.July, 16, 10, 0, 0, 0, time.UTC)
	runs := make([]runSummary, recentCompletionRunLimit+1)
	for i := range runs {
		runs[i] = runSummary{
			RunID:     fmt.Sprintf("run-%03d", i),
			StartedAt: started.Add(time.Duration(i) * time.Minute),
		}
	}

	got := recentCompletionRunIDs(runs)
	if len(got) != recentCompletionRunLimit {
		t.Fatalf("candidate count = %d, want %d", len(got), recentCompletionRunLimit)
	}
	if got[0] != "run-100" || got[len(got)-1] != "run-001" {
		t.Fatalf("bounded candidates start/end = %q/%q, want run-100/run-001", got[0], got[len(got)-1])
	}
}

func TestCompletionCandidatesDegradeSilentlyOutsideInstance(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Chdir(outside); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	code, stdout, stderr := runArgs(t, "__complete", "workflows")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("outside instance: code=%d stdout=%q stderr=%q, want silent success", code, stdout, stderr)
	}
}
