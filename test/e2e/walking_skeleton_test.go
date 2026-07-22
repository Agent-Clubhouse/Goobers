// This file is the V0 walking skeleton (issue #29): it drives a single
// backlog item through the REAL local runner (internal/runner, #17) — real
// journal on disk, real git worktrees, a fake harness (#19's FakeAdapter)
// standing in for the Copilot CLI, and no network access. It replaces the
// earlier version of this file, which proved the same state machine only
// inside Temporal's in-memory test environment — that coverage of the V2
// Temporal adapter now lives in internal/engine's own test suite
// (internal/engine/*_test.go); this file is V0's standing integration gate
// and the seed of the V2 local↔Temporal conformance harness
// (docs/ARCHITECTURE.md §3.3). test/e2e/integration_test.go is untouched: it
// exercises the separate, still-compiling quarantined tier-3 wired path
// (bootstrap/scheduler/engine) against its own fixture.
package e2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

const (
	e2eCommandHelperMode = "GOOBERS_TEST_E2E_COMMAND_HELPER_MODE"
	e2eCommandHelperPath = "GOOBERS_TEST_E2E_COMMAND_HELPER_PATH"
)

func TestE2ECommandHelper(t *testing.T) {
	switch os.Getenv(e2eCommandHelperMode) {
	case "":
		return
	case "success":
		os.Exit(0)
	case "read-file":
		data, err := os.ReadFile(os.Getenv(e2eCommandHelperPath))
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_, _ = os.Stdout.Write(data)
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

func e2eTestCommand(t *testing.T) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	return []string{executable, "-test.run=^TestE2ECommandHelper$"}
}

// --- fixture repo: a local bare git repo, so the walking skeleton needs no
// network access (issue #29 acceptance: "green in CI on a runner with no
// network access"). ---

func newSkeletonFixtureRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runSkeletonGit(t, work, "init", "--initial-branch=main")
	runSkeletonGit(t, work, "config", "user.email", "test@example.com")
	runSkeletonGit(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSkeletonGit(t, work, "add", "README.md")
	runSkeletonGit(t, work, "commit", "-m", "initial")
	runSkeletonGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func runSkeletonGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.autocrlf",
		"GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.safecrlf",
		"GIT_CONFIG_VALUE_1=false",
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
}

// --- fixture workflow: multi-stage with a gate repass (issue #29 scope):
// implement (agentic) -> review (agentic gate; needs-changes repasses to
// implement once, then passes) -> local-ci (deterministic) -> terminal. ---

func skeletonMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement the backlog item",
				// Matches config-examples/gaggles/acme-web/workflows/implementation.yaml's
				// real "implement" task (#27) — a modest retry budget beyond
				// the crash boundary a mid-attempt kill consumes (see
				// TestWalkingSkeletonCrashResume). MaxAttempts=1 (the
				// no-Retry default) would make ANY mid-attempt crash
				// unresumable by design (internal/runner/resume.go
				// fail-closed contract) — a real agentic task budgets for
				// this on purpose.
				Retry: &apiv1.RetryPolicy{MaxAttempts: 2},
				Next:  "review",
			},
			{
				Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "run the local CI-equivalent",
				Run: &apiv1.DeterministicRun{
					Command: e2eTestCommand(t),
					Env:     map[string]string{e2eCommandHelperMode: "success"},
				},
			},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					"pass":          "local-ci",
					"needs-changes": "implement",
					"fail":          workflow.TargetAbort,
				},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "walking-skeleton", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile skeleton machine: %v", err)
	}
	return m
}

// --- the runner wiring: real ShellExecutor (#18) for deterministic tasks,
// real harness.Executor (#19) per goober for agentic tasks/gates, real
// gate.Evaluator (#20) for gates, real worktree.Manager (#16) and journal
// (#8) — only the goober harness's Copilot subprocess is faked. ---

func newSkeletonRunner(t *testing.T, coderAct, reviewerAct func(callNum int) interface{}) (*runner.Runner, string) {
	return newSkeletonRunnerWithSpanCapture(t, coderAct, reviewerAct, nil)
}

type skeletonSpanExporter []sdktrace.SpanExporter

func (e skeletonSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, exporter := range e {
		if err := exporter.ExportSpans(ctx, spans); err != nil {
			return err
		}
	}
	return nil
}

func (e skeletonSpanExporter) Shutdown(ctx context.Context) error {
	for _, exporter := range e {
		if err := exporter.Shutdown(ctx); err != nil {
			return err
		}
	}
	return nil
}

