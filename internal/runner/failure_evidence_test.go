package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
)

// The failed-task evidence fixture (#5221) pins the retention, routing and
// context-delivery matrix documented in
// docs/guides/failed-task-evidence.md. The producer is this test binary
// re-entered through TestFailedTaskEvidenceHelper, so the fixture needs no
// shell, LLM, provider or network and runs identically on every platform.

const failureEvidenceModeEnv = "GOOBERS_INPUT_EVIDENCEMODE"

// TestFailedTaskEvidenceHelper is the producer process for the fixture. It is
// inert unless the fixture launches it as a stage with an evidence mode.
func TestFailedTaskEvidenceHelper(t *testing.T) {
	mode := os.Getenv(failureEvidenceModeEnv)
	if mode == "" {
		return
	}
	round := 1
	if mode == "round" {
		counter := os.Getenv("GOOBERS_INPUT_COUNTERFILE")
		prior, _ := os.ReadFile(counter)
		round = len(prior) + 1
		if err := os.WriteFile(counter, append(prior, 'x'), 0o600); err != nil {
			os.Exit(93)
		}
	}
	switch mode {
	case "fail", "timeout", "round":
		writeFailureEvidence(round)
	case "observe":
		os.Exit(0)
	default:
		os.Exit(99)
	}
	_, _ = fmt.Fprintf(os.Stdout, "producer stdout round %d\n", round)
	if mode == "timeout" {
		time.Sleep(time.Minute)
		os.Exit(4)
	}
	_, _ = fmt.Fprintf(os.Stderr, "producer stderr round %d\n", round)
	os.Exit(3)
}

func writeFailureEvidence(round int) {
	if err := os.MkdirAll("diag", 0o755); err != nil {
		os.Exit(90)
	}
	if err := os.WriteFile(filepath.Join("diag", "trace.txt"), []byte(fmt.Sprintf("round %d trace\n", round)), 0o600); err != nil {
		os.Exit(91)
	}
	result := fmt.Sprintf(`{"diagnosis":"bad-input","round":%d}`, round)
	if err := os.WriteFile(os.Getenv("GOOBERS_INPUT_RESULTFILE"), []byte(result), 0o600); err != nil {
		os.Exit(92)
	}
}

func failureEvidenceHelperCommand(t *testing.T) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe, "-test.run=^TestFailedTaskEvidenceHelper$"}
}

func failureEvidenceProducer(t *testing.T, mode string) apiv1.Task {
	t.Helper()
	return apiv1.Task{
		Run:    &apiv1.DeterministicRun{Command: failureEvidenceHelperCommand(t)},
		Inputs: map[string]string{"evidenceMode": mode, "resultFile": "result.json"},
		Outbox: []string{"diag"},
	}
}

type failureEvidenceRun struct {
	runDir   string
	phase    journal.RunPhase
	startErr error
	events   []journal.Event
}

