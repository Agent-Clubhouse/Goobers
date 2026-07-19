// This file is T1's (#101) executable acceptance check, mirroring
// acceptance_test.go's pattern for the same reasons: it drives the real
// `tutor` shape (gather -> analyze -> draft-change -> validate gate -> push ->
// open-pr) through the real runner via `goobers run`, offline, with a fake
// harness standing in for the Copilot CLI on both agentic stages. Provider
// stages use `true` sentinels here; their own behavior is covered separately.
// gather-signals here uses `true` in place of the real `goobers
// telemetry-query` command, which — like work-nomination.yaml's own
// gather-signals stage — isn't wired into the live runner path yet (#132/
// #148); this is the same accepted gap the shipped work-nomination.yaml
// already carries, not something T1 introduces.
package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
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
)

// tutorWorkflowYAML mirrors the real selfhost Tutor control flow while
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
        command: ["VALIDATE_COMMAND"]
      next: config-valid
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
        pass: push-branch
        fail: "@abort"
`

// initTutorDemo scaffolds an instance via `goobers init`, swaps the starter
// workflow for the trimmed tutor fixture above, and installs the analyst +
// config-author goobers it references — the tutor analogue of
// acceptance_test.go's initAcceptanceDemo. repoCloneURL points worktrees at
// a local bare git fixture; newAgenticAdapter scripts a fake harness for
// both agentic stages. config-author's fake writes and commits a file in the
// run's real worktree, leaving publication to the later deterministic stage.
func initTutorDemo(t *testing.T, validateCommand string) string {
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
	workflow := strings.Replace(tutorWorkflowYAML, "VALIDATE_COMMAND", validateCommand, 1)
	writeFixture(t, filepath.Join(gaggleDir, "workflows", "tutor.yaml"), workflow)
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
	// resolve identically to how the real selfhost config declares it.
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

	fixtureRepo := newDaemonFixtureRepo(t)
	prevRepo := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil }

	prevAdapter := newAgenticAdapter
	newAgenticAdapter = func(gooberName string, _ map[string]string) harness.Adapter {
		return &harness.FakeAdapter{
			Transcript: []byte("fake harness session for " + gooberName + "\n"),
			Act: func(_ context.Context, req harness.RunRequest) error {
				if gooberName == "config-author" {
					if err := tutorFixtureCommit(req.Workspace); err != nil {
						return err
					}
				}
				return harness.WriteCompletion(req.Workspace, req.CompletionPath, tutorFixtureAct(gooberName))
			},
		}
	}

	t.Cleanup(func() {
		repoCloneURL = prevRepo
		newAgenticAdapter = prevAdapter
	})
	return root
}

// tutorFixtureCommit simulates config-author's real job: write and commit the
// drafted change, leaving publication to the later deterministic stage.
func tutorFixtureCommit(workspace string) error {
	if err := os.WriteFile(filepath.Join(workspace, "tutor-finding.md"), []byte("# Tutor finding\n\nfixture config change\n"), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		// Explicit, worktree-local identity (matching daemon_test.go's
		// newDaemonFixtureRepo pattern) rather than relying on a global
		// ~/.gitconfig existing in whatever environment this test runs in —
		// a CI runner's minimal/ephemeral git environment may have no global
		// identity configured at all, where a dev workstation typically
		// does, so `git commit` can fail here in CI while passing locally
		// every time.
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"add", "tutor-finding.md"},
		{"commit", "-m", "tutor: fixture config change"},
	} {
		cmd := exec.Command("git", args...)
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
// fires" test-plan item: the real selfhost tutor.yaml's schedule expression
// is a valid 5-field cron and actually computes a next fire time, not just a
// structurally-parseable string (already covered separately by
// TestSelfhostWorkflowsCompile's CheckSchedules pass).
func TestTutorScheduleParsesAndFires(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "selfhost", "gaggles", "goobers", "workflows", "tutor.yaml"))
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
	root := initTutorDemo(t, "true")

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
		"gather-signals", "analyze", "draft-change", "validate-config", "push-branch", "open-pr",
	}) {
		t.Errorf("started stages = %v, want the full Tutor path through open-pr", got)
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
	root := initTutorDemo(t, "false")

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
		t.Fatalf("started stages = %v, push/open-pr must not run after invalid config", got)
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

func tutorStartedStages(events []journal.Event) []string {
	var names []string
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			names = append(names, event.Stage)
		}
	}
	return names
}