func newSkeletonRunnerWithSpanCapture(
	t *testing.T,
	coderAct, reviewerAct func(callNum int) interface{},
	capture *telemetry.MemoryExporter,
) (*runner.Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newSkeletonFixtureRepo(t)
	var spanExporter sdktrace.SpanExporter = telemetry.NewJournalSpanExporter(runsDir, nil)
	if capture != nil {
		spanExporter = skeletonSpanExporter{capture, spanExporter}
	}
	tel, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "walking-skeleton",
		SpanExporter: spanExporter,
	})
	if err != nil {
		t.Fatalf("new telemetry client: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}

	r, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			injector, ierr := credentials.NewInjector(resolver, nil, reg)
			if ierr != nil {
				return nil, ierr
			}
			return executor.NewShellExecutor(injector, rec)
		},
		NewAgentic: func(gooberName string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
			act := coderAct
			if gooberName == "reviewer" {
				act = reviewerAct
			}
			injector, ierr := credentials.NewInjector(resolver, nil, reg)
			if ierr != nil {
				return nil, ierr
			}
			calls := 0
			adapter := &harness.FakeAdapter{
				Transcript: []byte("fake harness session transcript for " + gooberName + "\n"),
				Act: func(_ context.Context, req harness.RunRequest) error {
					calls++
					payload := act(calls)
					// A coderAct/reviewerAct closure may return dispatchFailure
					// to simulate a dispatch error on this call (distinct from a
					// well-formed ResultEnvelope{Status: failure}) — the only
					// path internal/runner's own retry loop (run.go's runTask)
					// actually retries, tagging the next attempt AttemptPolicy.
					if df, ok := payload.(dispatchFailure); ok {
						return df.err
					}
					// The coder's true deliverable is a committed diff on the run
					// branch, and since #415 an empty diff fast-fails at the
					// review gate before the reviewer runs. So on a successful
					// coder result, commit a change — unique per call, so a
					// repass produces a *different* diff (clearing the #316
					// identical-diff guard too). The reviewer commits nothing.
					if gooberName != "reviewer" {
						if env, ok := payload.(apiv1.ResultEnvelope); ok && env.Status == apiv1.ResultSuccess {
							if werr := os.WriteFile(filepath.Join(req.Workspace, "impl.txt"), []byte(fmt.Sprintf("coder change %d\n", calls)), 0o644); werr != nil {
								return werr
							}
							runSkeletonGit(t, req.Workspace, "add", "-A")
							runSkeletonGit(t, req.Workspace, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-m", fmt.Sprintf("coder impl %d", calls))
						}
					}
					return harness.WriteCompletion(req.Workspace, req.CompletionPath, payload)
				},
			}
			// The runner always constructs executors against the run's own
			// *journal.Run, which implements harness.SpanRecorder alongside
			// runner.ArtifactRecorder (same RecordSpan method) — asserting
			// that here avoids widening runner.ArtifactRecorder itself.
			recorder, ok := rec.(harness.SpanRecorder)
			if !ok {
				return nil, fmt.Errorf("test double %T does not implement harness.SpanRecorder", rec)
			}
			// rec is this run's own *journal.Run, which satisfies Dir()
			// structurally — harness.NewContextResolver pairs that with
			// runsDir (this test's own instance layout) for cross-run
			// resolution (#103/T3), mirroring cmd/goobers/runnerwiring.go's
			// production wiring.
			direr, ok := rec.(interface{ Dir() string })
			if !ok {
				return nil, fmt.Errorf("test double %T does not implement Dir() string", rec)
			}
			contextResolver := harness.NewContextResolver(direr, runsDir)
			// reg is this run's own *journal.RegistryScrubber (#66) — it also
			// implements journal.Scrubber, so chaining it with the pattern
			// net gives the harness executor the SAME per-run scrubbing the
			// runner's own journal uses, rather than a disconnected one.
			registryScrubber, ok := reg.(journal.Scrubber)
			if !ok {
				return nil, fmt.Errorf("test double %T does not implement journal.Scrubber", reg)
			}
			scrubber := journal.Chain(registryScrubber, journal.NewPatternScrubber())
			// rec is this run's own *journal.Run, which also satisfies
			// harness.ArtifactRecorder structurally (same RecordArtifact
			// method as runner.ArtifactRecorder) — passed straight through,
			// same as recorder above.
			return harness.NewExecutor(adapter, injector, recorder, rec, contextResolver, scrubber, "you are the "+gooberName+" fixture goober")
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		Telemetry: tel,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r, runsDir
}

// dispatchFailure, returned by a coderAct/reviewerAct closure in place of a
// resultPayload/verdictPayload, tells newSkeletonRunner's FakeAdapter to fail
// this call's dispatch outright (harness.RunRequest.Act returning err) rather
// than write a completion file — the shape a real infrastructure hiccup takes,
// and the only one internal/runner's task-level retry loop actually retries
// (run.go's runTask: a dispatch error is retried up to the task's declared
// Retry.MaxAttempts, tagging the next attempt AttemptPolicy; a well-formed
// ResultEnvelope{Status: failure} is NOT retried — it's a completed, failed
// attempt). Used to exercise a genuine stage-level policy retry in
// TestWalkingSkeletonLocalRunnerDeterministicJournal's conformance comparison.
type dispatchFailure struct{ err error }

func resultPayload(status apiv1.ResultStatus, summary string) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{
		Status:  status,
		Summary: summary,
		// Outputs are scalar-only by contract (api/v1alpha1.ResultEnvelope,
		// #10) — a changed-files artifact would be a real ArtifactPointer in
		// a real run; this fixture only needs a small scalar to prove the
		// envelope round-trips.
		Outputs: map[string]interface{}{"changedFileCount": 1},
	}
}

func verdictPayload(decision apiv1.VerdictDecision, rationale string) apiv1.Verdict {
	return apiv1.Verdict{Decision: decision, Rationale: rationale}
}

// skeletonStartInput builds the StartInput for one run of the skeleton
// machine, claiming a single fixture backlog item.
func skeletonStartInput(runID string, machine *workflow.Machine) runner.StartInput {
	return runner.StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerItem},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item: &apiv1.BacklogItem{
			ID: "101", Provider: apiv1.ProviderGitHub,
			Title: "Add walking skeleton smoke path", Labels: []string{"goobers:ready"},
			URL: "https://github.com/acme/web/issues/101",
		},
	}
}

