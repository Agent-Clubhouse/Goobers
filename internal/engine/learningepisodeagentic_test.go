package engine

// Engine-side coverage for #3942: the agentic reviewer repass.
//
// learningepisode_test.go covers replay, determinism and naming for the
// injection on a DETERMINISTIC failure through an AUTOMATED gate — the shape
// the retry classifier accepts, and therefore the one shape where the
// classifier's answer and the #3929 ruling's answer coincide. This file covers
// the shape where they do NOT: an agentic reviewer resolving needs-changes back
// into its implementer, which the classifier declines and the ruling admits.
//
// The parity row (parity_row_learning_episode_agentic_test.go) proves the two
// drivers AGREE on it. These tests prove the engine's half is correct and
// replay-safe on its own, which parity cannot: the harness walks each side once,
// in-process, and never replays.

import (
	"context"
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
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// agenticLearningEpisodeSpec is a DETERMINISTIC implementer under an AGENTIC
// review gate whose needs-changes branch re-enters it.
//
// The subject is deliberately deterministic: an agentic subject reporting
// success over the empty diff scriptedExec leaves behind would trip #415's
// empty-diff fast-fail and never reach a reviewer at all, so the fixture would
// measure the fast-fail instead of the injection. A deterministic subject under
// the same empty diff is still reviewed, which is what row
// E5-empty-diff-deterministic-subject pins.
func agenticLearningEpisodeSpec() apiv1.WorkflowSpec {
	return fixtureSpec("implement",
		[]apiv1.Task{detTask("implement", "review")},
		[]apiv1.Gate{reviewerGate("review", map[string]string{
			"pass":          wf.TerminalComplete,
			"fail":          wf.TargetAbort,
			"needs-changes": "implement",
		})},
	)
}

// agenticLearningEpisodeVerdicts sends the implementer back once, then accepts.
// The rationale is load-bearing: BuildLearningEpisode's verdict arm takes the
// episode's correctionFeedback from it, so an empty one would silently exercise
// the non-verdict fallback instead.
func agenticLearningEpisodeVerdicts() map[string][]apiv1.Verdict {
	return map[string][]apiv1.Verdict{
		"review": {
			{
				Decision:  apiv1.VerdictNeedsChanges,
				Summary:   "the parser still accepts empty input",
				Rationale: "reject an empty document before the token scan rather than after it",
			},
			{
				Decision:  apiv1.VerdictPass,
				Summary:   "empty input is rejected now",
				Rationale: "the guard is in the right place and covered",
			},
		},
	}
}

// agenticLearningEpisodeScript succeeds on both implement dispatches: the
// repass here is driven by the REVIEWER, not by a stage failure, which is the
// whole distinction #3942 is about.
func agenticLearningEpisodeScript() map[string][]scriptedCall {
	return map[string][]scriptedCall{
		"implement": {
			succeed(map[string]interface{}{"attempt": "1"}),
			succeed(map[string]interface{}{"attempt": "2"}),
		},
	}
}

// agenticEngineWalk runs one agentic fixture through the engine in Temporal's
// test environment. It is engineWalk with a scripted reviewer attached — the
// shared helper takes only a stage script, and a gate the fixture does not
// script a verdict for fails loudly rather than returning a zero verdict.
func agenticEngineWalk(
	t *testing.T, runID string, spec apiv1.WorkflowSpec,
	script map[string][]scriptedCall, verdicts map[string][]apiv1.Verdict,
) ([]journal.Event, RunResult) {
	t.Helper()
	exec := newScriptedExec(script)
	exec.verdicts = verdicts
	in := RunInput{
		RunID:                  runID,
		Gaggle:                 "web",
		WorkflowName:           "agentic-episode",
		Version:                1,
		PreviewFeaturesEnabled: boolPointer(true),
		Spec:                   spec,
		RepoRef:                apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		TriggerKind:            string(journal.TriggerManual),
	}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	env.RegisterActivity(&Activities{
		Goober: exec, Det: exec, Auto: gate.NewAutomatedEvaluator(), Workspaces: testWorkspaces(t),
	})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("engine walk: %v", err)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("engine result: %v", err)
	}
	return projectEngineJournal(t, env), res
}

