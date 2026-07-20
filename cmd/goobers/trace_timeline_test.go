package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func TestTraceTimelineCompletedRunTextAndJSON(t *testing.T) {
	root := t.TempDir()
	const runID = "timeline-completed"
	base := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	next := base
	clock := func() time.Time {
		current := next
		next = next.Add(1500 * time.Millisecond)
		return current
	}
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	transcript := []byte(
		`{"role":"assistant","model":"gpt-5.6","usage":{"input_tokens":100,"output_tokens":20,"requests":1,"cost":0.25}}` + "\n",
	)
	if _, err := run.RecordSpan(runID+":implement", "copilot-cli.transcript", transcript); err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: string(apiv1.VerdictPass), Target: "local-ci"},
		{Type: journal.EventStageStarted, Stage: "local-ci", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "local-ci", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", runID, root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"timeline:\n  implement attempts=1",
		"attempt #1",
		"gate review=pass -> local-ci",
		"attempt #1 usage model=gpt-5.6 in=100 out=20 requests=1 cost=0.25",
		"local-ci attempts=1",
		"\nevents:\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("trace stdout missing %q:\n%s", want, stdout)
		}
	}

	jsonCode, jsonStdout, jsonStderr := runArgs(t, "trace", "--json", runID, root)
	if jsonCode != 0 {
		t.Fatalf("trace --json: code = %d, stderr = %q", jsonCode, jsonStderr)
	}
	var got traceJSONResult
	if err := json.Unmarshal([]byte(jsonStdout), &got); err != nil {
		t.Fatalf("trace --json produced invalid JSON: %v\n%s", err, jsonStdout)
	}
	if len(got.Timeline) != 2 || got.Timeline[0].Stage != "implement" || got.Timeline[1].Stage != "local-ci" {
		t.Fatalf("timeline = %+v", got.Timeline)
	}
	implement := got.Timeline[0].Attempts[0]
	if implement.StartedAt == nil || implement.FinishedAt == nil || implement.DurationMillis == nil {
		t.Fatalf("implement attempt missing timestamps: %+v", implement)
	}
	wantDuration := implement.FinishedAt.Sub(*implement.StartedAt).Milliseconds()
	if *implement.DurationMillis != wantDuration {
		t.Fatalf("durationMillis = %d, want timestamp delta %d", *implement.DurationMillis, wantDuration)
	}
	if len(implement.Gates) != 1 ||
		implement.Gates[0].Name != "review" ||
		implement.Gates[0].Verdict != string(apiv1.VerdictPass) {
		t.Fatalf("implement gates = %+v", implement.Gates)
	}
	if len(implement.Usage) != 1 ||
		implement.Usage[0].InputTokens == nil ||
		*implement.Usage[0].InputTokens != 100 ||
		implement.Usage[0].Cost == nil ||
		*implement.Usage[0].Cost != 0.25 {
		t.Fatalf("implement usage = %+v", implement.Usage)
	}
	if got.TerminalCause != nil {
		t.Fatalf("completed terminal cause = %+v, want nil", got.TerminalCause)
	}
}

