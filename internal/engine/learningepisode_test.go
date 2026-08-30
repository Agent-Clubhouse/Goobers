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
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"

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
	spec, script := parityLearningEpisodeSpec(), learningEpisodeScript()
	first, _, firstErr := shortcutRunWithID(t, "e10-det", spec, script)
	second, _, secondErr := shortcutRunWithID(t, "e10-det", spec, script)
	if (firstErr == nil) != (secondErr == nil) {
		t.Fatalf("walk error differs between runs: %v vs %v", firstErr, secondErr)
	}
	if err := diffConformanceViews(first, second); err != nil {
		t.Fatalf("journal projections differ between two identical walks: %v", err)
	}
	firstEpisodes := learningEpisodes(paritySide{Name: "first", Events: first})
	secondEpisodes := learningEpisodes(paritySide{Name: "second", Events: second})
	if len(firstEpisodes) != 1 {
		t.Fatalf("first walk injected %d learning episode(s), want exactly 1:\n%s",
			len(firstEpisodes), formatEventSeqs(first))
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

	taskQueue := "e10-replay"
	exec := newScriptedExec(learningEpisodeScript())
	w := temporalworker.New(temporalClient, taskQueue, temporalworker.Options{})
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

	in := runInput("e10-replay", parityLearningEpisodeSpec())
	in.RunID = "e10-replay"
	in.TriggerKind = string(journal.TriggerManual)
	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "e10-replay",
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
	if len(episodes) != 1 {
		t.Fatalf("engine run injected %d learning episode(s), want exactly 1 — the fail branch must re-enter "+
			"implement and fire the retry arm once", len(episodes))
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
	t.Logf("replayed learning episode: artifact=%s digest=%s pointer=%s id=%s",
		episodes[0].Name, episodes[0].Digest, episodes[0].Pointer, episodes[0].EpisodeID)
}

// projectionDigest is the in-walk-derived identity of one injected episode, in
// the form the replay assertion compares.
type projectionDigest struct {
	Name      string
	Digest    string
	Pointer   string
	EpisodeID string
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
