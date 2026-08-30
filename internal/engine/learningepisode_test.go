package engine

// Replay, determinism and naming coverage for plan item E10 (#3913): the
// learning-episode injection on the gate retry arm.
//
// The parity harness proves the engine and the local runner AGREE. It cannot
// prove the engine's half is replay-safe, because it walks each side exactly
// once, in-process, and never replays. That distinction matters more here than
// for most ports: the injection is a walk decision whose OUTPUT is a
// content-addressed digest threaded into the next stage's dispatch. Three
// failure modes follow, none of them visible to parity:
//
//   - IMPURITY. If the episode's bytes ever picked up a real clock, a random
//     source, a map iteration or an activity result, the digest would differ on
//     replay — and a differing digest means a differing artifact name, pointer
//     name and envelope, i.e. a nondeterminism panic that wedges the run.
//   - SEQ SKEW. The artifact and pointer names embed the journal SEQUENCE of
//     the event being corrected, which the walk derives from its own
//     accumulated ops (projectedEvents) before any journal exists. If that
//     derivation drifted from the sequence the writer actually assigns, the
//     walk would name the episode after an event that is not the one it
//     corrects — silently, with no test failing anywhere near the change.
//   - HISTORY SKEW. A worker running this code replays a history recorded
//     before it. learning.episode.injected is a runner.annotation, the same
//     event class that had to be added to projectableEventTypes for E2.
//
// TestEngineLearningEpisodeNamesTheCorrectedEvent pins the second against a
// real projected journal. TestEngineLearningEpisodeIsDeterministic covers the
// first cheaply, in the test environment. TestEngineLearningEpisodeHistoryReplays
// covers all three against a real dev server — the only place a genuine replay
// happens — and asserts the digest the replay re-derives is the digest the
// original walk produced, which is the claim the injected POINTER rests on.
//
// e4replay_test.go replays the implementation-lane fixtures #3882 landed. This
// file is deliberately separate and deliberately DETERMINISTIC: the injection
// is on the generic retry arm, so it must be provably replay-safe on a fixture
// with no reviewer, no agentic stage and no cached verdict anywhere in it.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	temporalworker "go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// learningEpisodeScript fails the implementer once with a retry-classifiable
// code, so the review gate takes its fail branch back into implement and the
// retry arm fires exactly once.
func learningEpisodeScript() map[string][]scriptedCall {
	return map[string][]scriptedCall{
		"implement": {
			fail("nonzero_exit", "3 tests failed"),
			succeed(map[string]interface{}{"tests": "green"}),
		},
	}
}

