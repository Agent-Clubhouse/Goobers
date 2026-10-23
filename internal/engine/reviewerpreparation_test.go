package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/temporaltest"
)

type reviewerLostAckEmitter struct {
	writer *livejournal.Writer
	mu     sync.Mutex
	lost   bool
}

func (e *reviewerLostAckEmitter) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	response, err := e.writer.Emit(ctx, req)
	if err != nil {
		return response, err
	}
	for _, op := range req.Ops {
		if !e.lost && op.Artifact != nil && strings.HasSuffix(op.Artifact.Name, "reviewer-diff.patch") {
			e.lost = true
			return livejournal.EmitResponse{}, errors.New("lost reviewer evidence acknowledgement")
		}
	}
	return response, nil
}

func reviewerPreparationFixture(t *testing.T) (RunInput, *Activities, *fakeWorkspaces, *reviewerLostAckEmitter, string, []byte, []byte) {
	t.Helper()
	source := []byte("+credential-for-reviewer-preparation\n")
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte("credential-for-reviewer-preparation"))
	stored := scrubber.Scrub(source)
	writer, root := newLiveWriter(t)
	emitter := &reviewerLostAckEmitter{writer: writer}
	spaces := testWorkspaces(t)
	spaces.scriptDiffSequence("review", [][]byte{source, []byte("+must-not-recapture\n")})
	input := runInput("prepared-review", gatedSpec())
	input.RunID, input.LiveJournal = strings.Repeat("d", 32), true
	invoker := successInvoker()
	invoker.review = func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
		pointer, err := runsemantics.PreparedReviewerDiffPointer(env)
		if err != nil || pointer == nil || pointer.Artifact.Digest != journal.Digest(stored) {
			return apiv1.Verdict{}, errors.New("reviewer did not receive prepared evidence")
		}
		events := liveEvents(t, root, input.RunID)
		assertPreparedReviewOrder(t, events, source, stored, false)
		return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
	}
	return input, &Activities{Journal: emitter, Goober: invoker, Workspaces: spaces, Canary: registry, Scrubber: scrubber}, spaces, emitter, root, source, stored
}

func assertPreparedReviewOrder(t *testing.T, events []journal.Event, source, stored []byte, finished bool) {
	t.Helper()
	var artifact, annotation, gateStart, reviewStart uint64
	artifacts, annotations := 0, 0
	for _, event := range events {
		if event.Type == journal.EventArtifactRecorded && strings.HasSuffix(event.Name, "reviewer-diff.patch") {
			artifacts++
			artifact = event.Seq
			if event.Ref == nil || event.Ref.Digest != journal.Digest(stored) {
				t.Fatalf("wrong reviewer blob: %+v", event)
			}
			if event.Runner[livejournal.EmitKeyRunnerField] == "" {
				t.Fatal("artifact is not keyed")
			}
		}
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == runsemantics.ReviewerDiffRedactionKind {
			annotations++
			annotation = event.Seq
			if event.Runner["sourceDigest"] != journal.Digest(source) || event.Runner["digest"] != journal.Digest(stored) || event.Runner["redacted"] != true || event.Runner["sourceBytes"] != float64(len(source)) || event.Runner["bytes"] != float64(len(stored)) {
				t.Fatalf("redaction evidence=%v", event.Runner)
			}
			if event.Runner[livejournal.EmitKeyRunnerField] == "" {
				t.Fatal("annotation is not keyed")
			}
		}
		if event.Type == journal.EventGateStarted && event.Gate == "review" {
			gateStart = event.Seq
		}
		if event.Type == journal.EventReviewerStarted && event.Gate == "review" {
			reviewStart = event.Seq
		}
	}
	if artifacts != 1 || annotations != 1 || artifact >= annotation || annotation >= gateStart || gateStart >= reviewStart {
		t.Fatalf("counts=%d/%d order=%d/%d/%d/%d", artifacts, annotations, artifact, annotation, gateStart, reviewStart)
	}
	if finished && events[len(events)-1].Type != journal.EventRunFinished {
		t.Fatal("run did not finish")
	}
}

