package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

var terminalExitCases = []struct {
	name  string
	phase journal.RunPhase
	exit  int
}{
	{name: "completed", phase: journal.PhaseCompleted, exit: 0},
	{name: "failed", phase: journal.PhaseFailed, exit: 1},
	{name: "aborted", phase: journal.PhaseAborted, exit: 1},
	{name: "escalated", phase: journal.PhaseEscalated, exit: 3},
}

func initTerminalPhaseDemo(t *testing.T, phase journal.RunPhase, signal bool) string {
	t.Helper()
	root := initDemo(t)

	command := "true"
	next := ""
	switch phase {
	case journal.PhaseFailed:
		command = "false"
	case journal.PhaseAborted:
		next = "\n      next: \"@abort\""
	case journal.PhaseEscalated:
		next = "\n      next: \"@escalate\""
	}
	trigger := `    - type: backlog-item
      selector:
        goobers: "true"`
	if signal {
		trigger = `    - type: signal
      signal: "deploy"`
	}
	workflow := fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
%s
  start: local-ci
  tasks:
    - name: local-ci
      type: deterministic
      goal: exercise terminal exit behavior
      run:
        command: [%q]%s
`, trigger, command, next)

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "config", "gaggles", "example", "goobers")); err != nil {
		t.Fatal(err)
	}

	fixtureRepo := newDaemonFixtureRepo(t)
	prev := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil }
	t.Cleanup(func() { repoCloneURL = prev })
	return root
}

func TestRunExitCodesForTerminalPhases(t *testing.T) {
	for _, tt := range terminalExitCases {
		t.Run(tt.name, func(t *testing.T) {
			root := initTerminalPhaseDemo(t, tt.phase, false)
			code, stdout, stderr := runArgs(t, "run", "default-implement", root)
			if code != tt.exit {
				t.Fatalf("code = %d, want %d; stdout = %q, stderr = %q", code, tt.exit, stdout, stderr)
			}
			if !strings.Contains(stdout, "phase="+string(tt.phase)) {
				t.Fatalf("stdout = %q, want phase %s", stdout, tt.phase)
			}
		})
	}
}

func TestRunDelegatedExitCodesForTerminalPhases(t *testing.T) {
	oldPollInterval := delegationPollInterval
	delegationPollInterval = time.Millisecond
	t.Cleanup(func() { delegationPollInterval = oldPollInterval })

	for _, tt := range terminalExitCases {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			l := instance.NewLayout(root)
			runID := "delegated-" + tt.name
			writeStatusRunWithPhase(t, root, runID, "default-implement", "example", time.Now(), tt.phase)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			responseDone := make(chan error, 1)
			go func() {
				responseDone <- respondToDelegatedRequest(ctx, l.SchedulerDir(), runID)
			}()

			var stdout, stderr bytes.Buffer
			code := runDelegatedTrigger(ctx, l, "default-implement", root, false, &stdout, &stderr)
			if err := <-responseDone; err != nil {
				t.Fatal(err)
			}
			if code != tt.exit {
				t.Fatalf("code = %d, want %d; stdout = %q, stderr = %q", code, tt.exit, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "phase="+string(tt.phase)) {
				t.Fatalf("stdout = %q, want phase %s", stdout.String(), tt.phase)
			}
		})
	}
}

func TestRunDelegatedWaitsForJournalCreation(t *testing.T) {
	oldDelegationInterval := delegationPollInterval
	oldRunInterval := runPollInterval
	delegationPollInterval = time.Millisecond
	runPollInterval = time.Millisecond
	t.Cleanup(func() {
		delegationPollInterval = oldDelegationInterval
		runPollInterval = oldRunInterval
	})

	root := t.TempDir()
	l := instance.NewLayout(root)
	const runID = "delegated-delayed-journal"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	responseDone := make(chan error, 1)
	go func() {
		if err := respondToDelegatedRequest(ctx, l.SchedulerDir(), runID); err != nil {
			responseDone <- err
			return
		}
		time.Sleep(20 * time.Millisecond)
		run, err := journal.Create(l.ForGaggle("example").RunsDir(), journal.RunIdentity{
			RunID:     runID,
			Workflow:  "default-implement",
			Gaggle:    "example",
			StartedAt: time.Now(),
		}, nil)
		if err == nil {
			err = run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)})
		}
		if run != nil {
			err = errors.Join(err, run.Close())
		}
		responseDone <- err
	}()

	var stdout, stderr bytes.Buffer
	code := runDelegatedTrigger(ctx, l, "default-implement", root, false, &stdout, &stderr)
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "phase=completed") {
		t.Fatalf("stdout = %q, want completed phase", stdout.String())
	}
}

func respondToDelegatedRequest(ctx context.Context, schedulerDir, runID string) error {
	requestDir := filepath.Join(schedulerDir, pendingTriggersDir)
	for {
		entries, err := os.ReadDir(requestDir)
		if err == nil {
			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name(), requestSuffix) {
					continue
				}
				requestID := strings.TrimSuffix(entry.Name(), requestSuffix)
				data, err := json.Marshal(triggerResponse{RunID: runID})
				if err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(requestDir, requestID+responseSuffix), data, 0o644)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func TestSignalExitCodesForTerminalPhases(t *testing.T) {
	for _, tt := range terminalExitCases {
		t.Run(tt.name, func(t *testing.T) {
			root := initTerminalPhaseDemo(t, tt.phase, true)
			code, stdout, stderr := runArgs(t, "signal", "deploy", root)
			if code != tt.exit {
				t.Fatalf("code = %d, want %d; stdout = %q, stderr = %q", code, tt.exit, stdout, stderr)
			}
			if !strings.Contains(stdout, "phase="+string(tt.phase)) {
				t.Fatalf("stdout = %q, want phase %s", stdout, tt.phase)
			}
		})
	}
}

func TestRunAndSignalHelpDocumentsTerminalExitCodes(t *testing.T) {
	for _, command := range []string{"run", "signal"} {
		t.Run(command, func(t *testing.T) {
			_, _, stderr := runArgs(t, command, "-h")
			for _, want := range []string{"0 =", "1 =", "2 =", "3 =", "submission-only"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("%s help missing %q: %q", command, want, stderr)
				}
			}
		})
	}

	_, stdout, _ := runArgs(t, "help")
	if !strings.Contains(stdout, "3 = escalated") {
		t.Fatalf("top-level help does not document escalation exit code: %q", stdout)
	}
}