// The episode must be named after the event it actually corrects, and the
// projected journal must be densely numbered so that name means what it says.
//
// "learning/episode-<gate>-<seq>" and "learning.episode[<seq>]" are keyed on
// the journal SEQUENCE of the failure being corrected, and the walk derives
// that sequence from its own accumulated ops before any journal exists. The
// derivation is only sound while ops map one-to-one onto appended events —
// run.started is op 0 and seq 1, an append is one Append, an artifact record
// appends exactly one artifact.recorded.
//
// Nothing in the type system says so. A future op kind appending two events (or
// none) would shift every subsequent sequence, and the only symptom would be an
// episode named after the wrong event: no panic, no failure, just a pointer
// that no longer means what it says, and a reconciliation that correlates the
// wrong failure. Checking the numbering AND the resulting name against the
// projection turns that into a test failure on the first walk after the change.
func TestEngineLearningEpisodeNamesTheCorrectedEvent(t *testing.T) {
	events, _, err := shortcutRunWithID(t, "e10-seq", parityLearningEpisodeSpec(), learningEpisodeScript())
	if err != nil {
		t.Fatalf("engine walk: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("engine walk produced no journal events")
	}
	for i, ev := range events {
		if want := uint64(i + 1); ev.Seq != want {
			t.Fatalf("projected event %d (%s) has seq %d, want %d — the projected journal is no longer "+
				"densely numbered from 1, so a walk cannot derive an event's sequence from its op position",
				i, ev.Type, ev.Seq, want)
		}
	}

	var failed journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventStageFinished && ev.Stage == "implement" && ev.Status != string(apiv1.ResultSuccess) {
			failed = ev
			break
		}
	}
	if failed.Seq == 0 {
		t.Fatalf("no failed implement stage.finished in the projected journal:\n%s", formatEventSeqs(events))
	}
	wantName := runner.LearningEpisodeArtifactName("review", failed.Seq)
	if _, ok := findArtifactRecorded(events, wantName); !ok {
		t.Fatalf("no artifact.recorded named %q; the walk named the episode after some other event's "+
			"sequence.\njournal:\n%s", wantName, formatEventSeqs(events))
	}
	// And the pointer the repass was dispatched with names the same sequence.
	wantPointer := runner.LearningEpisodePointerName(failed.Seq)
	found := false
	for _, ev := range events {
		if ev.Type == journal.EventRunnerAnnotation && ev.Name == wantName {
			if runner.LearningEpisodePointerName(uint64(annotationInt(ev.Runner, "sourceSeq"))) == wantPointer {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no learning annotation whose sourceSeq yields pointer %q; the artifact and the pointer "+
			"must name the same corrected event", wantPointer)
	}
}

// Two identical walks must produce the identical episode: same digest, same
// artifact name, same pointer name, same annotation payload.
//
// The digest is the strong assertion. It is content-addressed over the whole
// episode — schema, run identity, workflow digest, normalized findings,
// evidence, correction feedback, recommended action — so requiring it to be
// stable across two walks is requiring every one of those fields to be a pure
// function of deterministic workflow state. An impurity anywhere inside
// BuildLearningEpisode surfaces here as a digest diff rather than as a
// nondeterminism panic in production.
func TestEngineLearningEpisodeIsDeterministic(t *testing.T) {
	for _, fixture := range learningEpisodeFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			assertLearningEpisodeIsDeterministic(t, fixture)
		})
	}
}

// learningEpisodeFixtures is the shared table the determinism and replay tests
// both walk. The send-back entry is #3931's: it is the only one whose episodes
// are addressed to an attempt the SUBJECT's counter cannot name, so it is the
// only one where a target-derived nextAttempt can be shown to replay stably.
func learningEpisodeFixtures() []learningEpisodeFixture {
	return []learningEpisodeFixture{
		{name: "no contextFrom", id: "e10-det", spec: parityLearningEpisodeSpec(),
			script: learningEpisodeScript(), episodes: 1},
		{name: "implementation-lane contextFrom", id: "e10-det-contextfrom",
			spec: parityLearningEpisodeContextFromSpec(), script: learningEpisodeScript(), episodes: 1},
		{name: "nontrivial send-back", id: "e10-det-send-back",
			spec: parityLearningEpisodeSendBackSpec(), script: learningEpisodeSendBackScript(), episodes: 2},
	}
}

type learningEpisodeFixture struct {
	name     string
	id       string
	spec     apiv1.WorkflowSpec
	script   map[string][]scriptedCall
	episodes int
}

// learningEpisodeSendBackScript drives parityLearningEpisodeSendBackSpec
// through BOTH of its send-backs, which is what makes implement's entry count
// overtake local-ci's attempt.
func learningEpisodeSendBackScript() map[string][]scriptedCall {
	return map[string][]scriptedCall{
		"implement": {
			fail("nonzero_exit", "3 tests failed"),
			succeed(map[string]interface{}{"tests": "green"}),
			succeed(map[string]interface{}{"tests": "green"}),
		},
		"local-ci": {
			fail("nonzero_exit", "local ci is red"),
			succeed(map[string]interface{}{"ci": "green"}),
		},
	}
}