func TestBuildTraceTimelineRetriesGateUsageAndTornEvents(t *testing.T) {
	base := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	events := timelineRunEvents(
		journal.Event{Seq: 1, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1, Time: base},
		journal.Event{
			Seq: 2, Type: journal.EventError, Stage: "implement", Attempt: 1, Time: base.Add(2 * time.Minute),
			Error: &journal.ErrorDetail{Code: "executor_error", Message: "temporary failure"},
		},
		journal.Event{
			Seq: 3, Type: journal.EventStageStarted, Stage: "implement", Attempt: 2,
			AttemptClass: journal.AttemptInfra, Time: base.Add(3 * time.Minute),
		},
		journal.Event{
			Seq: 5, Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
			AttemptClass: journal.AttemptInfra, Status: string(apiv1.ResultSuccess), Time: base.Add(8 * time.Minute),
		},
		journal.Event{Seq: 6, Type: journal.EventGateStarted, Gate: "review", Time: base.Add(9 * time.Minute)},
		journal.Event{
			Seq: 8, Type: journal.EventGateEvaluated, Gate: "review",
			Verdict: string(apiv1.VerdictNeedsChanges), Target: "implement", Time: base.Add(10 * time.Minute),
		},
		journal.Event{
			Seq: 9, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1, Time: base.Add(11 * time.Minute),
		},
		journal.Event{
			Seq: 10, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
			Status: string(apiv1.ResultSuccess), Time: base.Add(12 * time.Minute),
		},
		journal.Event{
			Seq: 11, Type: journal.EventStageFinished, Stage: "torn-stage", Attempt: 1,
			Status: string(apiv1.ResultSuccess), Time: base.Add(11 * time.Minute),
		},
	)
	events = append(events, readservice.RunEvent{
		KnownSchema: false,
		Seq:         12,
		Type:        "future.stage",
		Stage:       "ignored",
	})
	transcripts := []readservice.TranscriptContent{
		{
			Seq: 4, Stage: "implement",
			Bytes: []byte(`{"role":"assistant","model":"agent-model","usage":{"input_tokens":8,"output_tokens":3}}` + "\n"),
		},
		{
			Seq: 7, Stage: "review",
			Bytes: []byte(`{"role":"assistant","model":"review-model","usage":{"input_tokens":5,"cost":0.1}}` + "\n"),
		},
	}

	timeline := buildTraceTimeline(
		readservice.RunDetail{RunSummary: readservice.RunSummary{Phase: journal.PhaseCompleted}},
		events,
		transcripts,
		base.Add(20*time.Minute),
	)
	if len(timeline) != 2 || timeline[0].Stage != "implement" || timeline[1].Stage != "torn-stage" {
		t.Fatalf("timeline = %+v", timeline)
	}
	attempts := timeline[0].Attempts
	if len(attempts) != 3 ||
		attempts[0].Status != string(apiv1.ResultFailure) ||
		attempts[0].DurationMillis == nil ||
		*attempts[0].DurationMillis != int64((2*time.Minute)/time.Millisecond) ||
		attempts[1].Class != string(journal.AttemptInfra) ||
		attempts[1].DurationMillis == nil ||
		*attempts[1].DurationMillis != int64((5*time.Minute)/time.Millisecond) {
		t.Fatalf("attempts = %+v", attempts)
	}
	if !attempts[2].Repass || attempts[2].Class != "initial" {
		t.Fatalf("repass attempt = %+v", attempts[2])
	}
	if len(attempts[1].Usage) != 1 || attempts[1].Usage[0].OutputTokens == nil || *attempts[1].Usage[0].OutputTokens != 3 {
		t.Fatalf("attempt usage = %+v", attempts[1].Usage)
	}
	if len(attempts[1].Gates) != 1 ||
		len(attempts[1].Gates[0].Usage) != 1 ||
		attempts[1].Gates[0].Usage[0].Cost == nil ||
		*attempts[1].Gates[0].Usage[0].Cost != 0.1 {
		t.Fatalf("gate = %+v", attempts[1].Gates)
	}
	torn := timeline[1].Attempts[0]
	if torn.StartedAt != nil || torn.FinishedAt == nil || torn.DurationMillis != nil || torn.InFlight {
		t.Fatalf("torn attempt = %+v", torn)
	}

	var output bytes.Buffer
	printTraceTimeline(&output, timeline, nil)
	if !strings.Contains(output.String(), "attempt #1 repass") {
		t.Fatalf("timeline text missing repass label:\n%s", output.String())
	}
}

func TestBuildTraceTimelineLiveAttemptUsesElapsedSoFar(t *testing.T) {
	base := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	timeline := buildTraceTimeline(
		readservice.RunDetail{RunSummary: readservice.RunSummary{Phase: journal.PhaseRunning}},
		timelineRunEvents(
			journal.Event{Seq: 1, Branch: 1, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1, Time: base},
			journal.Event{Seq: 2, Branch: 1, Type: journal.EventStageHeartbeat, Stage: "implement", Attempt: 1, Time: base.Add(time.Minute)},
			journal.Event{Seq: 3, Branch: 2, Type: journal.EventStageStarted, Stage: "parallel-check", Attempt: 1, Time: base.Add(30 * time.Second)},
		),
		nil,
		base.Add(90*time.Second),
	)
	attempt := timeline[0].Attempts[0]
	if !attempt.InFlight || attempt.DurationMillis == nil || *attempt.DurationMillis != 90000 || attempt.FinishedAt != nil {
		t.Fatalf("live attempt = %+v", attempt)
	}
	parallel := timeline[1].Attempts[0]
	if !parallel.InFlight || parallel.DurationMillis == nil || *parallel.DurationMillis != 60000 {
		t.Fatalf("parallel live attempt = %+v", parallel)
	}
}

