package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