func assertLearningEpisodeIsDeterministic(t *testing.T, fixture learningEpisodeFixture) {
	t.Helper()
	id, spec, script := fixture.id, fixture.spec, fixture.script
	first, _, firstErr := shortcutRunWithID(t, id, spec, script)
	second, _, secondErr := shortcutRunWithID(t, id, spec, script)
	if (firstErr == nil) != (secondErr == nil) {
		t.Fatalf("walk error differs between runs: %v vs %v", firstErr, secondErr)
	}
	if err := diffConformanceViews(first, second); err != nil {
		t.Fatalf("journal projections differ between two identical walks: %v", err)
	}
	firstEpisodes := learningEpisodes(paritySide{Name: "first", Events: first})
	secondEpisodes := learningEpisodes(paritySide{Name: "second", Events: second})
	if len(firstEpisodes) != fixture.episodes {
		t.Fatalf("first walk injected %d learning episode(s), want exactly %d:\n%s",
			len(firstEpisodes), fixture.episodes, formatEventSeqs(first))
	}
	if err := diffLearningEpisodes("second walk", secondEpisodes, firstEpisodes); err != nil {
		t.Fatalf("learning episodes differ between two identical walks: %v", err)
	}
}

// A recorded history of a walk that injected a learning episode must replay
// cleanly through Temporal's own determinism checker, AND re-derive the same
// digest.
//
// The test environment does not replay — it executes the workflow once and
// hands back the result — so only NewWorkflowReplayer over a real recorded
// history exercises the check that matters. Replaying proves the command
// sequence is stable; comparing the digest the replayed run's journal query
// reports against the digest the original walk recorded proves the in-walk
// seam's OUTPUT is stable too, which is the property the next stage's context
// pointer depends on and the one a determinism panic would never tell us about
// (a walk can be perfectly deterministic in its commands while deriving a
// different artifact name).
//
// It runs over BOTH E10 fixtures: the original one, whose re-entered stage
// declares no contextFrom, and #3928's, whose stage declares the flagship
// implementation lane's contextFrom and minimum. The second is not redundant:
// selection happens inside the walk, on the pointer set the injection just
// added to, so a selector that dropped the episode would change what the walk
// dispatches — and the replayed history has to agree with the original about
// that too, or the digest a repass is handed stops being reproducible.
func TestEngineLearningEpisodeHistoryReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error",
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})
	temporalClient := server.Client()

	// One worker (and one scriptedExec) PER fixture: the script's last call
	// repeats, so a shared executor would hand the second run's first
	// implement dispatch the previous run's trailing success — no failure, no
	// retry arm, no injection, and a fixture that silently stopped testing
	// anything.
	for _, fixture := range learningEpisodeFixtures() {
		fixture.id = strings.Replace(fixture.id, "e10-det", "e10-replay", 1)
		t.Run(fixture.name, func(t *testing.T) {
			exec := newScriptedExec(fixture.script)
			w := temporalworker.New(temporalClient, fixture.id, temporalworker.Options{})
			RegisterWith(w, &Activities{
				Goober:     exec,
				Det:        exec,
				Auto:       gate.NewAutomatedEvaluator(),
				Workspaces: testWorkspaces(t),
			})
			if err := w.Start(); err != nil {
				t.Fatalf("start Temporal worker: %v", err)
			}
			t.Cleanup(w.Stop)
			replayLearningEpisodeHistory(ctx, t, temporalClient, fixture.id, fixture.id, fixture)
		})
	}
}

// replayLearningEpisodeHistory runs one fixture to completion on a live dev
// server, replays its recorded history through the determinism checker, and
// asserts the episode identity the replay re-derives is the one the original
// walk produced.
func replayLearningEpisodeHistory(
	ctx context.Context, t *testing.T, temporalClient client.Client,
	taskQueue, id string, fixture learningEpisodeFixture,
) {
	t.Helper()
	in := runInput(id, fixture.spec)
	in.RunID = id
	in.TriggerKind = string(journal.TriggerManual)
	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        id,
		TaskQueue: taskQueue,
	}, Run, in)
	if err != nil {
		t.Fatalf("execute workflow: %v", err)
	}
	var result RunResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}

	// The projection the completed run reports. The journal query re-derives
	// it from history, so this is already one round trip through replay.
	recorded := mustQueryProjection(ctx, t, temporalClient, run.GetID())
	episodes := projectionEpisodes(t, recorded)
	if len(episodes) != fixture.episodes {
		t.Fatalf("engine run injected %d learning episode(s), want exactly %d — the fail branch(es) must "+
			"re-enter implement and fire the retry arm", len(episodes), fixture.episodes)
	}

	iter := temporalClient.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false,
		enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	history := &historypb.History{}
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatalf("read workflow history: %v", err)
		}
		history.Events = append(history.Events, event)
	}
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("replay a learning-episode history: %v\n"+
			"the in-walk artifact seam must be a pure function of deterministic workflow state", err)
	}

	// Re-query after the replay: the digest, artifact name and pointer name the
	// walk derived in-walk must be exactly the ones it derived the first time.
	after := mustQueryProjection(ctx, t, temporalClient, run.GetID())
	replayed := projectionEpisodes(t, after)
	if len(replayed) != len(episodes) {
		t.Fatalf("replayed projection has %d learning episode(s), want %d", len(replayed), len(episodes))
	}
	for i := range episodes {
		if replayed[i] != episodes[i] {
			t.Fatalf("learning episode %d is not stable across replay:\n  recorded: %+v\n  replayed: %+v",
				i, episodes[i], replayed[i])
		}
	}
	for i, episode := range episodes {
		t.Logf("replayed learning episode %d: artifact=%s digest=%s pointer=%s id=%s nextAttempt=%d",
			i+1, episode.Name, episode.Digest, episode.Pointer, episode.EpisodeID, episode.NextAttempt)
	}
}

