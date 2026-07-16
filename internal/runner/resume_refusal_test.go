package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// newRefusalRun hand-constructs a mid-flight run pinned to digest — one
// finished stage, checkpoint naming it, no terminal event — exactly the
// on-disk shape a daemon restart finds after a run was interrupted and the
// workflow YAML changed underneath it (WF-016's refusal trigger, #520).
func newRefusalRun(t *testing.T, runsDir, runID, digest string) {
	t.Helper()
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "fixture", WorkflowVersion: 1,
		WorkflowDigest: digest, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// refusalTestRunner builds a Runner whose FinalizeTerminal hook records its
// calls — the seam cmd/goobers wires to claim release (#526), so "the hook
// fired with the terminal phase" is the runner-level proof the claim is
// released.
func refusalTestRunner(t *testing.T, runsDir, fixtureRepo string, wtMgr *worktree.Manager, det invoke.Deterministic) (*Runner, *[]journal.RunPhase) {
	t.Helper()
	var finalized []journal.RunPhase
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
		FinalizeTerminal: func(_ string, phase journal.RunPhase, _ *journal.Run) error {
			finalized = append(finalized, phase)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, &finalized
}

// TestRunnerResumeDigestMismatchFailsAndFinalizes is #520's acceptance
// scenario, per the maintainer ruling (issue #520 comment
// 2026-07-16T08:45:22Z): a run pinned to a digest the passed Machine no
// longer matches (the workflow YAML changed while the run was in flight)
// must refuse to resume — but terminally, at the canonical PhaseFailed (not
// aborted: nobody chose to stop this run, it structurally cannot proceed) —
// run.finished{failed} journaled with the WF-016 text as ITS OWN error
// detail (not a separate preceding event), the FinalizeTerminal hook (claim
// release, #526) fired, and nil error returned so the daemon records the
// canonical phase — never a raw error string with the journal left
// phase=running forever.
func TestRunnerResumeDigestMismatchFailsAndFinalizes(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	const runID = "run-digest-mismatch"
	newRefusalRun(t, runsDir, runID, "sha256:pinned-to-the-old-workflow-shape")

	det := &countingDeterministic{}
	r, finalized := refusalTestRunner(t, runsDir, fixtureRepo, wtMgr, det)

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   runID,
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v — a handled refusal must not surface an error, or the daemon journals it as a raw status string", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if det.calls != 0 {
		t.Fatalf("executor dispatched %d times, want 0 — WF-016's refusal itself must not change: the run is never walked", det.calls)
	}
	if len(*finalized) != 1 || (*finalized)[0] != journal.PhaseFailed {
		t.Fatalf("FinalizeTerminal calls = %v, want exactly [failed] — this hook is what releases the run's claim", *finalized)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	phase, err := rd.Phase()
	if err != nil {
		t.Fatalf("Phase: %v", err)
	}
	if phase != journal.PhaseFailed {
		t.Fatalf("journaled phase = %q, want failed — the canonical terminal must be in the run's own journal, not just the returned Result", phase)
	}

	// state.json's Reason must durably carry the WF-016 text too (ruling
	// point 2) — an operator inspecting state.json alone, without reading
	// the full event log, must be able to see why.
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !strings.Contains(st.Reason, "WF-016") {
		t.Fatalf("state.json Reason = %q, want it to contain \"WF-016\"", st.Reason)
	}
	if !strings.Contains(st.Reason, "sha256:pinned-to-the-old-workflow-shape") || !strings.Contains(st.Reason, machine.Digest()) {
		t.Fatalf("state.json Reason = %q, want it to name both the pinned and the offered digest", st.Reason)
	}

	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var finished int
	var runFinishedErr *journal.ErrorDetail
	for _, e := range events {
		if e.Type == journal.EventRunFinished {
			finished++
			runFinishedErr = e.Error
		}
	}
	if finished != 1 {
		t.Fatalf("run.finished count = %d, want exactly 1", finished)
	}
	if runFinishedErr == nil {
		t.Fatal("run.finished carries no Error detail — the refusal reason must be ON the terminal event itself, per the ruling")
	}
	if runFinishedErr.Code != "resume_refused_digest_mismatch" {
		t.Fatalf("run.finished error code = %q, want resume_refused_digest_mismatch", runFinishedErr.Code)
	}
	if !strings.Contains(runFinishedErr.Message, "WF-016") {
		t.Fatalf("run.finished error message = %q, want it to contain \"WF-016\"", runFinishedErr.Message)
	}
	if !strings.Contains(runFinishedErr.Message, "sha256:pinned-to-the-old-workflow-shape") || !strings.Contains(runFinishedErr.Message, machine.Digest()) {
		t.Fatalf("run.finished error message %q must name both the pinned and the offered digest", runFinishedErr.Message)
	}
}

// TestRunnerResumeMissingDigestFailsAndFinalizes covers the sibling WF-016
// refusal branch (#112's unpinned-run hardening) under #520's terminal
// contract: same canonical failed phase, distinct journaled code so the two
// refusal causes stay distinguishable.
func TestRunnerResumeMissingDigestFailsAndFinalizes(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	const runID = "run-missing-digest"
	newRefusalRun(t, runsDir, runID, "")

	det := &countingDeterministic{}
	r, finalized := refusalTestRunner(t, runsDir, fixtureRepo, wtMgr, det)

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   runID,
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if len(*finalized) != 1 || (*finalized)[0] != journal.PhaseFailed {
		t.Fatalf("FinalizeTerminal calls = %v, want exactly [failed]", *finalized)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !strings.Contains(st.Reason, "WF-016") {
		t.Fatalf("state.json Reason = %q, want it to contain \"WF-016\"", st.Reason)
	}

	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawCode bool
	for _, e := range events {
		if e.Type == journal.EventRunFinished && e.Error != nil && e.Error.Code == "resume_refused_missing_digest" {
			sawCode = true
		}
	}
	if !sawCode {
		t.Fatal("run.finished carries no resume_refused_missing_digest error detail")
	}
}

// TestRunnerResumeAfterRefusalIsTerminalNoop: once a refusal has failed a
// run, a later Resume of the same run (the daemon restarting again — the
// exact loop the live leak came from) must short-circuit on the terminal
// fast path: report failed, never append a second run.finished. This is why
// Resume checks the reconstructed phase BEFORE the WF-016 digest
// verification.
func TestRunnerResumeAfterRefusalIsTerminalNoop(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	const runID = "run-refused-twice"
	newRefusalRun(t, runsDir, runID, "sha256:pinned-to-the-old-workflow-shape")

	det := &countingDeterministic{}
	r, finalized := refusalTestRunner(t, runsDir, fixtureRepo, wtMgr, det)
	in := ResumeInput{
		RunID:   runID,
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}

	if _, err := r.Resume(context.Background(), in); err != nil {
		t.Fatalf("first Resume: %v", err)
	}
	res, err := r.Resume(context.Background(), in)
	if err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("second Resume phase = %q, want failed", res.Phase)
	}
	// The terminal fast path re-fires FinalizeTerminal (idempotent claim
	// release, same as any already-terminal resume) — both calls must carry
	// the failed phase.
	for i, p := range *finalized {
		if p != journal.PhaseFailed {
			t.Fatalf("finalize call %d phase = %q, want failed", i, p)
		}
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var finished, refusals int
	for _, e := range events {
		if e.Type == journal.EventRunFinished {
			finished++
			if e.Error != nil && e.Error.Code == "resume_refused_digest_mismatch" {
				refusals++
			}
		}
	}
	if finished != 1 {
		t.Fatalf("run.finished count = %d, want exactly 1 — a second Resume must not re-terminate a finished run", finished)
	}
	if refusals != 1 {
		t.Fatalf("refusal event count = %d, want exactly 1", refusals)
	}
}