func TestBuildTraceTimelineAssociatesGateWithItsBranch(t *testing.T) {
	base := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	timeline := buildTraceTimeline(
		readservice.RunDetail{RunSummary: readservice.RunSummary{Phase: journal.PhaseCompleted}},
		timelineRunEvents(
			journal.Event{Seq: 1, Branch: 1, Type: journal.EventStageStarted, Stage: "branch-one", Attempt: 1, Time: base},
			journal.Event{Seq: 2, Branch: 1, Type: journal.EventStageFinished, Stage: "branch-one", Attempt: 1, Status: string(apiv1.ResultSuccess), Time: base.Add(time.Minute)},
			journal.Event{Seq: 3, Branch: 2, Type: journal.EventStageStarted, Stage: "branch-two", Attempt: 1, Time: base},
			journal.Event{Seq: 4, Branch: 2, Type: journal.EventStageFinished, Stage: "branch-two", Attempt: 1, Status: string(apiv1.ResultSuccess), Time: base.Add(time.Minute)},
			journal.Event{Seq: 5, Branch: 1, Type: journal.EventGateEvaluated, Gate: "review-one", Verdict: string(apiv1.VerdictPass), Target: "done", Time: base.Add(2 * time.Minute)},
		),
		nil,
		base.Add(3*time.Minute),
	)
	if len(timeline[0].Attempts[0].Gates) != 1 ||
		timeline[0].Attempts[0].Gates[0].Name != "review-one" ||
		len(timeline[1].Attempts[0].Gates) != 0 {
		t.Fatalf("branch gate association = %+v", timeline)
	}
}

func TestPrintTraceTimelineBoundsEveryLine(t *testing.T) {
	duration := int64((15 * time.Minute) / time.Millisecond)
	timeline := []traceTimelineStage{{
		Stage: strings.Repeat("very-long-stage-", 12),
		Attempts: []traceTimelineAttempt{{
			Number:         1,
			Class:          "initial",
			Status:         string(apiv1.ResultSuccess),
			DurationMillis: &duration,
			Gates: []traceTimelineGate{{
				Name:    strings.Repeat("review-", 20),
				Verdict: string(apiv1.VerdictNeedsChanges),
				Target:  "implement",
			}},
		}},
	}}
	var output bytes.Buffer
	printTraceTimeline(&output, timeline, &traceTerminalCause{
		Phase:   journal.PhaseFailed,
		Message: strings.Repeat("terminal failure detail ", 20),
	})
	for _, line := range strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n") {
		if len([]rune(line)) > traceTimelineWidth {
			t.Fatalf("timeline line has %d runes, want <= %d: %q", len([]rune(line)), traceTimelineWidth, line)
		}
	}
}

func TestTerminalCauseUsesRunFinishedError(t *testing.T) {
	events := timelineRunEvents(journal.Event{
		Seq:    4,
		Type:   journal.EventRunFinished,
		Status: string(journal.PhaseFailed),
		Error: &journal.ErrorDetail{
			Code:    "resume_refused",
			Message: "workflow definition is unavailable",
		},
	})
	cause := terminalCause(
		readservice.RunDetail{RunSummary: readservice.RunSummary{Phase: journal.PhaseFailed}},
		events,
	)
	if cause == nil ||
		cause.Code != "resume_refused" ||
		cause.Message != "workflow definition is unavailable" ||
		cause.CausalEventSeq != 4 {
		t.Fatalf("terminal cause = %+v", cause)
	}
}

func timelineRunEvents(events ...journal.Event) []readservice.RunEvent {
	projected := make([]readservice.RunEvent, 0, len(events))
	for _, event := range events {
		copy := event
		projected = append(projected, readservice.RunEvent{
			Seq:          event.Seq,
			Type:         event.Type,
			Time:         event.Time,
			KnownSchema:  true,
			Stage:        event.Stage,
			Attempt:      event.Attempt,
			AttemptClass: string(event.AttemptClass),
			Gate:         event.Gate,
			Verdict:      event.Verdict,
			Target:       event.Target,
			Status:       event.Status,
			Error:        event.Error,
			JournalEvent: &copy,
		})
	}
	return projected
}