// projectionDigest is the in-walk-derived identity of one injected episode, in
// the form the replay assertion compares.
type projectionDigest struct {
	Name      string
	Digest    string
	Pointer   string
	EpisodeID string
	// NextAttempt is #3931's number. It is inside the digest already, so it is
	// not adding coverage so much as making a replay failure legible: a worker
	// that re-derived the attempt from the wrong stage reports a digest diff
	// and, beside it, the two attempt numbers that explain it.
	NextAttempt int
}

// projectionEpisodes reads the injected episodes straight off a JournalProjection
// — the workflow's own accumulated ops, which is where the in-walk seam's output
// lives before any writer has touched it.
func projectionEpisodes(t *testing.T, proj JournalProjection) []projectionDigest {
	t.Helper()
	var out []projectionDigest
	for i, op := range proj.Ops {
		if op.Kind != opArtifact || op.Artifact == nil ||
			!strings.HasPrefix(op.Artifact.Name, learningEpisodeArtifactPrefix) {
			continue
		}
		ref, err := journal.ArtifactRef(op.Artifact.Data)
		if err != nil {
			t.Fatalf("address episode artifact %q: %v", op.Artifact.Name, err)
		}
		entry := projectionDigest{Name: op.Artifact.Name, Digest: ref.Digest}
		// The annotation the walk appended immediately after carries the
		// episode identity and the sequence the pointer is named for.
		if i+1 >= len(proj.Ops) || proj.Ops[i+1].Kind != opAppend || proj.Ops[i+1].Event == nil ||
			proj.Ops[i+1].Event.Name != op.Artifact.Name {
			t.Fatalf("episode artifact %q is not followed by its %s annotation; the two ops must stay "+
				"adjacent so a reader can attribute the artifact", op.Artifact.Name, runner.LearningEpisodeInjectedKind)
		}
		ev := proj.Ops[i+1].Event
		entry.EpisodeID = annotationString(ev.Runner, "episodeId")
		entry.Pointer = runner.LearningEpisodePointerName(uint64(annotationInt(ev.Runner, "sourceSeq")))
		entry.NextAttempt = annotationInt(ev.Runner, "nextAttempt")
		out = append(out, entry)
	}
	return out
}

// mustQueryProjection reads a run's journal projection back through the
// production query path, which re-derives it from history rather than from
// live state.
func mustQueryProjection(ctx context.Context, t *testing.T, c client.Client, workflowID string) JournalProjection {
	t.Helper()
	proj, err := queryProjection(ctx, c, workflowID)
	if err != nil {
		t.Fatalf("query journal projection: %v", err)
	}
	return proj
}