// runFailureEvidenceFixture runs produce -> classify -> collect ->
// after-collect. The default classify gate routes every failure class to the
// diagnostic consumer, which narrows its context to the producer.
func runFailureEvidenceFixture(t *testing.T, produce, collect apiv1.Task, classify apiv1.Gate) failureEvidenceRun {
	t.Helper()
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, reg SecretRegistrar) (invoke.Deterministic, error) {
			injector, err := credentials.NewInjector(resolver, nil, reg)
			if err != nil {
				return nil, err
			}
			return executor.NewShellExecutor(injector, rec)
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	produce.Name, produce.Type, produce.Goal, produce.Next = "produce", apiv1.TaskDeterministic, "produce evidence", "classify"
	collect.Name, collect.Type, collect.Goal = "collect", apiv1.TaskDeterministic, "collect failure evidence"
	if collect.Run == nil {
		collect.Run = &apiv1.DeterministicRun{Command: failureEvidenceHelperCommand(t)}
	}
	if collect.Inputs == nil {
		collect.Inputs = map[string]string{"evidenceMode": "observe"}
	}
	collect.ContextFrom = []string{"produce"}
	collect.Next = "after-collect"
	if classify.Automated == nil {
		classify = apiv1.Gate{
			Automated: &apiv1.AutomatedGate{Check: "failure-class"},
			Branches: map[string]string{
				gate.OutcomePass:  workflow.TerminalComplete,
				gate.OutcomeFail:  "collect",
				gate.OutcomeInfra: "collect",
			},
		}
	}
	classify.Name, classify.Evaluator = "classify", apiv1.EvaluatorAutomated
	machine, err := workflow.Compile(workflow.Definition{
		Name: "failure-evidence", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web",
			Start:  "produce",
			Tasks:  []apiv1.Task{produce, collect},
			Gates: []apiv1.Gate{classify, {
				Name: "after-collect", Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches: map[string]string{
					gate.OutcomePass: workflow.TerminalComplete,
					gate.OutcomeFail: workflow.TargetAbort,
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	runID, err := telemetry.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	result, startErr := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	runDir := filepath.Join(runsDir, runID)
	return failureEvidenceRun{runDir: runDir, phase: result.Phase, startErr: startErr, events: readJournalEvents(t, runDir)}
}

// evidenceState is the documented classification of one journal-bound
// evidence item.
type evidenceState string

const (
	evidenceVerified  evidenceState = "verified"
	evidenceAbsent    evidenceState = "absent"
	evidenceMalformed evidenceState = "malformed"
)

// producerOccurrence is one exact producer execution, selected the way
// docs/guides/failed-task-evidence.md prescribes.
type producerOccurrence struct {
	startedSeq, finishedSeq uint64
	attempt                 int
	finished                journal.Event
	// recorded maps the journal artifact name (stdout.log, stderr.log, result,
	// outbox/<path>) to the ref recorded inside this occurrence's window.
	recorded map[string]journal.Ref
}

// exactProducerOccurrence implements the documented selection: the
// triggering occurrence is the last producer stage.finished before the
// consumer's last stage.started on the same branch. Attempt numbers are
// never used to identify it; gate repasses legitimately reuse attempt 1.
func exactProducerOccurrence(t *testing.T, events []journal.Event, producer, consumer string) producerOccurrence {
	t.Helper()
	var consumerStarted uint64
	for _, ev := range events {
		if ev.Branch == 0 && ev.Type == journal.EventStageStarted && ev.Stage == consumer {
			consumerStarted = ev.Seq
		}
	}
	if consumerStarted == 0 {
		t.Fatalf("consumer %q never started", consumer)
	}
	var occ producerOccurrence
	for _, ev := range events {
		if ev.Branch != 0 || ev.Stage != producer || ev.Seq >= consumerStarted {
			continue
		}
		switch ev.Type {
		case journal.EventStageStarted:
			occ = producerOccurrence{startedSeq: ev.Seq, attempt: ev.Attempt}
		case journal.EventStageFinished:
			occ.finishedSeq, occ.finished = ev.Seq, ev
		}
	}
	if occ.finishedSeq == 0 {
		t.Fatalf("no finished %q occurrence precedes consumer %q", producer, consumer)
	}
	occ.recorded = recordedInOccurrence(events, producer, occ)
	return occ
}

// firstProducerOccurrence returns the producer's first finished occurrence,
// used to show that a repass retains each occurrence's evidence separately.
func firstProducerOccurrence(t *testing.T, events []journal.Event, producer string) producerOccurrence {
	t.Helper()
	var occ producerOccurrence
	for _, ev := range events {
		if ev.Branch != 0 || ev.Stage != producer {
			continue
		}
		if ev.Type == journal.EventStageStarted && occ.startedSeq == 0 {
			occ = producerOccurrence{startedSeq: ev.Seq, attempt: ev.Attempt}
		}
		if ev.Type == journal.EventStageFinished && occ.startedSeq != 0 {
			occ.finishedSeq, occ.finished = ev.Seq, ev
			occ.recorded = recordedInOccurrence(events, producer, occ)
			return occ
		}
	}
	t.Fatalf("producer %q never finished", producer)
	return occ
}

// recordedInOccurrence maps the artifacts recorded strictly inside the
// occurrence's stage.started/stage.finished window to their journal refs.
func recordedInOccurrence(events []journal.Event, producer string, occ producerOccurrence) map[string]journal.Ref {
	recorded := map[string]journal.Ref{}
	for _, ev := range events {
		if ev.Branch != 0 || ev.Type != journal.EventArtifactRecorded || ev.Ref == nil ||
			ev.Seq <= occ.startedSeq || ev.Seq >= occ.finishedSeq || strings.HasPrefix(ev.Name, "context/") {
			continue
		}
		name := ev.Name
		if i := strings.Index(name, ":"+producer+"/"); i >= 0 {
			name = name[i+len(producer)+2:]
		}
		recorded[name] = *ev.Ref
	}
	return recorded
}

// assertRetainedVerified checks the occurrence retained exactly the named
// evidence and that every retained ref verifies by digest and size.
func assertRetainedVerified(t *testing.T, runDir string, occ producerOccurrence, want []string) {
	t.Helper()
	got := make([]string, 0, len(occ.recorded))
	for name, ref := range occ.recorded {
		got = append(got, name)
		if state := verifyEvidence(runDir, ref); state != evidenceVerified {
			t.Fatalf("occurrence %d %s is %s", occ.startedSeq, name, state)
		}
		if strings.HasPrefix(name, "outbox/") {
			assertOutboxInOccurrence(t, occ, ref)
		}
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("occurrence %d retained evidence = %v, want %v", occ.startedSeq, got, want)
	}
}

// assertClassifyVerdict checks the classify gate evaluation that follows the
// occurrence returned the documented verdict, so a routing regression cannot
// hide behind fail and infra sharing the collect branch.
func assertClassifyVerdict(t *testing.T, events []journal.Event, occ producerOccurrence, want string) {
	t.Helper()
	ev, ok := eventOfType(events, journal.EventGateEvaluated, func(ev journal.Event) bool {
		return ev.Branch == 0 && ev.Gate == "classify" && ev.Seq > occ.finishedSeq
	})
	if !ok {
		t.Fatal("classify gate was not evaluated after the producer occurrence")
	}
	if ev.Verdict != want {
		t.Fatalf("classify verdict = %q, want %q", ev.Verdict, want)
	}
}

func verifyEvidence(runDir string, ref journal.Ref) evidenceState {
	full, err := apiv1.ResolveContainedPath(runDir, filepath.FromSlash(ref.Path))
	if errors.Is(err, fs.ErrNotExist) {
		return evidenceAbsent
	}
	if err != nil {
		return evidenceMalformed
	}
	data, err := os.ReadFile(full)
	if errors.Is(err, fs.ErrNotExist) {
		return evidenceAbsent
	}
	if err != nil || int64(len(data)) != ref.Size || journal.Digest(data) != ref.Digest {
		return evidenceMalformed
	}
	return evidenceVerified
}

func consumerManifest(t *testing.T, run failureEvidenceRun, consumer string) ([]apiv1.ContextPointer, bool) {
	t.Helper()
	var manifest *journal.Ref
	for _, ev := range run.events {
		if ev.Type == journal.EventArtifactRecorded && ev.Name == journal.ContextManifestArtifactName(consumer, 1) {
			manifest = ev.Ref
		}
	}
	if manifest == nil {
		return nil, false
	}
	if state := verifyEvidence(run.runDir, *manifest); state != evidenceVerified {
		t.Fatalf("%s context manifest is %s", consumer, state)
	}
	data, err := os.ReadFile(filepath.Join(run.runDir, filepath.FromSlash(manifest.Path)))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ContextPointers []apiv1.ContextPointer `json:"contextPointers"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.ContextPointers, true
}

func manifestDigests(pointers []apiv1.ContextPointer) map[string]int {
	digests := map[string]int{}
	for _, p := range pointers {
		if p.Artifact != nil {
			digests[p.Artifact.Digest]++
		}
	}
	return digests
}

func eventOfType(events []journal.Event, typ journal.EventType, match func(journal.Event) bool) (journal.Event, bool) {
	for _, ev := range events {
		if ev.Type == typ && match(ev) {
			return ev, true
		}
	}
	return journal.Event{}, false
}

// assertEvidenceMatrixRow checks one routable failure row: the exact
// occurrence's status/code, which named evidence the journal retained and
// verified, and that the consumer manifest delivers exactly the
// stage.finished pointers — never the outbox.
func assertEvidenceMatrixRow(t *testing.T, run failureEvidenceRun, wantCode, wantVerdict string, wantRecorded []string) producerOccurrence {
	t.Helper()
	if run.startErr != nil || run.phase != journal.PhaseCompleted {
		t.Fatalf("run = %q, %v; want the diagnostic path to complete", run.phase, run.startErr)
	}
	occ := exactProducerOccurrence(t, run.events, "produce", "collect")
	if occ.finished.Status != string(apiv1.ResultFailure) || occ.finished.Error == nil || occ.finished.Error.Code != wantCode {
		t.Fatalf("producer stage.finished = %+v, want failure %q", occ.finished, wantCode)
	}
	assertClassifyVerdict(t, run.events, occ, wantVerdict)
	assertRetainedVerified(t, run.runDir, occ, wantRecorded)
	pointers, ok := consumerManifest(t, run, "collect")
	if !ok {
		t.Fatal("consumer context manifest missing")
	}
	delivered := manifestDigests(pointers)
	if len(pointers) != len(occ.finished.Artifacts) {
		t.Fatalf("consumer received %d pointers, want the %d stage.finished artifacts", len(pointers), len(occ.finished.Artifacts))
	}
	for _, ref := range occ.finished.Artifacts {
		if delivered[ref.Digest] == 0 {
			t.Fatalf("stage.finished artifact %s not delivered to consumer", ref.Digest)
		}
	}
	for name, ref := range occ.recorded {
		if strings.HasPrefix(name, "outbox/") && delivered[ref.Digest] != 0 {
			t.Fatalf("outbox file %s was delivered through contextFrom", name)
		}
	}
	return occ
}

// assertOutboxInOccurrence checks the outbox path names the selected
// occurrence: its occurrence-<S> segment falls inside the occurrence's
// stage.started/stage.finished window.
func assertOutboxInOccurrence(t *testing.T, occ producerOccurrence, ref journal.Ref) {
	t.Helper()
	prefix := fmt.Sprintf("artifacts/outbox/produce/attempt-%d/occurrence-", occ.attempt)
	var seq uint64
	if !strings.HasPrefix(ref.Path, prefix) {
		t.Fatalf("outbox path %q lacks %q", ref.Path, prefix)
	}
	if _, err := fmt.Sscanf(strings.TrimPrefix(ref.Path, prefix), "%d/", &seq); err != nil || seq <= occ.startedSeq || seq >= occ.finishedSeq {
		t.Fatalf("outbox path %q is outside occurrence (%d, %d)", ref.Path, occ.startedSeq, occ.finishedSeq)
	}
}

func TestFailedTaskEvidenceOrdinaryFailure(t *testing.T) {
	run := runFailureEvidenceFixture(t, failureEvidenceProducer(t, "fail"), apiv1.Task{}, apiv1.Gate{})
	occ := assertEvidenceMatrixRow(t, run, "nonzero_exit", gate.OutcomeFail,
		[]string{"stdout.log", "stderr.log", "result", "outbox/diag/trace.txt"})
	if occ.finished.Outputs["diagnosis"] != "bad-input" {
		t.Fatalf("failed result outputs = %v, want the declared result file lifted", occ.finished.Outputs)
	}

	outbox := occ.recorded["outbox/diag/trace.txt"]
	full := filepath.Join(run.runDir, filepath.FromSlash(outbox.Path))
	if err := os.WriteFile(full, []byte("round 1 trac"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := verifyEvidence(run.runDir, outbox); state != evidenceMalformed {
		t.Fatalf("truncated outbox file is %s, want malformed", state)
	}
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	if state := verifyEvidence(run.runDir, outbox); state != evidenceAbsent {
		t.Fatalf("removed outbox file is %s, want absent", state)
	}
}

func TestFailedTaskEvidenceTimeoutKeepsStreamsAndOutboxButNotResult(t *testing.T) {
	produce := failureEvidenceProducer(t, "timeout")
	// The deadline covers helper start-up and evidence writes before the
	// helper's one-minute sleep, so leave generous headroom for slow hosts.
	produce.TimeoutSeconds = 10
	run := runFailureEvidenceFixture(t, produce, apiv1.Task{}, apiv1.Gate{})
	occ := assertEvidenceMatrixRow(t, run, "timeout", gate.OutcomeInfra,
		[]string{"stdout.log", "stderr.log", "outbox/diag/trace.txt"})
	if _, ok := occ.finished.Outputs["diagnosis"]; ok {
		t.Fatalf("timed-out result file was lifted: %v", occ.finished.Outputs)
	}
}

func TestFailedTaskEvidenceLaunchFailureHasNoProducerFiles(t *testing.T) {
	produce := apiv1.Task{
		Run:    &apiv1.DeterministicRun{Command: []string{"goobers-missing-evidence-producer"}},
		Outbox: []string{"diag"},
	}
	run := runFailureEvidenceFixture(t, produce, apiv1.Task{}, apiv1.Gate{})
	occ := assertEvidenceMatrixRow(t, run, "exec_start", gate.OutcomeFail, nil)
	if len(occ.finished.Artifacts) != 0 {
		t.Fatalf("launch failure carried artifacts: %+v", occ.finished.Artifacts)
	}
}

func TestFailedTaskEvidenceDispatchRefusalSkipsConsumerGate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retry     *apiv1.RetryPolicy
		wantCause journal.TerminalClassification
	}{
		{name: "no-retry", wantCause: journal.TerminalInfrastructureFailure},
		{name: "retry", retry: &apiv1.RetryPolicy{MaxAttempts: 2}, wantCause: journal.TerminalRetryExhaustion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collect := apiv1.Task{InputsFrom: map[string]string{"missing": "produce.notProduced"}, Retry: tc.retry}
			run := runFailureEvidenceFixture(t, failureEvidenceProducer(t, "fail"), collect, apiv1.Gate{})
			assertDispatchRefusal(t, run, tc.wantCause)
		})
	}
}

// assertDispatchRefusal checks a consumer refused at dispatch: the run fails
// with the documented terminal cause, the consumer never finishes or gets a
// manifest, its next gate is skipped, and the producer evidence stays
// verifiable for outer recovery.
func assertDispatchRefusal(t *testing.T, run failureEvidenceRun, wantCause journal.TerminalClassification) {
	t.Helper()
	if run.startErr == nil || run.phase != journal.PhaseFailed {
		t.Fatalf("run = %q, %v; want a failed dispatch", run.phase, run.startErr)
	}
	finished, ok := eventOfType(run.events, journal.EventRunFinished, func(journal.Event) bool { return true })
	if !ok || finished.TerminalCause == nil {
		t.Fatalf("run.finished = %+v, want a terminal cause", finished)
	}
	if finished.TerminalCause.Classification != wantCause {
		t.Fatalf("terminal cause = %q, want %q", finished.TerminalCause.Classification, wantCause)
	}
	if _, ok := eventOfType(run.events, journal.EventStageFinished, func(ev journal.Event) bool { return ev.Stage == "collect" }); ok {
		t.Fatal("refused consumer journaled stage.finished")
	}
	if _, ok := eventOfType(run.events, journal.EventGateEvaluated, func(ev journal.Event) bool { return ev.Gate == "after-collect" }); ok {
		t.Fatal("refused consumer's next gate executed")
	}
	if _, ok := eventOfType(run.events, journal.EventArtifactRecorded, func(ev journal.Event) bool {
		return strings.HasPrefix(ev.Name, "context/collect-")
	}); ok {
		t.Fatal("refused consumer recorded a context manifest")
	}
	var consumerStarts, refusals int
	for _, ev := range run.events {
		if ev.Stage != "collect" {
			continue
		}
		if ev.Type == journal.EventStageStarted {
			consumerStarts++
		}
		if ev.Type == journal.EventError && ev.Error != nil && ev.Error.Code == "executor_error" {
			refusals++
		}
	}
	if consumerStarts == 0 || refusals != consumerStarts {
		t.Fatalf("consumer starts = %d, executor_error refusals = %d; want every attempt refused", consumerStarts, refusals)
	}
	// Outer recovery still finds the producer's verified evidence.
	occ := exactProducerOccurrence(t, run.events, "produce", "collect")
	assertRetainedVerified(t, run.runDir, occ, []string{"stdout.log", "stderr.log", "result", "outbox/diag/trace.txt"})
}

func TestFailedTaskEvidenceRepassSelectsExactOccurrence(t *testing.T) {
	produce := failureEvidenceProducer(t, "round")
	produce.Inputs["counterFile"] = filepath.Join(t.TempDir(), "rounds")
	classify := apiv1.Gate{
		Automated: &apiv1.AutomatedGate{Check: "output-equals", Params: map[string]string{"key": "round", "equals": "2"}},
		Branches:  map[string]string{gate.OutcomePass: "collect", gate.OutcomeFail: "produce"},
	}
	run := runFailureEvidenceFixture(t, produce, apiv1.Task{}, classify)
	if run.startErr != nil || run.phase != journal.PhaseCompleted {
		t.Fatalf("run = %q, %v", run.phase, run.startErr)
	}
	var attempts []int
	for _, ev := range run.events {
		if ev.Type == journal.EventStageFinished && ev.Stage == "produce" {
			attempts = append(attempts, ev.Attempt)
		}
	}
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 1 {
		t.Fatalf("producer attempts = %v, want two occurrences both at attempt 1", attempts)
	}

	occ := exactProducerOccurrence(t, run.events, "produce", "collect")
	if occ.finished.Outputs["round"] != float64(2) {
		t.Fatalf("selected occurrence outputs = %v, want round 2", occ.finished.Outputs)
	}
	assertRepassRetention(t, run, occ)
	outbox := occ.recorded["outbox/diag/trace.txt"]
	assertOutboxInOccurrence(t, occ, outbox)
	data, err := os.ReadFile(filepath.Join(run.runDir, filepath.FromSlash(outbox.Path)))
	if err != nil || string(data) != "round 2 trace\n" || verifyEvidence(run.runDir, outbox) != evidenceVerified {
		t.Fatalf("selected outbox = %q, %v", data, err)
	}

	// The consumer manifest accumulates both occurrences under the same
	// pointer names, so names alone cannot pick the triggering occurrence.
	pointers, ok := consumerManifest(t, run, "collect")
	if !ok {
		t.Fatal("consumer context manifest missing")
	}
	names := map[string]int{}
	for _, p := range pointers {
		if strings.HasPrefix(p.Name, "produce.") {
			names[p.Name]++
		}
	}
	if names["produce.artifact[0]"] != 2 {
		t.Fatalf("manifest producer names = %v, want each name delivered for both occurrences", names)
	}
	delivered := manifestDigests(pointers)
	for _, ref := range occ.finished.Artifacts {
		if delivered[ref.Digest] == 0 {
			t.Fatalf("selected occurrence artifact %s not delivered", ref.Digest)
		}
	}

	assertRepassOccurrenceStates(t, run, occ, outbox)
}

// assertRepassRetention checks each repass occurrence retains its own complete
// evidence set: stdout, stderr, result and outbox, every ref verified, and
// each item distinct from the other occurrence's (all carry the round).
func assertRepassRetention(t *testing.T, run failureEvidenceRun, selected producerOccurrence) {
	t.Helper()
	want := []string{"stdout.log", "stderr.log", "result", "outbox/diag/trace.txt"}
	first := firstProducerOccurrence(t, run.events, "produce")
	if first.finishedSeq >= selected.startedSeq {
		t.Fatalf("first occurrence (%d, %d) does not precede the selected one at %d", first.startedSeq, first.finishedSeq, selected.startedSeq)
	}
	if first.finished.Outputs["round"] != float64(1) {
		t.Fatalf("first occurrence outputs = %v, want round 1", first.finished.Outputs)
	}
	assertRetainedVerified(t, run.runDir, first, want)
	assertRetainedVerified(t, run.runDir, selected, want)
	for _, name := range want {
		if first.recorded[name].Digest == selected.recorded[name].Digest {
			t.Fatalf("%s has the same digest in both occurrences; want per-occurrence evidence", name)
		}
	}
}

// assertRepassOccurrenceStates tampers with, then removes, the outbox file of
// the occurrence selected by journal seq and checks it reports malformed then
// absent, while the earlier occurrence's same-named file stays verified.
func assertRepassOccurrenceStates(t *testing.T, run failureEvidenceRun, occ producerOccurrence, outbox journal.Ref) {
	t.Helper()
	earlier, ok := eventOfType(run.events, journal.EventArtifactRecorded, func(ev journal.Event) bool {
		return ev.Branch == 0 && ev.Ref != nil && ev.Seq < occ.startedSeq &&
			strings.HasPrefix(ev.Ref.Path, "artifacts/outbox/produce/") && strings.HasSuffix(ev.Ref.Path, "/diag/trace.txt")
	})
	if !ok {
		t.Fatal("earlier occurrence's outbox trace not recorded")
	}
	if earlier.Ref.Path == outbox.Path {
		t.Fatalf("both occurrences share outbox path %q", outbox.Path)
	}
	assertEarlierVerified := func(phase string) {
		t.Helper()
		if state := verifyEvidence(run.runDir, *earlier.Ref); state != evidenceVerified {
			t.Fatalf("earlier occurrence's outbox is %s after %s", state, phase)
		}
	}

	full := filepath.Join(run.runDir, filepath.FromSlash(outbox.Path))
	// Same size, different digest: the earlier round's content.
	if err := os.WriteFile(full, []byte("round 1 trace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := verifyEvidence(run.runDir, outbox); state != evidenceMalformed {
		t.Fatalf("selected outbox with another round's content is %s, want malformed", state)
	}
	if err := os.WriteFile(full, []byte("round 2 trac"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := verifyEvidence(run.runDir, outbox); state != evidenceMalformed {
		t.Fatalf("truncated selected outbox is %s, want malformed", state)
	}
	assertEarlierVerified("tampering")
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	if state := verifyEvidence(run.runDir, outbox); state != evidenceAbsent {
		t.Fatalf("removed selected outbox is %s, want absent", state)
	}
	assertEarlierVerified("removal")
}
