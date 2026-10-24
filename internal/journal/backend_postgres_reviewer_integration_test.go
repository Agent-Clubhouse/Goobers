//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/temporaltest"
)

type postgresReviewerWorkspace struct {
	dir    string
	source []byte
	mu     sync.Mutex
	reads  int
}

func (w *postgresReviewerWorkspace) Path() string                 { return w.dir }
func (w *postgresReviewerWorkspace) Remove(context.Context) error { return nil }
func (w *postgresReviewerWorkspace) Head(context.Context) (string, string, error) {
	return "run/fixture", "main", nil
}
func (w *postgresReviewerWorkspace) Diff(context.Context, string) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reads++
	if w.reads > 1 {
		return []byte("+must-not-recapture\n"), nil
	}
	return w.source, nil
}
func (w *postgresReviewerWorkspace) Provision(context.Context, engine.WorkspaceRequest) (engine.Workspace, error) {
	return w, nil
}

type postgresPreparedReviewer struct {
	invoke.Goober
	backend        *journalpg.Backend
	source, stored []byte
	calls          int
}

func (r *postgresPreparedReviewer) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.calls++
	pointer, err := runsemantics.PreparedReviewerDiffPointer(env)
	if err != nil || pointer == nil || pointer.Artifact.Digest != journal.Digest(r.stored) {
		return apiv1.Verdict{}, fmt.Errorf("prepared pointer missing or invalid: %w", err)
	}
	reader, err := r.backend.OpenRun(ctx, journal.RunRef{ID: env.RunID})
	if err != nil {
		return apiv1.Verdict{}, err
	}
	events, err := reader.Events()
	if err != nil {
		return apiv1.Verdict{}, err
	}
	if err := postgresReviewEvidence(events, r.source, r.stored); err != nil {
		return apiv1.Verdict{}, err
	}
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

type postgresReviewLostAck struct {
	writer *livejournal.Writer
	mu     sync.Mutex
	lost   bool
}

func (e *postgresReviewLostAck) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	response, err := e.writer.Emit(ctx, req)
	if err != nil {
		return response, err
	}
	for _, op := range req.Ops {
		if op.Artifact != nil && strings.HasSuffix(op.Artifact.Name, "reviewer-diff.patch") && !e.lost {
			e.lost = true
			return livejournal.EmitResponse{}, errors.New("lost PG reviewer evidence acknowledgement")
		}
	}
	return response, nil
}

func postgresReviewEvidence(events []journal.Event, source, stored []byte) error {
	var artifact, annotation, gateStart, reviewStart uint64
	artifacts, annotations := 0, 0
	keys := map[string]bool{}
	for _, event := range events {
		relevant := false
		switch {
		case event.Type == journal.EventArtifactRecorded && strings.HasSuffix(event.Name, "reviewer-diff.patch"):
			artifacts++
			artifact = event.Seq
			relevant = true
			if event.Ref == nil || event.Ref.Digest != journal.Digest(stored) {
				return errors.New("wrong scrubbed artifact digest")
			}
		case event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == runsemantics.ReviewerDiffRedactionKind:
			annotations++
			annotation = event.Seq
			relevant = true
			if event.Runner["sourceDigest"] != journal.Digest(source) || event.Runner["sourceBytes"] != float64(len(source)) || event.Runner["digest"] != journal.Digest(stored) || event.Runner["bytes"] != float64(len(stored)) || event.Runner["redacted"] != true {
				return fmt.Errorf("redaction annotation: %v", event.Runner)
			}
		case event.Type == journal.EventGateStarted:
			gateStart = event.Seq
		case event.Type == journal.EventReviewerStarted:
			reviewStart = event.Seq
		}
		if relevant {
			key, ok := event.Runner[livejournal.EmitKeyRunnerField].(string)
			if !ok || key == "" || keys[key] {
				return errors.New("reviewer evidence key missing or duplicated")
			}
			keys[key] = true
		}
	}
	if artifacts != 1 || annotations != 1 || artifact >= annotation || annotation >= gateStart || gateStart >= reviewStart {
		return fmt.Errorf("review evidence counts=%d/%d sequence=%d/%d/%d/%d", artifacts, annotations, artifact, annotation, gateStart, reviewStart)
	}
	return nil
}

func TestPostgresReviewerPreparationCommitsScrubbedEvidenceBeforeReview(t *testing.T) {
	config := postgresJournalConfig(t)
	writer, backend, root := postgresLiveWriter(t, config) // asserts current_user = goobers_controller
	source := []byte("+reviewer-secret-in-committed-diff\n")
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte("reviewer-secret-in-committed-diff"))
	stored := scrubber.Scrub(source)
	spaces := &postgresReviewerWorkspace{dir: t.TempDir(), source: source}
	reviewer := &postgresPreparedReviewer{backend: backend, source: source, stored: stored}
	emitter := &postgresReviewLostAck{writer: writer}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&engine.Activities{Journal: emitter, Goober: reviewer, Workspaces: spaces, Canary: registry, Scrubber: scrubber})
	runID := strings.Repeat("d", 32)
	env.ExecuteWorkflow(engine.Run, engine.RunInput{RunID: runID, Gaggle: "web", WorkflowName: "prepared-review", Version: 1, DSLVersion: "3.0", LiveJournal: true, TriggerKind: string(journal.TriggerManual), RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "fixture", Name: "repo", Branch: "main"}, Spec: apiv1.WorkflowSpec{Gaggle: "web", Start: "review", Gates: []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"}, Branches: map[string]string{"pass": "", "fail": "@abort", "needs-changes": "@abort"}}}}})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if err := postgresReviewEvidence(events, source, stored); err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Type != journal.EventRunFinished {
		t.Fatal("run never finished")
	}
	spaces.mu.Lock()
	reads := spaces.reads
	spaces.mu.Unlock()
	emitter.mu.Lock()
	lost := emitter.lost
	emitter.mu.Unlock()
	if reads != 1 || reviewer.calls != 1 || !lost {
		t.Fatalf("diff reads=%d reviews=%d lost ack=%v", reads, reviewer.calls, lost)
	}
	ref, err := journal.ArtifactRef(stored)
	if err != nil {
		t.Fatal(err)
	}
	data, err := reader.ArtifactBytes(ref)
	if err != nil || string(data) != string(stored) {
		t.Fatalf("PG blob=%q error=%v", data, err)
	}
	assertNoLocalJournal(t, root)
}