// The migration guard for #3931, asserted against a real recorded history.
//
// #3931 changed the episode's BYTES: nextAttempt is serialized into the
// episode, so it is inside the artifact's content digest and inside the digest
// the injected pointer carries. A worker that replayed a pre-change history
// with the post-change derivation would re-derive a different artifact than the
// one that history recorded, which is a run's journal disagreeing with itself
// about the correction a stage was dispatched with.
//
// The seam is workflow.GetVersion(learningEpisodeTargetAttemptChange), taken on
// the injection path only. This test proves the two halves that make it real:
//
//  1. the marker is actually RECORDED — a MarkerRecorded history event carrying
//     the change id — so a future worker replaying this history is pinned to
//     version 1 and cannot silently adopt a third derivation;
//  2. a history recorded WITHOUT the marker (the pre-change shape) still
//     replays, taking the DefaultVersion branch, which passes
//     TargetNextAttempt 0 and falls back to the pre-change SourceAttempt+1.
//
// The second half is stated in internal/runner by
// TestBuildLearningEpisodeFallsBackToThePreChangeDerivation, which pins the
// fallback to the exact pre-change arithmetic; what is asserted here is that
// the engine reaches it, by replaying a history whose marker has been stripped.
func TestEngineLearningEpisodeVersionMarkerIsRecorded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error",
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})
	temporalClient := server.Client()

	const id = "e10-version-marker"
	exec := newScriptedExec(learningEpisodeSendBackScript())
	w := temporalworker.New(temporalClient, id, temporalworker.Options{})
	RegisterWith(w, &Activities{
		Goober:     exec,
		Det:        exec,
		Auto:       gate.NewAutomatedEvaluator(),
		Workspaces: testWorkspaces(t),
	})
	if err := w.Start(); err != nil {
		t.Fatalf("start Temporal worker: %v", err)
	}
	t.Cleanup(w.Stop)

	in := runInput(id, parityLearningEpisodeSendBackSpec())
	in.RunID = id
	in.TriggerKind = string(journal.TriggerManual)
	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: id, TaskQueue: id,
	}, Run, in)
	if err != nil {
		t.Fatalf("execute workflow: %v", err)
	}
	var result RunResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}

	history := &historypb.History{}
	iter := temporalClient.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false,
		enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatalf("read workflow history: %v", err)
		}
		history.Events = append(history.Events, event)
	}

	markers := 0
	for _, event := range history.Events {
		attrs := event.GetMarkerRecordedEventAttributes()
		if attrs == nil || attrs.MarkerName != temporalVersionMarkerName {
			continue
		}
		if strings.Contains(marshalMarkerDetails(attrs.Details), learningEpisodeTargetAttemptChange) {
			markers++
		}
	}
	if markers == 0 {
		t.Fatalf("no %s marker for change id %q in a history that injected learning episodes.\n"+
			"Without the marker a worker replaying this history is not pinned to version %d, and a "+
			"future third derivation of the target attempt would re-derive different episode bytes "+
			"than the ones this run recorded",
			temporalVersionMarkerName, learningEpisodeTargetAttemptChange, learningEpisodeTargetAttempt)
	}
	t.Logf("recorded %d %s marker(s) for change id %q (version %d)",
		markers, temporalVersionMarkerName, learningEpisodeTargetAttemptChange, learningEpisodeTargetAttempt)

	// (1) The recorded history replays as itself.
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("replay a versioned learning-episode history: %v", err)
	}

}

