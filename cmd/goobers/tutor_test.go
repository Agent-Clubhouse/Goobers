// This file is T1's (#101) executable acceptance check, mirroring
// acceptance_test.go's pattern for the same reasons: it drives the real
// `tutor` shape (gather -> analyze -> draft-change -> validate gate -> push ->
// open-pr) through the real runner via `goobers run`, offline, with a fake
// harness standing in for the Copilot CLI on both agentic stages. Provider
// stages use `true` sentinels here; their own behavior is covered separately.
// Validation runs the real CLI against a drafted reference-workflows tree.
// gather-signals here uses `true` to keep this workflow-plumbing fixture
// independent of telemetry-query's rollup fixture; telemetryquery_test.go
// covers the real connector command.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/testgit"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

// tutorWorkflowYAML mirrors the real reference-workflows Tutor control flow while
// replacing provider-backed commands with offline sentinels.
const tutorWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: tutor
spec:
  gaggle: example
  displayName: Tutor (acceptance fixture)
  triggers:
    - type: schedule
      schedule: "30 5 * * 0"
  readiness:
    maxConcurrentRuns: 1
    maxRunsPerHour: 1
  start: gather-signals
  tasks:
    - name: gather-signals
      type: deterministic
      goal: Query cross-run telemetry for recurring problems.
      run:
        command: ["true"]
      next: analyze
    - name: analyze
      type: agentic
      goober: analyst
      goal: Diagnose the highest-priority recurring problem and write a finding.
      capabilities:
        - telemetry:read
        - journal:read
        - agent:model
      next: draft-change
    - name: draft-change
      type: agentic
      goober: config-author
      goal: Implement the analyst's finding and push it to the run's branch.
      capabilities:
        - repo:push
        - agent:model
      next: validate-config
    - name: validate-config
      type: deterministic
      goal: Validate the drafted config.
      run:
        command: ["goobers", "validate", "--source-tree", "reference-workflows"]
      next: config-valid
    - name: check-fail-first
      type: deterministic
      goal: Enforce the fail-first validation-authorship contract (#1214).
      run:
        command: ["goobers", "check-fail-first"]
      next: fail-first-valid
    - name: push-branch
      type: deterministic
      goal: Push the run branch.
      run:
        command: ["true"]
      next: open-pr
    - name: open-pr
      type: deterministic
      goal: Open the config PR.
      run:
        command: ["true"]
  gates:
    - name: config-valid
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: check-fail-first
        fail: "@abort"
    - name: fail-first-valid
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: push-branch
        fail: "@abort"
`

// tutorDraftMode selects what config-author's fake commits in
// tutorFixtureCommit, exercising validate-config's config-valid gate and (#1214)
// check-fail-first's fail-first-valid gate independently.
type tutorDraftMode int

const (
	// tutorDraftValid changes only a display-name string: valid config, no new
	// gate, so check-fail-first must pass trivially with no evidence required.
	tutorDraftValid tutorDraftMode = iota
	// tutorDraftInvalidYAML corrupts the YAML so validate-config's config-valid
	// gate fails and the run aborts before check-fail-first ever runs.
	tutorDraftInvalidYAML
	// tutorDraftNewGateNoEvidence adds a new gate to the drafted tutor.yaml
	// (TUT-A2's "workflow-level validation stage") without committing the
	// required fail-first evidence file — check-fail-first must abort.
	tutorDraftNewGateNoEvidence
	// tutorDraftNewGateWithEvidence adds the same new gate plus a valid
	// fail-first-evidence.json — check-fail-first must pass.
	tutorDraftNewGateWithEvidence
)

// initTutorDemo scaffolds an instance via `goobers init`, swaps the starter
// workflow for the trimmed tutor fixture above, and installs the analyst +
// config-author goobers it references — the tutor analogue of
// acceptance_test.go's initAcceptanceDemo. repoCloneURL points worktrees at
// a local bare git fixture; newAgenticAdapter scripts a fake harness for
// both agentic stages. config-author's fake writes and commits a valid or
// malformed reference-workflows config change in the run's real worktree, leaving
// validation and publication to the later deterministic stages.
func initTutorDemo(t *testing.T, mode tutorDraftMode) string {
	t.Helper()
	t.Setenv("GOOBERS_GITHUB_TOKEN", "ghp_tutor_fixture_dummy_token")
	root := initDemo(t)

	gaggleDir := filepath.Join(root, "config", "gaggles", "example")
	if err := os.RemoveAll(filepath.Join(gaggleDir, "workflows")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(gaggleDir, "goobers")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(gaggleDir, "workflows", "tutor.yaml"), tutorWorkflowYAML)
	for _, g := range []struct {
		name, role string
		caps       []string
	}{
		{"analyst", "analyst", []string{"telemetry:read", "journal:read", "agent:model"}},
		{"config-author", "config-author", []string{"repo:push", "agent:model"}},
	} {
		dir := filepath.Join(gaggleDir, "goobers", g.name)
		writeFixture(t, filepath.Join(dir, "goober.yaml"), acceptanceGooberYAML(g.name, g.role, g.caps))
		writeFixture(t, filepath.Join(dir, "instructions.md"), "You are the "+g.name+" fixture goober for the #101 acceptance check.\n")
	}
	// acceptanceGooberYAML hardcodes `workflows: [acceptance]`; rewrite it to
	// this fixture's real workflow name so goobersByName/compiledMachines
	// resolve identically to how the real reference-workflows config declares it.
	for _, name := range []string{"analyst", "config-author"} {
		p := filepath.Join(gaggleDir, "goobers", name, "goober.yaml")
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		fixed := strings.ReplaceAll(string(data), "- acceptance", "- tutor")
		if err := os.WriteFile(p, []byte(fixed), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fixtureRepo := newTutorFixtureRepo(t)
	prevRepo := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil }

	prevAdapter := newAgenticAdapter
	newAgenticAdapter = func(gooberName string, _ map[string]string) harness.Adapter {
		return &harnesstest.FakeAdapter{
			Transcript: []byte("fake harness session for " + gooberName + "\n"),
			Act: func(_ context.Context, req harness.RunRequest) error {
				if gooberName == "config-author" {
					if err := tutorFixtureCommit(req.Workspace, mode); err != nil {
						return err
					}
				}
				return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, tutorFixtureAct(gooberName))
			},
		}
	}

	t.Cleanup(func() {
		repoCloneURL = prevRepo
		newAgenticAdapter = prevAdapter
	})
	return root
}

func newTutorFixtureRepo(t *testing.T) string {
	t.Helper()
	bare := newDaemonFixtureRepo(t)
	work := t.TempDir()
	runFixtureGit(t, "", "clone", bare, work)
	runFixtureGit(t, work, "config", "user.email", "test@example.com")
	runFixtureGit(t, work, "config", "user.name", "test")
	if err := os.CopyFS(filepath.Join(work, "reference-workflows"), os.DirFS(filepath.Join("..", "..", "reference-workflows"))); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(work, "docs", "fixture.md"), "# Fixture documentation\n")
	runFixtureGit(t, work, "add", "reference-workflows", "docs")
	runFixtureGit(t, work, "commit", "-m", "add reference-workflows config fixture")
	runFixtureGit(t, work, "push", "origin", "main")
	return bare
}

// tutorGateFixtureRelPath is the file config-author's fake edits — the same
// path the real Tutor's draft-change stage would touch, and the path
// check-fail-first's IsWorkflowFile match must recognize as a workflows/*.yaml.
const tutorGateFixtureRelPath = "reference-workflows/gaggles/goobers/workflows/tutor.yaml"

// tutorFailFirstValidGateBlock is the fail-first-valid gate exactly as landed
// in the real reference-workflows/gaggles/goobers/workflows/tutor.yaml (#1214). The
// new-gate fixture modes rewrite its "fail" branch (never exercised by these
// tests' successful runs) to route through a fixture-only extra task+gate,
// making the newly added gate reachable without disturbing the traversed path.
const tutorFailFirstValidGateBlock = `    - name: fail-first-valid
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: push-branch
        fail: "@abort"
`

// tutorFailFirstValidGateBlockWithExtraGate is the same block with its "fail"
// branch rewired to a new fixture-only task, plus a second, newly authored
// gate ("extra-diagnostic-valid") — the "workflow-level validation stage" #1214
// requires fail-first evidence for.
const tutorFailFirstValidGateBlockWithExtraGate = `    - name: fail-first-valid
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: push-branch
        fail: extra-diagnostic
    - name: extra-diagnostic-valid
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: push-branch
        fail: "@abort"
`

// tutorExtraDiagnosticTaskBlock is the fixture-only task the rewired
// fail-first-valid "fail" branch above reaches, feeding the new
// extra-diagnostic-valid gate. Inserted directly before the tasks list's
// "gates:" sibling key.
const tutorExtraDiagnosticTaskBlock = `    - name: extra-diagnostic
      type: deterministic
      goal: Fixture-only diagnostic task exercising a newly authored gate (#1214 test).
      run:
        command: ["true"]
      next: extra-diagnostic-valid
`

// tutorNewGateKey is the Evidence map key check-fail-first expects for the
// fixture's newly authored gate: "<file>#<gate>" (failfirst.GateRef.Key()).
const tutorNewGateKey = tutorGateFixtureRelPath + "#extra-diagnostic-valid"

// tutorFixtureCommit simulates config-author's real job: write and commit the
// drafted change, leaving validation and publication to the later
// deterministic stages.
func tutorFixtureCommit(workspace string, mode tutorDraftMode) error {
	path := filepath.Join(workspace, tutorGateFixtureRelPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	extraFiles := []string{tutorGateFixtureRelPath}
	switch mode {
	case tutorDraftInvalidYAML:
		data = append(data, []byte("\nmalformed: [\n")...)

	case tutorDraftNewGateNoEvidence, tutorDraftNewGateWithEvidence:
		changed := []byte(strings.Replace(string(data), tutorFailFirstValidGateBlock, tutorFailFirstValidGateBlockWithExtraGate, 1))
		if bytes.Equal(data, changed) {
			return errors.New("tutor fixture fail-first-valid gate sentinel not found")
		}
		const gatesKey = "  gates:\n"
		withTask := strings.Replace(string(changed), gatesKey, tutorExtraDiagnosticTaskBlock+gatesKey, 1)
		if withTask == string(changed) {
			return errors.New("tutor fixture gates: sentinel not found")
		}
		data = []byte(withTask)

		if mode == tutorDraftNewGateWithEvidence {
			evidence := fmt.Sprintf(`{"gates":{%q:{"preFix":"fail","postFix":"pass","runEvidence":"fixture-run-1214"}}}`, tutorNewGateKey)
			evidencePath := filepath.Join(workspace, "fail-first-evidence.json")
			if err := os.WriteFile(evidencePath, []byte(evidence), 0o644); err != nil {
				return err
			}
			extraFiles = append(extraFiles, "fail-first-evidence.json")
		}

	default:
		changed := []byte(strings.Replace(
			string(data),
			"displayName: Tutor (self-improvement loop)",
			"displayName: Tutor (self-improvement loop fixture)",
			1,
		))
		if bytes.Equal(data, changed) {
			return errors.New("tutor fixture displayName sentinel not found")
		}
		data = changed
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	commitArgs := [][]string{
		// Explicit, worktree-local identity (matching daemon_test.go's
		// newDaemonFixtureRepo pattern) rather than relying on a global
		// ~/.gitconfig existing in whatever environment this test runs in —
		// a CI runner's minimal/ephemeral git environment may have no global
		// identity configured at all, where a dev workstation typically
		// does, so `git commit` can fail here in CI while passing locally
		// every time.
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	}
	for _, f := range extraFiles {
		commitArgs = append(commitArgs, []string{"add", f})
	}
	commitArgs = append(commitArgs, []string{"commit", "-m", "tutor: fixture config change"})
	for _, args := range commitArgs {
		cmd := testgit.Command(args...)
		cmd.Dir = workspace
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			return &execError{args: args, out: out.String(), err: err}
		}
	}
	return nil
}

type execError struct {
	args []string
	out  string
	err  error
}

func (e *execError) Error() string {
	return "git " + strings.Join(e.args, " ") + ": " + e.err.Error() + "\n" + e.out
}

// tutorFixtureAct is the scripted fake-harness completion payload.
func tutorFixtureAct(gooberName string) apiv1.ResultEnvelope {
	switch gooberName {
	case "analyst":
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "diagnosed one recurring problem"}
	default:
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "drafted the recommended config change"}
	}
}

// TestTutorScheduleParsesAndFires is T1's "schedule expression parses and
// fires" test-plan item: the real reference-workflows tutor.yaml's schedule expression
// is a valid 5-field cron and actually computes a next fire time, not just a
// structurally-parseable string (already covered separately by
// TestReferenceWorkflowsCompile's CheckSchedules pass).
func TestTutorScheduleParsesAndFires(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "tutor.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	if len(w.Spec.Triggers) != 1 || w.Spec.Triggers[0].Type != apiv1.TriggerSchedule {
		t.Fatalf("expected exactly one schedule trigger, got %+v", w.Spec.Triggers)
	}
	sched, err := localscheduler.ParseSchedule(w.Spec.Triggers[0].Schedule)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", w.Spec.Triggers[0].Schedule, err)
	}
	after, err := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	next := sched.Next(after)
	if !next.After(after) {
		t.Fatalf("Next(%v) = %v, want a time strictly after", after, next)
	}
}

func TestTutorValidDraftReachesOpenPR(t *testing.T) {
	root := initTutorDemo(t, tutorDraftValid)

	code, stdout, stderr := runArgs(t, "run", "tutor", root)
	if code != 0 {
		t.Fatalf("goobers run tutor: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "phase=completed") {
		t.Fatalf("expected the real runner to complete the tutor fixture loop, stdout = %q", stdout)
	}

	runID := runIDFromRunStdout(t, stdout)
	rd, err := journal.OpenRead(filepath.Join(root, "runs", runID))
	if err != nil {
		t.Fatalf("OpenRead(%s): %v", runID, err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) == 0 || events[0].Type != journal.EventRunStarted || events[len(events)-1].Type != journal.EventRunFinished {
		t.Fatalf("event sequence must start with run.started and end with run.finished")
	}
	if got := tutorStartedStages(events); !slices.Equal(got, []string{
		"gather-signals", "analyze", "draft-change", "validate-config", "check-fail-first", "push-branch", "open-pr",
	}) {
		t.Errorf("started stages = %v, want the full Tutor path through open-pr", got)
	}
	if !tutorGateEvaluatedPass(events, "config-valid", "check-fail-first") {
		t.Fatal("journal has no config-valid pass verdict targeting check-fail-first")
	}
	if !tutorGateEvaluatedPass(events, "fail-first-valid", "push-branch") {
		t.Fatal("journal has no fail-first-valid pass verdict targeting push-branch (no new gate on this branch, so it must pass trivially)")
	}

	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseCompleted {
		t.Fatalf("state.json phase = %q, want completed", st.Phase)
	}
}

func TestTutorInvalidDraftAbortsBeforePush(t *testing.T) {
	root := initTutorDemo(t, tutorDraftInvalidYAML)

	code, stdout, stderr := runArgs(t, "run", "tutor", root)
	if code != 1 {
		t.Fatalf("goobers run tutor: code = %d, want 1; stderr = %q", code, stderr)
	}

	runID := runIDFromRunStdout(t, stdout)
	rd, err := journal.OpenRead(filepath.Join(root, "runs", runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	if got := tutorStartedStages(events); !slices.Equal(got, []string{
		"gather-signals", "analyze", "draft-change", "validate-config",
	}) {
		t.Fatalf("started stages = %v, check-fail-first/push/open-pr must not run after invalid config", got)
	}
	var failedClosed bool
	for _, event := range events {
		if event.Type == journal.EventGateEvaluated &&
			event.Gate == "config-valid" &&
			event.Verdict == "fail" &&
			event.Target == "@abort" {
			failedClosed = true
		}
	}
	if !failedClosed {
		t.Fatal("journal has no config-valid fail verdict targeting @abort")
	}
	st, err := rd.State()
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != journal.PhaseAborted {
		t.Fatalf("state.json phase = %q, want aborted", st.Phase)
	}
}

// TestTutorNewGateWithoutFailFirstEvidenceAbortsBeforePush is TUT-A2's (#1214)
// core acceptance case: a branch that adds a new workflow gate — a
// workflow-level validation stage — without committed fail-first evidence
// must never reach push-branch/open-pr. A vacuously-passing "closes the
// finding" check must not be publishable.
func TestTutorNewGateWithoutFailFirstEvidenceAbortsBeforePush(t *testing.T) {
	root := initTutorDemo(t, tutorDraftNewGateNoEvidence)

	code, stdout, stderr := runArgs(t, "run", "tutor", root)
	if code != 1 {
		t.Fatalf("goobers run tutor: code = %d, want 1; stderr = %q", code, stderr)
	}

	runID := runIDFromRunStdout(t, stdout)
	rd, err := journal.OpenRead(filepath.Join(root, "runs", runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	if got := tutorStartedStages(events); !slices.Equal(got, []string{
		"gather-signals", "analyze", "draft-change", "validate-config", "check-fail-first",
	}) {
		t.Fatalf("started stages = %v, push/open-pr must not run when a new gate lacks fail-first evidence", got)
	}
	var failedClosed bool
	for _, event := range events {
		if event.Type == journal.EventGateEvaluated &&
			event.Gate == "fail-first-valid" &&
			event.Verdict == "fail" &&
			event.Target == "@abort" {
			failedClosed = true
		}
	}
	if !failedClosed {
		t.Fatal("journal has no fail-first-valid fail verdict targeting @abort")
	}
	st, err := rd.State()
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != journal.PhaseAborted {
		t.Fatalf("state.json phase = %q, want aborted", st.Phase)
	}
}

// TestTutorNewGateWithFailFirstEvidenceReachesOpenPR is TUT-A2's (#1214)
// positive case: the same new-gate diff as
// TestTutorNewGateWithoutFailFirstEvidenceAbortsBeforePush, but with a
// committed fail-first-evidence.json asserting the required red-then-green
// result — the run must publish normally.
func TestTutorNewGateWithFailFirstEvidenceReachesOpenPR(t *testing.T) {
	root := initTutorDemo(t, tutorDraftNewGateWithEvidence)

	code, stdout, stderr := runArgs(t, "run", "tutor", root)
	if code != 0 {
		t.Fatalf("goobers run tutor: code = %d, stderr = %q", code, stderr)
	}

	runID := runIDFromRunStdout(t, stdout)
	rd, err := journal.OpenRead(filepath.Join(root, "runs", runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	if got := tutorStartedStages(events); !slices.Equal(got, []string{
		"gather-signals", "analyze", "draft-change", "validate-config", "check-fail-first", "push-branch", "open-pr",
	}) {
		t.Fatalf("started stages = %v, want the full Tutor path through open-pr with valid fail-first evidence", got)
	}
	if !tutorGateEvaluatedPass(events, "fail-first-valid", "push-branch") {
		t.Fatal("journal has no fail-first-valid pass verdict targeting push-branch")
	}
	st, err := rd.State()
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != journal.PhaseCompleted {
		t.Fatalf("state.json phase = %q, want completed", st.Phase)
	}
}

func tutorStartedStages(events []journal.Event) []string {
	var names []string
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			names = append(names, event.Stage)
		}
	}
	return names
}

// tutorGateEvaluatedPass reports whether events records gate evaluating to a
// pass verdict targeting target.
func tutorGateEvaluatedPass(events []journal.Event, gate, target string) bool {
	for _, event := range events {
		if event.Type == journal.EventGateEvaluated &&
			event.Gate == gate &&
			event.Verdict == "pass" &&
			event.Target == target {
			return true
		}
	}
	return false
}