// TestWalkingSkeletonLocalRunnerCompletesWithRepass is the headline walking
// skeleton (issue #29): a single item runs through the real local runner
// across a multi-stage workflow with a gate repass (reviewer requests
// changes once, then approves), asserting on the JOURNAL — not on runner
// internals — per the acceptance criteria: event sequence, digests verify,
// state.json terminal, artifacts resolvable, spans present.
func TestWalkingSkeletonLocalRunnerCompletesWithRepass(t *testing.T) {
	machine := skeletonMachine(t)
	coderAct := func(int) interface{} { return resultPayload(apiv1.ResultSuccess, "implemented") }
	reviewerAct := func(call int) interface{} {
		if call == 1 {
			return verdictPayload(apiv1.VerdictNeedsChanges, "add a test for the new branch")
		}
		return verdictPayload(apiv1.VerdictPass, "looks good")
	}
	liveSpans := telemetry.NewMemoryExporter()
	r, runsDir := newSkeletonRunnerWithSpanCapture(t, coderAct, reviewerAct, liveSpans)

	runID, err := telemetry.NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	res, err := r.Start(context.Background(), skeletonStartInput(runID, machine))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if res.FinalState != "local-ci" {
		t.Fatalf("finalState = %q, want local-ci", res.FinalState)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}

	// Identity: version-pinned (WF-016) to the exact compiled digest.
	id, err := rd.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.WorkflowDigest != machine.Digest() {
		t.Errorf("run.yaml workflowDigest = %q, want %q", id.WorkflowDigest, machine.Digest())
	}

	// Event sequence: implement runs, review requests changes (repass),
	// implement runs again, review passes, local-ci runs, run finishes.
	// Artifacts/spans interleave per stage but every stage/gate transition is
	// present and in order.
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var types []journal.EventType
	sawSpan, sawArtifact := false, false
	for _, e := range events {
		types = append(types, e.Type)
		if e.Type == journal.EventSpanRecorded {
			sawSpan = true
		}
		if e.Type == journal.EventArtifactRecorded {
			sawArtifact = true
		}
	}
	if !sawSpan {
		t.Error("expected at least one span.recorded event (the fake harness transcript)")
	}
	if !sawArtifact {
		t.Error("expected at least one artifact.recorded event")
	}
	gateEvals := countEventType(types, journal.EventGateEvaluated)
	if gateEvals != 2 {
		t.Errorf("gate.evaluated count = %d, want 2 (needs-changes then pass)", gateEvals)
	}
	gateStarts := countEventType(types, journal.EventGateStarted)
	if gateStarts != 2 {
		t.Errorf("gate.started count = %d, want 2 (one durable marker per evaluation)", gateStarts)
	}
	for _, e := range events {
		if e.Type == journal.EventGateStarted && e.IsConformanceNormative() {
			t.Errorf("gate.started must be excluded from conformance: %+v", e)
		}
	}
	stageStarts := countEventType(types, journal.EventStageStarted)
	if stageStarts != 3 {
		t.Errorf("stage.started count = %d, want 3 (implement x2, local-ci x1)", stageStarts)
	}
	if types[0] != journal.EventRunStarted || types[len(types)-1] != journal.EventRunFinished {
		t.Errorf("event sequence must start with run.started and end with run.finished, got %v", types)
	}

	// state.json: terminal, no pending machine state.
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseCompleted || st.MachineState != "" {
		t.Fatalf("state.json = %+v, want completed with empty machineState", st)
	}

	// Every recorded artifact/span is resolvable and digest-verified — the
	// same round-trip a downstream stage or the portal would do.
	for _, e := range events {
		switch e.Type {
		case journal.EventArtifactRecorded:
			if _, err := rd.ArtifactBytes(*e.Ref); err != nil {
				t.Errorf("ArtifactBytes(%+v): %v", e.Ref, err)
			}
		case journal.EventSpanRecorded:
			if _, err := rd.SpanBytes(*e.Ref); err != nil {
				t.Errorf("SpanBytes(%+v): %v", e.Ref, err)
			}
		}
	}
	assertSkeletonSpanAttributes(t, runsDir, runID, machine.Digest())
	assertSkeletonOTLPMatchesLiveSpans(t, runsDir, runID, liveSpans.Spans())
}