// The compatibility half of the migration claim, on a history recorded by a
// build that PREDATES #3931: testdata/learning-episode-prechange-history.json,
// recorded against a dev server with the target-attempt derivation reverted, so
// it carries no marker for learning-episode-target-attempt and its episodes
// were built with the old SourceAttempt+1 arithmetic.
//
// What this proves is that an in-flight run at deploy time CONTINUES. #3931
// moves a value the walk derives, and moving a derived value is exactly the
// kind of change that can alter the command sequence downstream of it — a
// different pointer set reaching a dispatch, a different routing decision, a
// different activity. The replayer is what says it did not.
//
// What it deliberately does NOT prove is that the bytes are the same. Recording
// an artifact is an in-walk projection op, not a Temporal command, so a walk
// that re-derived a DIFFERENT episode digest on replay would still replay
// clean. That claim is the version guard's, and it is pinned separately and
// exactly: learningEpisodeTargetAttemptFor's two branches
// (TestLearningEpisodeTargetAttemptIsVersioned), the fallback arithmetic
// itself (internal/runner's TestBuildLearningEpisodeFallsBackToThePreChangeDerivation),
// and the marker's presence in new histories
// (TestEngineLearningEpisodeVersionMarkerIsRecorded). Splitting the claims is
// the point: a single test asserting all three would be green for the wrong
// reason.
//
// The fixture is the nontrivial send-back, the only shape where the two
// derivations disagree at all: on a trivial send-back subject+1 and the
// target's next attempt are the same number, so a fixture built from one would
// be compatible under either branch and measure nothing.
func TestEngineLearningEpisodePreChangeHistoryReplays(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "learning-episode-prechange-history.json"))
	if err != nil {
		t.Fatal(err)
	}
	history := &historypb.History{}
	if err := protojson.Unmarshal(data, history); err != nil {
		t.Fatalf("decode recorded history: %v", err)
	}

	// The fixture must really be the pre-change shape. A fixture accidentally
	// re-recorded against this build would carry the marker, take the version-1
	// branch on replay, and grade green while testing nothing.
	for _, event := range history.Events {
		attrs := event.GetMarkerRecordedEventAttributes()
		if attrs == nil || attrs.MarkerName != temporalVersionMarkerName {
			continue
		}
		if strings.Contains(marshalMarkerDetails(attrs.Details), learningEpisodeTargetAttemptChange) {
			t.Fatalf("the pre-change fixture carries a %q version marker; it was recorded against a "+
				"build that already had #3931 and cannot demonstrate the DefaultVersion branch",
				learningEpisodeTargetAttemptChange)
		}
	}
	// And it must really be a learning-episode history, or the marker check
	// above is satisfied by a history that never reaches the seam at all.
	if !historySchedules(history, "EvaluateAutomated") {
		t.Fatal("the pre-change fixture scheduled no gate evaluation; it cannot have taken a retry arm")
	}

	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("a PRE-#3931 history does not replay under the target-attempt code: %v\n"+
			"workflow.GetVersion(%q) must answer DefaultVersion for a history that carries no marker "+
			"for it, so the walk re-derives SourceAttempt+1 and commits the artifacts the run "+
			"originally recorded", err, learningEpisodeTargetAttemptChange)
	}
}

// The #3931 version guard's two branches, stated directly.
//
// This is the test the pre-change replay CANNOT be: replaying proves the
// command sequence survived, and the thing the guard actually protects is a
// value that never becomes a command. Both branches are asserted here, against
// an addressing whose two derivations disagree — which is the only kind that
// can detect a guard wired to the wrong side.
func TestLearningEpisodeTargetAttemptIsVersioned(t *testing.T) {
	// A nontrivial send-back: the subject is on its first attempt while the
	// target is about to take its third. The two derivations disagree, which
	// is what makes each branch identifiable.
	addressing := runner.LearningEpisodeAddressing{SourceSeq: 19, SourceAttempt: 1, TargetNextAttempt: 3}

	if got := learningEpisodeTargetAttemptFor(workflow.DefaultVersion, addressing); got != 0 {
		t.Fatalf("DefaultVersion selected targetNextAttempt %d, want 0 — 0 is what makes "+
			"BuildLearningEpisode fall back to SourceAttempt+1, the arithmetic a pre-#3931 history was "+
			"recorded with; anything else makes a replayed run commit different episode bytes than the "+
			"ones its own history says it committed", got)
	}
	if got := learningEpisodeTargetAttemptFor(learningEpisodeTargetAttempt, addressing); got != 3 {
		t.Fatalf("version %d selected targetNextAttempt %d, want the target's own next attempt (3)",
			learningEpisodeTargetAttempt, got)
	}

	// The guard must be a floor, not an equality: a later change id adds a
	// higher version, and the target-attempt derivation stays selected.
	if got := learningEpisodeTargetAttemptFor(learningEpisodeTargetAttempt+1, addressing); got != 3 {
		t.Fatalf("a FUTURE version selected targetNextAttempt %d, want 3 — the guard is a floor; an "+
			"equality check would silently revert this derivation the next time the change id gains a "+
			"version", got)
	}
}