// TestEngineAgenticRepassInjectsALearningEpisode is #3942's engine-side
// acceptance.
//
// Before it, the engine's injection site read
// `retryRoute && LearningEpisodeAppliesToRepass(gr.Attempt)`. `retryRoute` is
// retryDecisionApplies(gr, retryable), and retryable comes from
// runner.RetryFailureClassForGateResult, which declines a reviewer verdict: not
// an automated status-equals gate, not `infra`. So the engine reached this
// point with a live, non-escalated, repassAttempt-1 branch and injected
// nothing.
//
// The scope negative rides along: the retry-decision annotation must STILL be
// absent, because the classifier is unchanged. Widening the episode must not
// start asserting a policy/infrastructure failure class over a reviewer's
// opinion — that class is what priorRepassCause reads to tell an
// infrastructure repass from a content one.
func TestEngineAgenticRepassInjectsALearningEpisode(t *testing.T) {
	events, res := agenticEngineWalk(t, "e10-agentic", agenticLearningEpisodeSpec(),
		agenticLearningEpisodeScript(), agenticLearningEpisodeVerdicts())
	if res.Status != StatusCompleted {
		t.Fatalf("run status = %q, want completed — the repass must converge, not escalate", res.Status)
	}
	side := paritySide{Name: "engine", Events: events}
	episodes := learningEpisodes(side)
	if len(episodes) != 1 {
		t.Fatalf("engine injected %d learning episode(s) on an agentic needs-changes repass, want 1:\n%s",
			len(episodes), formatEventSeqs(events))
	}
	record := episodes[0]
	if record.Target != "implement" || record.Gate != "review" {
		t.Fatalf("episode targets %s via gate %s, want implement via review", record.Target, record.Gate)
	}
	if !record.ArtifactRecorded {
		t.Fatalf("episode %q was annotated but no artifact was recorded for it", record.EpisodeID)
	}
	if record.Integrity != apiv1.IntegrityDerived {
		t.Fatalf("episode annotation integrity = %q, want %q", record.Integrity, apiv1.IntegrityDerived)
	}
	if record.Correction == "" {
		t.Fatal("episode carries no correctionFeedback; the reviewer's rationale is the payload the " +
			"repass is supposed to argue with, and its absence means the verdict arm was bypassed")
	}
	if len(record.FindingIdentities) == 0 {
		t.Fatal("episode carries no finding identities; cross-attempt correlation has nothing to key on")
	}

	// The scope claim: the classifier is untouched.
	if got := retryDecisions(side); len(got) != 0 {
		t.Fatalf("engine wrote %d retry-decision annotation(s) for an agentic needs-changes, want 0 — "+
			"#3942 widened the injection predicate, never runner.RetryFailureClassForGateResult: %+v",
			len(got), got)
	}

	// The pointer reached the re-entered dispatch, and downgraded it.
	grades := stageFinishedIntegrity(side, "implement")
	if len(grades) != 2 || grades[0] != apiv1.IntegrityTrusted || grades[1] != apiv1.IntegrityDerived {
		t.Fatalf("implement stage.finished integrity = [%s], want [trusted derived] — the derived episode "+
			"pointer is the floor of the repass's inputs, and that downgrade is admission control",
			joinIntegrity(grades))
	}
}

// TestEngineAgenticGateInjectsNoEpisodeOnATerminalBranch is the negative: the
// SAME agentic gate resolving `fail` routes to @abort, a reserved terminal
// with no dispatch to inject into and no next attempt to correct.
//
// Without it, "inject whenever a reviewer is unhappy" would leave the positive
// test above green while fabricating a correction for a run that is over.
func TestEngineAgenticGateInjectsNoEpisodeOnATerminalBranch(t *testing.T) {
	events, res := agenticEngineWalk(t, "e10-agentic-terminal", agenticLearningEpisodeSpec(),
		agenticLearningEpisodeScript(),
		map[string][]apiv1.Verdict{"review": {{
			Decision: apiv1.VerdictFail, Summary: "unscopeable", Rationale: "this needs a human",
		}}},
	)
	if res.Status != StatusBlocked {
		t.Fatalf("run status = %q, want blocked (@abort) — the fixture must actually take the fail branch", res.Status)
	}
	side := paritySide{Name: "engine", Events: events}
	if got := learningEpisodes(side); len(got) != 0 {
		t.Fatalf("engine injected %d learning episode(s) on a branch to @abort, want 0: %s",
			len(got), joinLearningEpisodes(got))
	}
	for _, ev := range events {
		if strings.HasPrefix(ev.Name, learningEpisodeArtifactPrefix) {
			t.Fatalf("seq %d recorded a learning episode artifact %q on a terminal branch", ev.Seq, ev.Name)
		}
		// And nothing was downgraded: with no derived pointer threaded in, the
		// stages keep the integrity they earned.
		if ev.Type == journal.EventStageFinished && ev.Integrity == apiv1.IntegrityDerived {
			t.Fatalf("stage %q finished derived without any injection", ev.Stage)
		}
	}
}