func assertSkeletonSpanAttributes(t *testing.T, runsDir, runID, workflowDigest string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(runsDir, runID, "spans", "spans.jsonl"))
	if err != nil {
		t.Fatalf("read OTel spans: %v", err)
	}

	counts := map[string]int{}
	gateResults := map[string]int{}
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var span telemetry.SpanRecord
		if err := json.Unmarshal(line, &span); err != nil {
			t.Fatalf("decode OTel span: %v", err)
		}
		counts[span.Kind]++
		for key, want := range map[string]string{
			telemetry.AttrRunID:           runID,
			telemetry.AttrGaggle:          "acme-web",
			telemetry.AttrWorkflow:        "walking-skeleton",
			telemetry.AttrWorkflowVersion: "1",
			telemetry.AttrWorkflowDigest:  workflowDigest,
			telemetry.AttrItemID:          "101",
			telemetry.AttrItemURL:         "https://github.com/acme/web/issues/101",
		} {
			if got := span.Attributes[key]; got != want {
				t.Errorf("span %q attribute %s = %q, want %q", span.Name, key, got, want)
			}
		}

		switch outcome := span.Attributes[telemetry.AttrOutcome]; outcome {
		case telemetry.OutcomeSuccess, telemetry.OutcomeFailure, telemetry.OutcomeBlocked:
		default:
			t.Errorf("span %q %s = %q, want success, failure, or blocked", span.Name, telemetry.AttrOutcome, outcome)
		}
		for _, legacy := range []string{"gaggle", "workflowId", "runId", "goobers.span.kind", "goobers.business_status"} {
			if _, ok := span.Attributes[legacy]; ok {
				t.Errorf("span %q carries legacy attribute %q", span.Name, legacy)
			}
		}

		switch span.Kind {
		case telemetry.SpanKindTask:
			if span.Attributes[telemetry.AttrStage] == "" ||
				span.Attributes[telemetry.AttrStageType] == "" ||
				span.Attributes[telemetry.AttrAttemptNumber] != "1" {
				t.Errorf("task span %q has incomplete stage-attempt attributes: %+v", span.Name, span.Attributes)
			}
		case telemetry.SpanKindGate:
			decision := span.Attributes[telemetry.AttrGateDecision]
			repass := span.Attributes[telemetry.AttrGateRepassNumber]
			if span.Attributes[telemetry.AttrStage] != "review" ||
				span.Attributes[telemetry.AttrStageType] != telemetry.StageTypeGate ||
				decision == "" || repass == "" {
				t.Errorf("gate span %q has incomplete gate attributes: %+v", span.Name, span.Attributes)
			}
			gateResults[decision+"/"+repass]++
		}
	}
	if counts[telemetry.SpanKindRun] != 1 || counts[telemetry.SpanKindTask] != 3 || counts[telemetry.SpanKindGate] != 2 {
		t.Fatalf("OTel span kinds = %v, want run=1 task=3 gate=2", counts)
	}
	if gateResults["needs-changes/1"] != 1 || gateResults["pass/0"] != 1 {
		t.Fatalf("gate span decisions/repasses = %v, want needs-changes/1 and pass/0", gateResults)
	}
}

type skeletonOTLPSpan struct {
	resource *tracepb.ResourceSpans
	scope    *tracepb.ScopeSpans
	span     *tracepb.Span
}