func TestReviewerPreparationCommitsBeforeReviewAndDeduplicatesLostAck(t *testing.T) {
	input, activities, spaces, emitter, root, source, stored := reviewerPreparationFixture(t)
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(activities)
	env.ExecuteWorkflow(Run, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	assertPreparedReviewOrder(t, liveEvents(t, root, input.RunID), source, stored, true)
	if spaces.diffCallCount("review") != 1 {
		t.Fatal("reviewer recaptured prepared diff")
	}
	emitter.mu.Lock()
	lost := emitter.lost
	emitter.mu.Unlock()
	if !lost {
		t.Fatal("lost acknowledgement not exercised")
	}
	reader, err := journal.OpenRead(filepath.Join(root, input.RunID))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := journal.ArtifactRef(stored)
	data, err := reader.ArtifactBytes(ref)
	if err != nil || string(data) != string(stored) {
		t.Fatalf("stored diff=%q err=%v", data, err)
	}
}

func TestReviewerPreparationHistoryAndEveryPrefixReplay(t *testing.T) {
	input, activities, _, _, root, source, stored := reviewerPreparationFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	w := worker.New(c, "reviewer-preparation-replay", worker.Options{})
	w.RegisterWorkflow(Run)
	w.RegisterActivity(activities)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: input.RunID, TaskQueue: "reviewer-preparation-replay"}, Run, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	assertPreparedReviewOrder(t, liveEvents(t, root, input.RunID), source, stored, true)
	history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
	prepared, marked := 0, false
	for _, event := range history.Events {
		if a := event.GetActivityTaskScheduledEventAttributes(); a != nil && a.ActivityType.Name == ActPrepareReviewerDiff {
			prepared++
		}
		if m := event.GetMarkerRecordedEventAttributes(); m != nil && strings.Contains(m.String(), reviewerPreparationChange) {
			marked = true
		}
	}
	if prepared != 1 || !marked {
		t.Fatalf("prepare commands=%d marked=%v", prepared, marked)
	}
	replayEveryWorkflowTask(t, history, replayReviewerPreparation)
}

func replayReviewerPreparation(t *testing.T, history *historypb.History) {
	t.Helper()
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflow(Run)
	if err := r.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatal(err)
	}
}

func TestReviewerPreparationCapturedNMinusOneHistoryReplays(t *testing.T) {
	// Captured before continuity positionals, so this also pins decoding the
	// original ReviewGoober payload and absence of prepared envelope fields.
	history := loadHistoryFixture(t, "testdata/continuity-prechange-history.json")
	reviewed := false
	for _, event := range history.Events {
		if a := event.GetActivityTaskScheduledEventAttributes(); a != nil {
			if a.ActivityType.Name == ActPrepareReviewerDiff {
				t.Fatal("fixture is not N-1")
			}
			reviewed = reviewed || a.ActivityType.Name == ActReviewGoober
		}
	}
	if !reviewed {
		t.Fatal("fixture never reviews")
	}
	replayEveryWorkflowTask(t, history, replayReviewerPreparation)
}

func TestReviewerPreparationDefaultVersionKeepsLegacyPath(t *testing.T) {
	spaces := testWorkspaces(t)
	spaces.scriptDiff("review", []byte("+legacy\n"))
	invoker := successInvoker()
	invoker.review = func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
		if env.PreparedReviewerDiff != nil {
			t.Fatal("N-1 reviewer received new envelope")
		}
		return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
	}
	env := laneEnv(t, invoker, spaces)
	env.OnGetVersion(reviewerPreparationChange, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	env.ExecuteWorkflow(Run, runInput("legacy-review", gatedSpec()))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	for _, op := range laneJournal(t, env).Ops {
		if op.Event != nil && op.Event.Runner["kind"] == runsemantics.ReviewerDiffRedactionKind {
			t.Fatal("N-1 emitted new annotation")
		}
	}
}
