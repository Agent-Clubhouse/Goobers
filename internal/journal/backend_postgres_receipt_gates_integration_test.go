//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
)

// Exercise the exact retry-artifact-inputs-and-gates corpus definition. The
// implementation-lane projection is consulted when review begins, after the
// trusted result.json CAS receipt has been committed; a single-stage test
// never reached that projection and missed the outbox-only assumption.
func TestPostgresRecorderReceiptRetryArtifactInputsAndGates(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "test", "journalequivalence", "testdata", "retry-gates.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc apiv1.Workflow
	if err := yaml.UnmarshalStrict(data, &doc); err != nil {
		t.Fatal(err)
	}
	config := postgresJournalConfig(t)
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer, backend, root := postgresLiveWriter(t, config, livejournal.WithArtifactSource(store))
	script := &receiptGateExecutor{pgReceiptExecutor: pgReceiptExecutor{root: t.TempDir(), store: store}}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&engine.Activities{Journal: writer, Det: script, Goober: script, Auto: script, Workspaces: receiptGateWorkspace(t.TempDir())})
	runID := strings.Repeat("8", 32)
	preview := true
	env.ExecuteWorkflow(engine.Run, engine.RunInput{RunID: runID, Gaggle: doc.Spec.Gaggle, WorkflowName: doc.Name, Version: 1, DSLVersion: doc.DSLVersion, Spec: doc.Spec, LiveJournal: true, PreviewFeaturesEnabled: &preview, RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "fixture", Name: "repo", Branch: "main"}, TriggerKind: string(journal.TriggerManual)})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if script.calls != 2 || script.automated != 1 || script.reviewed != 1 {
		t.Fatalf("calls=%d automated=%d reviewed=%d", script.calls, script.automated, script.reviewed)
	}
	reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	var artifactSeq, reviewSeq uint64
	for _, event := range events {
		if event.Type == journal.EventArtifactRecorded && event.Name == "result.json" {
			count++
			artifactSeq = event.Seq
			if event.Ref == nil {
				t.Fatal("missing receipt ref")
			}
			if _, err := reader.ArtifactBytes(*event.Ref); err != nil {
				t.Fatal(err)
			}
		}
		if event.Type == journal.EventReviewerStarted {
			reviewSeq = event.Seq
		}
	}
	if count != 1 || artifactSeq == 0 || reviewSeq <= artifactSeq || events[len(events)-1].Status != string(journal.PhaseCompleted) {
		t.Fatalf("receipt count=%d seq=%d review=%d last=%+v", count, artifactSeq, reviewSeq, events[len(events)-1])
	}
	assertNoLocalJournal(t, root)
}

type receiptGateExecutor struct {
	pgReceiptExecutor
	calls, automated, reviewed int
}

func (s *receiptGateExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	s.calls++
	if env.Inputs["fixture"] != "pinned-input" {
		return apiv1.ResultEnvelope{}, errors.New("static input missing")
	}
	if s.calls == 1 {
		return apiv1.ResultEnvelope{}, errors.New("fixture policy retry")
	}
	return s.pgReceiptExecutor.Run(ctx, env, run)
}
func (s *receiptGateExecutor) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	s.automated++
	return "pass", nil
}
func (s *receiptGateExecutor) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	s.reviewed++
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

type receiptGateWorkspace string

func (w receiptGateWorkspace) Provision(context.Context, engine.WorkspaceRequest) (engine.Workspace, error) {
	return w, nil
}
func (w receiptGateWorkspace) Path() string               { return string(w) }
func (receiptGateWorkspace) Remove(context.Context) error { return nil }
func (receiptGateWorkspace) Head(context.Context) (string, string, error) {
	return "fixture-branch", "main", nil
}
func (receiptGateWorkspace) Diff(context.Context, string) ([]byte, error) {
	return []byte("diff --git a/result b/result\n+stable\n"), nil
}