// historySchedules reports whether a recorded history scheduled an activity of
// the given type.
func historySchedules(history *historypb.History, activityType string) bool {
	for _, event := range history.Events {
		if attrs := event.GetActivityTaskScheduledEventAttributes(); attrs != nil &&
			attrs.ActivityType.GetName() == activityType {
			return true
		}
	}
	return false
}

// temporalVersionMarkerName is the SDK's marker name for workflow.GetVersion.
// It is unexported inside the SDK (internal_command_state_machine.go), so it is
// restated here; the test fails loudly if it ever stops matching, because zero
// markers is a hard failure rather than a skip.
const temporalVersionMarkerName = "Version"

// marshalMarkerDetails renders a marker's details payloads as text so a change
// id can be found in them without depending on the SDK's internal encoding of
// the version marker.
func marshalMarkerDetails(details map[string]*commonpb.Payloads) string {
	var sb strings.Builder
	for key, payloads := range details {
		sb.WriteString(key)
		for _, payload := range payloads.GetPayloads() {
			sb.Write(payload.GetData())
		}
	}
	return sb.String()
}

// The local runner's retry arm has a SECOND route for the injected pointer:
// when a branch is active it calls parallel.recordCurrentPointer, scoping the
// pointer to that branch instead of the run-level set. #3913's port has no such
// route, and this test is why that is correct rather than an omission.
//
// spec.parallels is refused at run START on the engine (ruling R9), so the
// engine walk can never reach a retry arm inside a branch: the branch-scoped
// route has no reachable counterpart to port. That reasoning is load-bearing
// for the parity claim — the harness cannot state it, because a parallels
// fixture never starts on the engine side and so can never be a parity row.
//
// Pinning it here makes it an executable claim: the day parallels start walking
// on the engine, this test goes red and the branch-scoped route must be ported
// with it.
func TestLearningEpisodeParallelRouteIsRunnerOnly(t *testing.T) {
	spec := parityLearningEpisodeSpec()
	spec.Parallels = []apiv1.Parallel{{
		Name:     "fan",
		Branches: []apiv1.Branch{{Name: "security", Start: "implement"}},
	}}
	err := refuseUnsupportedEngineFeatures("learning-parallel", spec)
	if err == nil {
		t.Fatal("the engine accepted a definition declaring spec.parallels.\n" +
			"internal/runner's retry arm scopes the injected learning.episode pointer to the ACTIVE BRANCH " +
			"(parallel.recordCurrentPointer). The engine's port deliberately has no branch arm, which is only " +
			"sound while parallels are refused at start. Port the branch-scoped route before lifting the refusal.")
	}
	if !errors.Is(err, ErrParallelsUnsupported) {
		t.Fatalf("refusal = %v, want ErrParallelsUnsupported", err)
	}
}

// A gate that does not retry must record nothing. The negative parity row makes
// the same claim about both runners together; this makes it about the engine
// alone, so a port that started injecting unconditionally fails here even if
// the local runner were changed to match.
func TestEngineInjectsNoEpisodeWithoutARetry(t *testing.T) {
	spec := fixtureSpec("implement",
		[]apiv1.Task{detTask("implement", "review"), detTask("park", wf.TargetAbort)},
		[]apiv1.Gate{statusGate("review", map[string]string{
			"pass": wf.TerminalComplete, "fail": "park",
		})},
	)
	events, _, err := shortcutRunWithID(t, "e10-negative", spec,
		map[string][]scriptedCall{
			"implement": {fail("assertion_failed", "an assertion tripped")},
			"park":      {succeed(map[string]interface{}{"parked": "true"})},
		})
	if err != nil {
		t.Fatalf("engine walk: %v", err)
	}
	if got := learningEpisodes(paritySide{Name: "engine", Events: events}); len(got) != 0 {
		t.Fatalf("engine injected %d learning episode(s) on a gate that did not retry, want 0:\n%s",
			len(got), formatEventSeqs(events))
	}
	// And nothing was downgraded: with no derived pointer threaded in, the
	// stages keep the integrity they earned.
	for _, ev := range events {
		if ev.Type != journal.EventStageFinished {
			continue
		}
		if ev.Integrity == apiv1.IntegrityDerived {
			t.Fatalf("stage %q finished derived without any injection; the integrity downgrade must be a "+
				"consequence of the injected pointer, not of evaluating a gate", ev.Stage)
		}
	}
}