// TestEngineAgenticLearningEpisodeIsDeterministic: the episode's bytes must be
// a pure function of deterministic workflow state on the agentic arm too. A
// clock, a random source or a map iteration reaching the verdict path would
// change the digest — and the digest is the artifact name, the pointer name and
// the envelope, so a differing one is a nondeterminism panic that wedges the
// run rather than a diff anybody reads.
func TestEngineAgenticLearningEpisodeIsDeterministic(t *testing.T) {
	first, _ := agenticEngineWalk(t, "e10-agentic-det", agenticLearningEpisodeSpec(),
		agenticLearningEpisodeScript(), agenticLearningEpisodeVerdicts())
	second, _ := agenticEngineWalk(t, "e10-agentic-det", agenticLearningEpisodeSpec(),
		agenticLearningEpisodeScript(), agenticLearningEpisodeVerdicts())
	if err := diffConformanceViews(first, second); err != nil {
		t.Fatalf("journal projections differ between two identical agentic walks: %v", err)
	}
	firstEpisodes := learningEpisodes(paritySide{Name: "first", Events: first})
	if len(firstEpisodes) != 1 {
		t.Fatalf("first walk injected %d learning episode(s), want 1:\n%s",
			len(firstEpisodes), formatEventSeqs(first))
	}
	secondEpisodes := learningEpisodes(paritySide{Name: "second", Events: second})
	if err := diffLearningEpisodes("second walk", secondEpisodes, firstEpisodes); err != nil {
		t.Fatalf("agentic learning episodes differ between two identical walks: %v", err)
	}
}

// TestEngineAgenticLearningEpisodeHistoryReplays is the REPLAY half, and the
// only place a genuine replay happens: the test environment executes a workflow
// once and hands back the result, so only NewWorkflowReplayer over a real
// recorded history exercises the determinism checker.
//
// It is not redundant with the deterministic sibling above. The injection is a
// walk decision whose output is a content-addressed digest threaded into the
// next dispatch, and the agentic arm reaches it through an ACTIVITY RESULT (the
// reviewer's verdict) rather than through a locally scripted failure. That is
// exactly the class of input a replay re-feeds from history, so a verdict field
// leaking into the digest in a non-replayable way — or a source-sequence
// derivation that shifted because the reviewer's own events changed the op
// numbering — would show up here and nowhere else.
func TestEngineAgenticLearningEpisodeHistoryReplays(t *testing.T) {
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

	const id = "e10-agentic-replay"
	exec := newScriptedExec(agenticLearningEpisodeScript())
	exec.verdicts = agenticLearningEpisodeVerdicts()
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

	in := runInput(id, agenticLearningEpisodeSpec())
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
	if result.Status != StatusCompleted {
		t.Fatalf("run status = %q, want completed", result.Status)
	}

	recorded := mustQueryProjection(ctx, t, temporalClient, run.GetID())
	episodes := projectionEpisodes(t, recorded)
	if len(episodes) != 1 {
		t.Fatalf("agentic run injected %d learning episode(s), want exactly 1 — the reviewer must send "+
			"implement back once", len(episodes))
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
		t.Fatalf("replay an agentic learning-episode history: %v\n"+
			"the in-walk artifact seam must be a pure function of deterministic workflow state, "+
			"including the reviewer verdict it reads back from an activity result", err)
	}

	after := mustQueryProjection(ctx, t, temporalClient, run.GetID())
	replayed := projectionEpisodes(t, after)
	if len(replayed) != len(episodes) {
		t.Fatalf("replayed projection has %d learning episode(s), want %d", len(replayed), len(episodes))
	}
	for i := range episodes {
		if replayed[i] != episodes[i] {
			t.Fatalf("agentic learning episode %d is not stable across replay:\n  recorded: %+v\n  replayed: %+v",
				i, episodes[i], replayed[i])
		}
	}
	t.Logf("replayed agentic learning episode: artifact=%s digest=%s pointer=%s id=%s",
		episodes[0].Name, episodes[0].Digest, episodes[0].Pointer, episodes[0].EpisodeID)
}
