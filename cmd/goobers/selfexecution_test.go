package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

func TestSelfExecutionDeniedValidateIsHardError(t *testing.T) {
	root := initDeterministicDemo(t)
	path := filepath.Join(root, "instance.yaml")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("\nplacement:\n  selfExecution: deny\n"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runArgs(t, "validate", root)
	if code != 1 || !strings.Contains(out, "ERROR RNR001") || !strings.Contains(out, "placement.selfExecution: deny") || !strings.Contains(out, `stage "`) {
		t.Fatalf("code %d stdout %s stderr %s", code, out, stderr)
	}
}

func TestSelfExecutionDeniedGateTerminalParks(t *testing.T) {
	log, _ := hookTestLog(t)
	recorder := &hookRecorder{}
	err := fmt.Errorf("workflow gate: %w", temporal.NewNonRetryableApplicationError("review denied", runner.SelfExecutionDeniedCode, nil))
	recorder.hooks(log).run(context.Background(), engineTerminalOutcome{RunID: "denied-review", Phase: journal.PhaseFailed, Item: &apiv1.BacklogItem{ID: "42"}, Err: err})
	if len(recorder.blocked) != 1 || recorder.blocked[0].ItemID != "42" {
		t.Fatalf("park: %+v", recorder.blocked)
	}
	if engineTerminalFailureCode(err) != runner.SelfExecutionDeniedCode {
		t.Fatal("lost refusal classification")
	}
}

func TestSelfExecutionMigrationListsEveryLocalWorkStage(t *testing.T) {
	reason := selfExecutionMigrationReason(workflow.Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{Name: "implement"}, {Name: "local-ci"}}, Gates: []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAgentic}, {Name: "check", Evaluator: apiv1.EvaluatorAutomated}}}})
	for _, name := range []string{"implement", "local-ci", "review"} {
		if !strings.Contains(reason, name) {
			t.Fatalf("migration omitted %s: %s", name, reason)
		}
	}
	if strings.Contains(reason, "check") {
		t.Fatal("automated bookkeeping reported as workflow process work")
	}
}

func TestWorkerSelfExecutionAdmissionInitializesWithoutWatcher(t *testing.T) {
	for _, policy := range []string{"", "allow", "deny"} {
		t.Run("policy="+policy, func(t *testing.T) {
			root := initDemo(t)
			if policy != "" {
				path := filepath.Join(root, "instance.yaml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, []byte("\nplacement:\n  selfExecution: "+policy+"\n")...)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			seams, err := newWorkerSeams(root, nil)
			if err != nil {
				t.Fatal(err)
			}
			if seams.snapshot.Load() != nil {
				t.Fatal("fixture skipped first-use snapshot initialization")
			}
			var deps bootstrap.EngineDeps
			wireWorkerRuntimeSeams(&deps, seams, t.TempDir())
			// No watcher, daemon API, workspace, or executor has initialized the
			// snapshot: this is the first local activity's admission callback.
			err = deps.AdmitSelfExecution("startup:work")
			var refusal *runner.SelfExecutionRefusal
			if policy == "deny" {
				if !errors.As(err, &refusal) {
					t.Fatalf("deny did not refuse first activity: %v", err)
				}
			} else if err != nil {
				t.Fatalf("allow refused first activity without watcher: %v", err)
			}
			snapshot := seams.snapshot.Load()
			if snapshot == nil || snapshot.cfg == nil {
				t.Fatal("first admission did not publish a trusted snapshot")
			}
			stats := snapshot.cfg.SelfExecutionStats()
			if policy == "deny" {
				if stats.Placements != 0 || stats.Refusals != 1 {
					t.Fatalf("deny accounting: %+v", stats)
				}
			} else if stats.Placements != 1 || stats.Refusals != 0 {
				t.Fatalf("allow accounting: %+v", stats)
			}
		})
	}
}

func TestWorkerSelfExecutionFirstLoadErrorDoesNotBecomePolicyRefusal(t *testing.T) {
	root := initDemo(t)
	seams, err := newWorkerSeams(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), []byte("kind: [broken"), 0600); err != nil {
		t.Fatal(err)
	}
	var deps bootstrap.EngineDeps
	wireWorkerRuntimeSeams(&deps, seams, t.TempDir())
	err = deps.AdmitSelfExecution("startup:work")
	var refusal *runner.SelfExecutionRefusal
	if err == nil || errors.As(err, &refusal) || !strings.Contains(err.Error(), "load instance config") {
		t.Fatalf("first snapshot load error: %v", err)
	}
	if seams.snapshot.Load() != nil {
		t.Fatal("failed snapshot was published")
	}
}