func assertSkeletonOTLPMatchesLiveSpans(t *testing.T, runsDir, runID string, live []sdktrace.ReadOnlySpan) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(runsDir, runID, "spans", "otlp.jsonl"))
	if err != nil {
		t.Fatalf("read OTLP journal spans: %v", err)
	}

	journalSpans := make(map[string]skeletonOTLPSpan)
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		traces, err := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(line)
		if err != nil {
			t.Fatalf("decode OTLP/JSON: %v", err)
		}
		wire, err := (&ptrace.ProtoMarshaler{}).MarshalTraces(traces)
		if err != nil {
			t.Fatalf("encode OTLP protobuf: %v", err)
		}
		request := new(collectortracepb.ExportTraceServiceRequest)
		if err := proto.Unmarshal(wire, request); err != nil {
			t.Fatalf("decode OTLP protobuf: %v", err)
		}
		for _, resourceSpans := range request.ResourceSpans {
			for _, scopeSpans := range resourceSpans.ScopeSpans {
				for _, span := range scopeSpans.Spans {
					journalSpans[hex.EncodeToString(span.SpanId)] = skeletonOTLPSpan{
						resource: resourceSpans,
						scope:    scopeSpans,
						span:     span,
					}
				}
			}
		}
	}
	if len(journalSpans) != len(live) {
		t.Fatalf("OTLP journal spans = %d, live exported spans = %d", len(journalSpans), len(live))
	}

	for _, want := range live {
		spanID := want.SpanContext().SpanID().String()
		got, ok := journalSpans[spanID]
		if !ok {
			t.Errorf("live span %s (%q) missing from OTLP journal", spanID, want.Name())
			continue
		}
		wantParentSpanID := ""
		if want.Parent().SpanID().IsValid() {
			wantParentSpanID = want.Parent().SpanID().String()
		}
		if hex.EncodeToString(got.span.TraceId) != want.SpanContext().TraceID().String() ||
			hex.EncodeToString(got.span.SpanId) != spanID ||
			hex.EncodeToString(got.span.ParentSpanId) != wantParentSpanID {
			t.Errorf("span %q identity differs: %+v", want.Name(), got.span)
		}
		if got.span.TraceState != want.SpanContext().TraceState().String() ||
			got.span.Name != want.Name() ||
			got.span.Kind != skeletonOTLPSpanKind(want.SpanKind()) ||
			got.span.StartTimeUnixNano != uint64(want.StartTime().UnixNano()) ||
			got.span.EndTimeUnixNano != uint64(want.EndTime().UnixNano()) {
			t.Errorf("span %q core fields differ: %+v", want.Name(), got.span)
		}
		if got.span.Status.Code != skeletonOTLPStatus(want.Status().Code) ||
			got.span.Status.Message != want.Status().Description {
			t.Errorf("span %q status differs: %+v vs %+v", want.Name(), got.span.Status, want.Status())
		}
		assertSkeletonOTLPAttributes(t, want.Name()+" span", want.Attributes(), got.span.Attributes)
		if got.span.DroppedAttributesCount != uint32(want.DroppedAttributes()) ||
			got.span.DroppedEventsCount != uint32(want.DroppedEvents()) ||
			got.span.DroppedLinksCount != uint32(want.DroppedLinks()) {
			t.Errorf("span %q dropped counts differ", want.Name())
		}

		resource := want.Resource()
		if got.resource.SchemaUrl != resource.SchemaURL() {
			t.Errorf("span %q resource schema = %q, want %q", want.Name(), got.resource.SchemaUrl, resource.SchemaURL())
		}
		assertSkeletonOTLPAttributes(t, want.Name()+" resource", resource.Attributes(), got.resource.Resource.Attributes)

		scope := want.InstrumentationScope()
		if got.scope.SchemaUrl != scope.SchemaURL || got.scope.Scope.Name != scope.Name || got.scope.Scope.Version != scope.Version {
			t.Errorf("span %q scope differs: %+v vs %+v", want.Name(), got.scope, scope)
		}
		assertSkeletonOTLPAttributes(t, want.Name()+" scope", scope.Attributes.ToSlice(), got.scope.Scope.Attributes)

		if len(got.span.Events) != len(want.Events()) {
			t.Errorf("span %q event count = %d, want %d", want.Name(), len(got.span.Events), len(want.Events()))
		} else {
			for i, event := range want.Events() {
				gotEvent := got.span.Events[i]
				if gotEvent.Name != event.Name ||
					gotEvent.TimeUnixNano != uint64(event.Time.UnixNano()) ||
					gotEvent.DroppedAttributesCount != uint32(event.DroppedAttributeCount) {
					t.Errorf("span %q event %d differs: %+v vs %+v", want.Name(), i, gotEvent, event)
				}
				assertSkeletonOTLPAttributes(t, want.Name()+" event", event.Attributes, gotEvent.Attributes)
			}
		}
		if len(got.span.Links) != len(want.Links()) {
			t.Errorf("span %q link count = %d, want %d", want.Name(), len(got.span.Links), len(want.Links()))
		} else {
			for i, link := range want.Links() {
				gotLink := got.span.Links[i]
				if hex.EncodeToString(gotLink.TraceId) != link.SpanContext.TraceID().String() ||
					hex.EncodeToString(gotLink.SpanId) != link.SpanContext.SpanID().String() ||
					gotLink.TraceState != link.SpanContext.TraceState().String() ||
					gotLink.DroppedAttributesCount != uint32(link.DroppedAttributeCount) {
					t.Errorf("span %q link %d differs: %+v vs %+v", want.Name(), i, gotLink, link)
				}
				assertSkeletonOTLPAttributes(t, want.Name()+" link", link.Attributes, gotLink.Attributes)
			}
		}
	}
}

func assertSkeletonOTLPAttributes(t *testing.T, name string, want []attribute.KeyValue, got []*commonpb.KeyValue) {
	t.Helper()
	gotByKey := make(map[string]*commonpb.AnyValue, len(got))
	for _, attr := range got {
		gotByKey[attr.Key] = attr.Value
	}
	if len(gotByKey) != len(want) {
		t.Errorf("%s OTLP attributes = %d, live attributes = %d", name, len(gotByKey), len(want))
	}
	for _, attr := range want {
		value, ok := gotByKey[string(attr.Key)]
		if !ok {
			t.Errorf("%s attribute %q missing from OTLP journal", name, attr.Key)
			continue
		}
		gotValue, gotType := skeletonOTLPAttributeValue(value)
		wantValue := attr.Value.AsInterface()
		if gotType != attr.Value.Type() || !reflect.DeepEqual(gotValue, wantValue) {
			t.Errorf("%s attribute %q = %T(%v), want %v(%v)", name, attr.Key, gotValue, gotValue, attr.Value.Type(), wantValue)
		}
	}
}

