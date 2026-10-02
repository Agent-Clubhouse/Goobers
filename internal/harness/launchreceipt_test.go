package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
)

func TestLocalLaunchReceiptPrecedesTaskAndReviewerAdapter(t *testing.T) {
	for _, review := range []bool{false, true} {
		t.Run(map[bool]string{false: "task", true: "reviewer"}[review], func(t *testing.T) {
			run := newSandboxTestRun(t)
			runID, _, _ := run.PinnedRun()
			kind := journal.EventStageStarted
			if review {
				kind = journal.EventReviewerStarted
			}
			event := journal.Event{Type: kind, Stage: "implement", Attempt: 1}
			seq, err := run.AppendWithSeq(event)
			if err != nil {
				t.Fatal(err)
			}
			ctx := launchreceipt.WithJournalStart(context.Background(), run, event, seq)
			binding, _ := launchreceipt.ContextBinding(ctx)
			root := t.TempDir()
			started := 0
			adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
				started++
				data, err := os.ReadFile(filepath.Join(root, binding.AttemptID+".json"))
				if err != nil {
					t.Fatal("adapter preceded durable receipt")
				}
				if strings.Contains(string(data), "fixture-private-requested-model") || strings.Contains(string(data), req.Workspace) {
					t.Fatal("receipt leaked model config or workspace")
				}
				var receipt launchreceipt.Receipt
				if err := json.Unmarshal(data, &receipt); err != nil {
					t.Fatal(err)
				}
				if receipt.Binding != binding || receipt.Local.Integrity != "unverified" || receipt.Local.ResolvedModel != "unknown" {
					t.Fatalf("invalid facts %+v", receipt)
				}
				if review {
					return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.Verdict{Decision: apiv1.VerdictPass, Rationale: "fixture"})
				}
				return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			}}
			executor, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), run, run, NewContextResolver(run, t.TempDir()), journal.NewPatternScrubber(), "fixture instructions", WithHarnessConfig("fixture-private-requested-model", nil), WithLaunchReceipts(launchreceipt.LocalRecorder{Root: root}))
			if err != nil {
				t.Fatal(err)
			}
			env := testEnvelope(t.TempDir(), "repo:read")
			env.RunID, env.TaskID, env.Attempt = runID, "implement", 1
			if review {
				_, err = executor.Review(ctx, env)
			} else {
				_, err = executor.Invoke(ctx, env)
			}
			if err != nil || started != 1 {
				t.Fatalf("launch: %v calls=%d", err, started)
			}
			if review {
				_, err = executor.Review(ctx, env)
			} else {
				_, err = executor.Invoke(ctx, env)
			}
			if !errors.Is(err, launchreceipt.ErrLocalPersistence) || started != 1 {
				t.Fatal("consumed attempt relaunched")
			}
		})
	}
}

func TestLocalLaunchReceiptFailureNeverEntersAdapter(t *testing.T) {
	run := newSandboxTestRun(t)
	runID, _, _ := run.PinnedRun()
	event := journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}
	seq, err := run.AppendWithSeq(event)
	if err != nil {
		t.Fatal(err)
	}
	ctx := launchreceipt.WithJournalStart(context.Background(), run, event, seq)
	blocked := filepath.Join(t.TempDir(), "private-path")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	adapter := &FakeAdapter{Act: func(context.Context, RunRequest) error { t.Fatal("failed persistence launched adapter"); return nil }}
	executor, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), run, run, NewContextResolver(run, t.TempDir()), journal.NewPatternScrubber(), "instructions", WithLaunchReceipts(launchreceipt.LocalRecorder{Root: blocked}))
	if err != nil {
		t.Fatal(err)
	}
	env := testEnvelope(t.TempDir(), "repo:read")
	env.RunID, env.TaskID, env.Attempt = runID, "implement", 1
	if _, err := executor.Invoke(ctx, env); !errors.Is(err, launchreceipt.ErrLocalPersistence) || strings.Contains(err.Error(), blocked) {
		t.Fatalf("unsafe persistence error: %v", err)
	}
	if _, err := executor.Review(ctx, env); !errors.Is(err, launchreceipt.ErrLocalPersistence) {
		t.Fatalf("task authority used by reviewer: %v", err)
	}
}