// TestEngineInjectsNoEpisodeWhenTheRepassAttemptIsZero is the engine-side
// Attempt==0 negative for the #3929 ruling, and it is deliberately NOT the
// same test as TestEngineInjectsNoEpisodeWithoutARetry above.
//
// That one fails with assertion_failed, which retryFailureClass does not
// recognise: the gate never enters the retry arm at all, so no episode is a
// trivially correct outcome that would survive any predicate whatsoever.
//
// This one fails with nonzero_exit through a status-equals gate, which is
// exactly the shape the retry arm DOES classify. The engine therefore reaches
// the injection site with a live retry route and a retryable, policy-classed
// failure, and the only thing standing between it and a fabricated episode is
// the ruling: the gate's fail branch routes ONWARD to park, park has never
// run, resolveGateOutcome finds no upstream entry for it, and the repass
// attempt is 0.
//
// Before #3929 the engine answered this question with its own re-derived
// gateSendsBack predicate rather than with the evidenced repass attempt. The
// two agreed here, which is why this row went unnoticed; pinning the negative
// against the CANONICAL predicate is what stops a future edit to either
// derivation from silently re-opening it.
func TestEngineInjectsNoEpisodeWhenTheRepassAttemptIsZero(t *testing.T) {
	spec := fixtureSpec("implement",
		[]apiv1.Task{detTask("implement", "review"), detTask("park", wf.TargetAbort)},
		[]apiv1.Gate{statusGate("review", map[string]string{
			"pass": wf.TerminalComplete, "fail": "park",
		})},
	)
	events, _, err := shortcutRunWithID(t, "e10-attempt-zero", spec,
		map[string][]scriptedCall{
			"implement": {fail("nonzero_exit", "exit status 1")},
			"park":      {succeed(map[string]interface{}{"parked": "true"})},
		})
	if err != nil {
		t.Fatalf("engine walk: %v", err)
	}

	// The premise: this really is the retry-classified route, and its
	// evidenced repass attempt really is 0. Without this, a future change that
	// stopped classifying nonzero_exit would make the assertion below pass for
	// the wrong reason.
	decisions := retryDecisions(paritySide{Name: "engine", Events: events})
	if len(decisions) != 1 {
		t.Fatalf("retry decisions = %d (%+v), want exactly 1 — the fixture must reach the retry arm:\n%s",
			len(decisions), decisions, formatEventSeqs(events))
	}
	if got := decisions[0]; got.RepassAttempt != 0 || got.Target != "park" ||
		got.FailureClass != string(journal.AttemptPolicy) || got.FailureCode != "nonzero_exit" {
		t.Fatalf("retry decision = %+v, want policy/nonzero_exit -> park at repass attempt 0", got)
	}

	if got := learningEpisodes(paritySide{Name: "engine", Events: events}); len(got) != 0 {
		t.Fatalf("engine injected %d learning episode(s) on a forward branch at repass attempt 0, want 0:\n%s",
			len(got), formatEventSeqs(events))
	}
	for _, ev := range events {
		if ev.Type == journal.EventStageFinished && ev.Integrity == apiv1.IntegrityDerived {
			t.Fatalf("stage %q finished derived with no injection; a forward branch must not downgrade integrity", ev.Stage)
		}
	}
}

// --- small readers ----------------------------------------------------------

func findArtifactRecorded(events []journal.Event, name string) (journal.Event, bool) {
	for _, ev := range events {
		if ev.Type == journal.EventArtifactRecorded && ev.Name == name {
			return ev, true
		}
	}
	return journal.Event{}, false
}

func formatEventSeqs(events []journal.Event) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("  ")
		b.WriteString(string(ev.Type))
		if ev.Name != "" {
			b.WriteString(" name=" + ev.Name)
		}
		if ev.Stage != "" {
			b.WriteString(" stage=" + ev.Stage)
		}
		b.WriteString("\n")
	}
	return b.String()
}