func skeletonOTLPAttributeValue(value *commonpb.AnyValue) (any, attribute.Type) {
	switch value := value.Value.(type) {
	case *commonpb.AnyValue_BoolValue:
		return value.BoolValue, attribute.BOOL
	case *commonpb.AnyValue_IntValue:
		return value.IntValue, attribute.INT64
	case *commonpb.AnyValue_DoubleValue:
		return value.DoubleValue, attribute.FLOAT64
	case *commonpb.AnyValue_StringValue:
		return value.StringValue, attribute.STRING
	case *commonpb.AnyValue_ArrayValue:
		if len(value.ArrayValue.Values) == 0 {
			return []string{}, attribute.STRINGSLICE
		}
		first := value.ArrayValue.Values[0]
		switch first.Value.(type) {
		case *commonpb.AnyValue_BoolValue:
			out := make([]bool, len(value.ArrayValue.Values))
			for i, item := range value.ArrayValue.Values {
				out[i] = item.GetBoolValue()
			}
			return out, attribute.BOOLSLICE
		case *commonpb.AnyValue_IntValue:
			out := make([]int64, len(value.ArrayValue.Values))
			for i, item := range value.ArrayValue.Values {
				out[i] = item.GetIntValue()
			}
			return out, attribute.INT64SLICE
		case *commonpb.AnyValue_DoubleValue:
			out := make([]float64, len(value.ArrayValue.Values))
			for i, item := range value.ArrayValue.Values {
				out[i] = item.GetDoubleValue()
			}
			return out, attribute.FLOAT64SLICE
		default:
			out := make([]string, len(value.ArrayValue.Values))
			for i, item := range value.ArrayValue.Values {
				out[i] = item.GetStringValue()
			}
			return out, attribute.STRINGSLICE
		}
	default:
		return nil, attribute.INVALID
	}
}

func skeletonOTLPSpanKind(kind trace.SpanKind) tracepb.Span_SpanKind {
	switch kind {
	case trace.SpanKindInternal:
		return tracepb.Span_SPAN_KIND_INTERNAL
	case trace.SpanKindServer:
		return tracepb.Span_SPAN_KIND_SERVER
	case trace.SpanKindClient:
		return tracepb.Span_SPAN_KIND_CLIENT
	case trace.SpanKindProducer:
		return tracepb.Span_SPAN_KIND_PRODUCER
	case trace.SpanKindConsumer:
		return tracepb.Span_SPAN_KIND_CONSUMER
	default:
		return tracepb.Span_SPAN_KIND_UNSPECIFIED
	}
}

func skeletonOTLPStatus(code codes.Code) tracepb.Status_StatusCode {
	switch code {
	case codes.Ok:
		return tracepb.Status_STATUS_CODE_OK
	case codes.Error:
		return tracepb.Status_STATUS_CODE_ERROR
	default:
		return tracepb.Status_STATUS_CODE_UNSET
	}
}

// TestWalkingSkeletonLocalRunnerGateFailAborts exercises the "fail" branch:
// review rejects outright and the run ends aborted without a repass.
func TestWalkingSkeletonLocalRunnerGateFailAborts(t *testing.T) {
	machine := skeletonMachine(t)
	coderAct := func(int) interface{} { return resultPayload(apiv1.ResultSuccess, "implemented") }
	reviewerAct := func(int) interface{} { return verdictPayload(apiv1.VerdictFail, "wrong approach entirely") }
	r, _ := newSkeletonRunner(t, coderAct, reviewerAct)

	res, err := r.Start(context.Background(), skeletonStartInput("run-skeleton-2", machine))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted", res.Phase)
	}
}

