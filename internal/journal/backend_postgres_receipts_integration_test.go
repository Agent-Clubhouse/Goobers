//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/workerhost"
)

type pgReceiptExecutor struct {
	invoke.Goober
	root  string
	store blobstore.Store
	fail  bool
}

func (e pgReceiptExecutor) Run(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	recorder := workerhost.NewStagingArtifacts(e.root, journal.NewPatternScrubber(), e.store)
	recorder.ReportArtifact = invoke.ArtifactReporter(ctx)
	ref, err := recorder.RecordArtifact("result.json", []byte("secret ghp_"+strings.Repeat("A", 80)))
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if e.fail {
		return apiv1.ResultEnvelope{}, errors.New("failed after recording")
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Artifacts: []apiv1.ArtifactPointer{{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, Integrity: ref.Integrity}}}, nil
}

type receiptRetryEmitter struct {
	writer   *livejournal.Writer
	loseOnce bool
	lost     bool
}

func (e *receiptRetryEmitter) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	response, err := e.writer.Emit(ctx, req)
	if err != nil {
		return response, err
	}
	for _, op := range req.Ops {
		if e.loseOnce && !e.lost && op.Artifact != nil && op.Artifact.Name == "result.json" {
			e.lost = true
			return livejournal.EmitResponse{}, errors.New("lost artifact commit response")
		}
	}
	return response, nil
}

func TestPostgresEngineRecorderReceiptsPrecedeOutcomeAndDeduplicate(t *testing.T) {
	for _, mode := range []string{"success", "failure", "lost-response", "agentic-success", "agentic-failure"} {
		t.Run(mode, func(t *testing.T) {
			config := postgresJournalConfig(t)
			store, err := blobstore.NewDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writer, backend, root := postgresLiveWriter(t, config, livejournal.WithArtifactSource(store), livejournal.WithSpanSource(store))
			emitter := &receiptRetryEmitter{writer: writer, loseOnce: mode == "lost-response"}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			script := pgReceiptExecutor{root: t.TempDir(), store: store, fail: strings.HasSuffix(mode, "failure")}
			env.RegisterActivity(&engine.Activities{Journal: emitter, Det: script, Goober: script, Workspaces: &workerhost.WorktreeWorkspaces{ScratchDir: t.TempDir()}})
			task := apiv1.Task{Name: "work", Type: apiv1.TaskDeterministic, Goal: "record", Run: &apiv1.DeterministicRun{Command: []string{"fixture"}, Workspace: apiv1.WorkspaceScratch}}
			if strings.HasPrefix(mode, "agentic") {
				task.Type, task.Goober, task.Workspace, task.Run = apiv1.TaskAgentic, "fixture", apiv1.WorkspaceScratch, nil
			}
			runID := strings.Repeat("9", 32)
			preview := true
			env.ExecuteWorkflow(engine.Run, engine.RunInput{RunID: runID, Gaggle: "web", WorkflowName: "receipts", Version: 1, DSLVersion: "3.0", LiveJournal: true, PreviewFeaturesEnabled: &preview,
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "fixture", Name: "repo", Branch: "main"}, TriggerKind: string(journal.TriggerManual),
				Spec: apiv1.WorkflowSpec{Gaggle: "web", Start: "work", Tasks: []apiv1.Task{task}}})
			if !script.fail && env.GetWorkflowError() != nil {
				t.Fatal(env.GetWorkflowError())
			}
			if emitter.loseOnce && !emitter.lost {
				t.Fatal("lost-response path was not exercised")
			}
			reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: runID})
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			artifactSeq, outcomeSeq := uint64(0), uint64(0)
			count := 0
			for _, event := range events {
				if event.Type == journal.EventArtifactRecorded && event.Name == "result.json" {
					count++
					artifactSeq = event.Seq
					if event.Stage != "" || event.Ref == nil || event.Integrity != apiv1.IntegrityDerived {
						t.Fatalf("recorder identity changed: %+v", event)
					}
					data, err := reader.ArtifactBytes(*event.Ref)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(data), strings.Repeat("A", 80)) || journal.Digest(data) != event.Ref.Digest {
						t.Fatal("receipt did not commit scrubbed bytes")
					}
				}
				if event.Stage == "work" && (event.Type == journal.EventStageFinished || event.Type == journal.EventError) && outcomeSeq == 0 {
					outcomeSeq = event.Seq
				}
			}
			if count != 1 || artifactSeq == 0 || outcomeSeq <= artifactSeq {
				t.Fatalf("artifact count=%d seq=%d outcome=%d events=%+v", count, artifactSeq, outcomeSeq, events)
			}
			assertNoLocalJournal(t, root)
		})
	}
}

func (e pgReceiptExecutor) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	invoke.ReportAgentUsage(ctx, map[string]float64{"gen_ai.usage.input_tokens": 7})
	return e.Run(ctx, env, apiv1.DeterministicRun{})
}