// TestWalkingSkeletonLocalRunnerDeterministicJournal is the conformance seed
// (docs/ARCHITECTURE.md §3.3): two independent runs of the identical
// definition, with the SAME pinned RunID (§3.3's conformance comparison is
// apples-to-apples over identical caller-supplied inputs, not two arbitrary
// runs — RunID is caller-supplied, never runner-generated, so a real
// conformance harness pins it identically across the runners it compares)
// and a deterministic fake harness (no live LLM variance), produce
// digest-identical event sequences over the FULL conformance-normative field
// set (journal.ConformanceView — issue #141: Schema, Branch, Attempt,
// AttemptClass, and ExternalRef included alongside the fields the seed
// already compared, not just a test-local subset) — timestamps, durations,
// infra-retry attempts, and namespaced runner.* annotations excluded per
// §3.3 and journal.Event.IsConformanceNormative/doc comments. (Artifact/span
// Name legitimately embeds the RunID via env.TaskID, per
// internal/executor.ShellExecutor — same RunID is what makes Name comparable
// at all, not something to strip from the comparison.) The comparison
// includes one genuine stage-level policy retry (coderAct's first dispatch
// fails, tagging the retried attempt AttemptClass=policy per run.go's
// runTask — see dispatchFailure) — #141 flagged that no prior compared run
// exercised this. journal.MonotonicSeq additionally asserts each run's own
// seq values are gap-free, per #141. This is the seed the V2 local↔Temporal
// conformance harness (ARCHITECTURE §3.3, issue #40) extends to diff the two
// runners' journals against shared fixtures, through this same
// ConformanceView — not a bespoke comparator.
func TestWalkingSkeletonLocalRunnerDeterministicJournal(t *testing.T) {
	machine := skeletonMachine(t)
	coderAct := func(call int) interface{} {
		if call == 1 {
			// Fails this dispatch outright (not a well-formed ResultEnvelope
			// failure) so runTask's own retry loop retries it, tagging
			// attempt 2 AttemptClass=policy — deterministic across both
			// canon() runs since `call` counts from a fresh 0 each run.
			return dispatchFailure{err: fmt.Errorf("transient dispatch failure")}
		}
		return resultPayload(apiv1.ResultSuccess, "implemented")
	}
	reviewerAct := func(call int) interface{} {
		if call == 1 {
			return verdictPayload(apiv1.VerdictNeedsChanges, "add a test for the new branch")
		}
		return verdictPayload(apiv1.VerdictPass, "looks good")
	}

	canon := func(runID string) []journal.NormativeEvent {
		r, runsDir := newSkeletonRunner(t, coderAct, reviewerAct)
		res, err := r.Start(context.Background(), skeletonStartInput(runID, machine))
		if err != nil {
			t.Fatalf("Start(%s): %v", runID, err)
		}
		if res.Phase != journal.PhaseCompleted {
			t.Fatalf("Start(%s) phase = %q, want completed", runID, res.Phase)
		}
		rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
		if err != nil {
			t.Fatalf("OpenRead(%s): %v", runID, err)
		}
		events, err := rd.Events()
		if err != nil {
			t.Fatalf("Events(%s): %v", runID, err)
		}
		if err := journal.MonotonicSeq(events); err != nil {
			t.Fatalf("MonotonicSeq(%s): %v", runID, err)
		}
		var sawPolicyRetry bool
		for _, e := range events {
			if e.Stage == "implement" && e.AttemptClass == journal.AttemptPolicy {
				sawPolicyRetry = true
			}
		}
		if !sawPolicyRetry {
			t.Fatalf("Start(%s): expected a policy-retried implement attempt, saw none", runID)
		}
		spans := readSkeletonSpanRecords(t, runsDir, runID)
		var sawPolicyRetrySpan bool
		for _, span := range spans {
			if span.Attributes[telemetry.AttrStage] == "implement" &&
				span.Attributes[telemetry.AttrAttemptNumber] == "2" &&
				span.Attributes[telemetry.AttrAttemptKind] == telemetry.AttemptKindPolicy {
				sawPolicyRetrySpan = true
			}
		}
		if !sawPolicyRetrySpan {
			t.Fatalf("Start(%s): expected a policy-retried implement span, got %+v", runID, spans)
		}
		return journal.ConformanceView(events)
	}

	// Same RunID for both — newSkeletonRunner gives each call its own fresh
	// instance root/runsDir, so there's no on-disk collision, and pinning the
	// identity is what makes RunID-embedding fields (artifact/span Name)
	// comparable across the two runs.
	const pinnedRunID = "11111111111111111111111111111111"
	a := canon(pinnedRunID)
	b := canon(pinnedRunID)

	if len(a) != len(b) {
		t.Fatalf("normative event count differs: %d vs %d\na=%v\nb=%v", len(a), len(b), a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("normative event %d differs:\n a: %s\n b: %s", i, a[i], b[i])
		}
	}
}

func readSkeletonSpanRecords(t *testing.T, runsDir, runID string) []telemetry.SpanRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(runsDir, runID, "spans", "spans.jsonl"))
	if err != nil {
		t.Fatalf("read OTel spans for %s: %v", runID, err)
	}
	var spans []telemetry.SpanRecord
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var span telemetry.SpanRecord
		if err := json.Unmarshal(line, &span); err != nil {
			t.Fatalf("decode OTel span for %s: %v", runID, err)
		}
		spans = append(spans, span)
	}
	return spans
}

// simulateSkeletonCrashMidImplement hand-builds a run journal exactly to the
// point a real Start would have reached had the process died right after
// dispatching "implement"'s first attempt: run.started, then
// stage.started(implement, attempt 1), with NO matching stage.finished — the
// crash signature Resume must detect (internal/runner/resume.go's
// interruptedAttempt). Mirrors internal/runner's own
// simulateCrashMidAttempt (run_test.go) exactly, but through this file's
// real StartInput shape (including the item snapshot input a genuine Start
// would have journaled) so Resume reconstructs the same run a real
// crash-mid-attempt would have left on disk. A clean journal.Create/Close
// (no torn write) is sufficient — torn-write repair is internal/journal's
// own, already-tested concern; this is about the runner's interpretation of
// "started with no finished".
func simulateSkeletonCrashMidImplement(t *testing.T, runsDir string, machine *workflow.Machine, in runner.StartInput) {
	t.Helper()
	inputs := map[string][]byte{}
	if in.Item != nil {
		b, err := json.Marshal(in.Item)
		if err != nil {
			t.Fatalf("simulateSkeletonCrashMidImplement: marshal item snapshot: %v", err)
		}
		inputs["item"] = b
	}
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: in.RunID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: in.Gaggle, Trigger: in.Trigger,
	}, inputs)
	if err != nil {
		t.Fatalf("simulateSkeletonCrashMidImplement: journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("simulateSkeletonCrashMidImplement: append stage.started: %v", err)
	}
	if _, err := jr.RecordStageArtifact(
		"implement", 1, "", journal.ContextManifestArtifactName("implement", 1), []byte(`{"contextPointers":[]}`),
	); err != nil {
		t.Fatalf("simulateSkeletonCrashMidImplement: record context manifest: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("simulateSkeletonCrashMidImplement: close: %v", err)
	}
}

// TestWalkingSkeletonCrashResume is #29's crash/resume acceptance scenario,
// now that internal/runner Deliverable B (#17) has landed: kill the runner
// mid-attempt on "implement", restart against a fresh Runner (a real
// process restart constructs a new one), and Resume from the journal's
// checkpointed state. Per resume.go's contract the interrupted attempt is
// journaled as a terminal infra-tagged failure — never silently re-run
// (§17) — before the runner continues the SAME attempt count against
// "implement"'s own retry budget, then the run rejoins the ordinary
// walking-skeleton machine (review passes, local-ci runs, completes) exactly
// as internal/runner's own TestRunnerResumeRetriesInterruptedAttempt proves
// at the runner-unit level — this test proves the same contract holds
// end-to-end through the real multi-stage/gate skeleton, asserting on the
// journal per this file's convention.
func TestWalkingSkeletonCrashResume(t *testing.T) {
	machine := skeletonMachine(t)
	coderAct := func(int) interface{} { return resultPayload(apiv1.ResultSuccess, "implemented") }
	reviewerAct := func(int) interface{} { return verdictPayload(apiv1.VerdictPass, "looks good") }
	r, runsDir := newSkeletonRunner(t, coderAct, reviewerAct)

	in := skeletonStartInput("run-skeleton-crash", machine)
	simulateSkeletonCrashMidImplement(t, runsDir, machine, in)

	res, err := r.Resume(context.Background(), runner.ResumeInput{
		RunID:   in.RunID,
		Machine: machine,
		RepoRef: in.RepoRef,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, in.RunID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	// "implement" saw exactly one failed attempt (the crash, journaled by
	// Resume) and one success — the acceptance scenario's own words.
	var implementEvents []journal.Event
	var types []journal.EventType
	for _, e := range events {
		types = append(types, e.Type)
		if e.Stage == "implement" {
			implementEvents = append(implementEvents, e)
		}
	}
	wantTypes := []journal.EventType{
		journal.EventStageStarted, // attempt 1, pre-crash (hand-built above)
		journal.EventArtifactRecorded,
		journal.EventStageFinished, // attempt 1, infra, journaled by Resume
		journal.EventStageStarted,  // attempt 2, the crash-driven continuation
		journal.EventArtifactRecorded,
		journal.EventStageFinished, // attempt 2, the crash-driven continuation, success
	}
	if len(implementEvents) != len(wantTypes) {
		t.Fatalf("implement-stage events = %d, want %d: %+v", len(implementEvents), len(wantTypes), implementEvents)
	}
	for i, e := range implementEvents {
		if e.Type != wantTypes[i] {
			t.Errorf("event[%d].Type = %q, want %q", i, e.Type, wantTypes[i])
		}
	}
	if implementEvents[2].Attempt != 1 || implementEvents[2].AttemptClass != journal.AttemptInfra || implementEvents[2].Status != string(apiv1.ResultFailure) {
		t.Errorf("interrupted-attempt event = %+v, want attempt=1 class=infra status=failure", implementEvents[2])
	}
	// #111: the continuation dispatched right after the interrupted attempt
	// is driven by the crash, not Task.Retry — it must be tagged "infra",
	// not "policy" (which would wrongly make it conformance-normative,
	// §3.3, adding a phantom retry event a crash-free run never produces).
	if implementEvents[3].Attempt != 2 || implementEvents[3].AttemptClass != journal.AttemptInfra {
		t.Errorf("resumed-attempt stage.started = %+v, want attempt=2 class=infra", implementEvents[3])
	}
	if implementEvents[4].Attempt != 2 || implementEvents[4].AttemptClass != journal.AttemptInfra || implementEvents[4].Type != journal.EventArtifactRecorded {
		t.Errorf("resumed-attempt context artifact = %+v, want attempt=2 class=infra artifact.recorded", implementEvents[4])
	}
	if implementEvents[5].Attempt != 2 || implementEvents[5].AttemptClass != journal.AttemptInfra || implementEvents[5].Status != string(apiv1.ResultSuccess) {
		t.Errorf("resumed-attempt stage.finished = %+v, want attempt=2 class=infra status=success", implementEvents[5])
	}
	// Every post-crash "implement" event is excluded from conformance
	// (§3.3) — confirm IsConformanceNormative agrees for all four, same as
	// internal/runner's own crash-resume test.
	for i := 2; i <= 5; i++ {
		if implementEvents[i].IsConformanceNormative() {
			t.Errorf("event[%d] = %+v must be excluded from conformance (§3.3)", i, implementEvents[i])
		}
	}

	// Resume doesn't just recover the interrupted stage in isolation — the
	// run rejoins the SAME multi-stage/gate machine the rest of #29
	// exercises: review evaluates once (pass, no repass in this scenario)
	// and local-ci runs before the run finishes completed.
	if gateEvals := countEventType(types, journal.EventGateEvaluated); gateEvals != 1 {
		t.Errorf("gate.evaluated count = %d, want 1 (single pass)", gateEvals)
	}
	if n := countEventType(types, journal.EventStageStarted); n != 3 {
		t.Errorf("stage.started count = %d, want 3 (implement x2 incl. crashed attempt, local-ci x1)", n)
	}

	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseCompleted || st.MachineState != "" {
		t.Fatalf("state.json = %+v, want completed with empty machineState", st)
	}
}

// countEventType counts occurrences of typ in types.
func countEventType(types []journal.EventType, typ journal.EventType) int {
	n := 0
	for _, t := range types {
		if t == typ {
			n++
		}
	}
	return n
}
