// This file is T1's (#101) executable acceptance check, mirroring
// acceptance_test.go's pattern for the same reasons: it drives the real
// `tutor` shape (gather -> analyze -> draft-change) through the real runner
// via `goobers run`, offline, with a fake harness standing in for the
// Copilot CLI on both agentic stages. It deliberately stops at
// draft-change, one stage short of the real tutor.yaml's `open-pr` — same
// scoping call acceptance_test.go already made for implementation.yaml's
// provider-backed stages: open-pr dispatches as a real `goobers open-pr` OS
// subprocess (DeterministicRun.Command), which needs either a built
// `goobers` binary on PATH or a GitHub-API-base-URL override neither test
// takes on; open-pr's own behavior is already covered by openpr_test.go.
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
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// tutorWorkflowYAML trims the real selfhost tutor.yaml to the stages
// runnable without a real telemetry-query binary or provider subprocess: a
// manual `goobers run` starts it directly at gather-signals.
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
      next: draft-change
    - name: draft-change
      type: agentic
      goober: config-author
      goal: Implement the analyst's finding and push it to the run's branch.
      capabilities:
        - repo:push
`

// initTutorDemo scaffolds an instance via `goobers init`, swaps the starter
// workflow for the trimmed tutor fixture above, and installs the analyst +
// config-author goobers it references — the tutor analogue of
// acceptance_test.go's initAcceptanceDemo. repoCloneURL points worktrees at
// a local bare git fixture; newAgenticAdapter scripts a fake harness for
// both agentic stages. config-author's fake actually writes a file, commits,
// and pushes it in the run's real worktree (Act's documented contract:
// "simulates whatever side effect the real harness would have had in the
// workspace") — a real, pushed commit in the fixture repo is the concrete
// "config-PR artifact" T1's test plan asks for, stronger proof than a merely
// fabricated ResultEnvelope.
func initTutorDemo(t *testing.T) string {
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
		{"analyst", "analyst", []string{"telemetry:read", "journal:read"}},
		{"config-author", "config-author", []string{"repo:push"}},
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
					if err := tutorFixtureCommitAndPush(req.Workspace); err != nil {
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

// tutorFixtureCommitAndPush simulates config-author's real job — writing a
// config change and pushing it to the run's branch — inside the worktree at
// workspace, so the fixture bare repo ends up with a real, inspectable
// commit standing in for the change a real open-pr stage would turn into a
// PR.
func tutorFixtureCommitAndPush(workspace string) error {
	if err := os.WriteFile(filepath.Join(workspace, "tutor-finding.md"), []byte("# Tutor finding\n\nfixture config change\n"), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"add", "tutor-finding.md"},
		{"commit", "-m", "tutor: fixture config change"},
		// The worktree's origin is a --mirror clone (worktree.Manager's
		// managed shared working copy), which sets remote.origin.mirror=true
		// in that repo's config — an explicit refspec conflicts with that
		// setting unless overridden for this one push.
		{"-c", "remote.origin.mirror=false", "push", "origin", "HEAD"},
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

// tutorFixtureAct is the scripted fake-harness completion payload: both
// agentic stages report success (this fixture has no gate/repass loop).
func tutorFixtureAct(gooberName string) apiv1.ResultEnvelope {
	switch gooberName {
	case "analyst":
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "diagnosed one recurring problem"}
	default:
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "pushed the recommended config change"}
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

// TestTutorProducesConfigChangeArtifact is T1's core acceptance check: a
// manual `goobers run` drives the real runner across
// gather-signals -> analyze -> draft-change, offline, and the journal proves
// the run happened; the fake config-author's real git push into the fixture
// repo stands in for "a run produces a config-PR artifact against a fixture
// config repo."
func TestTutorProducesConfigChangeArtifact(t *testing.T) {
	root := initTutorDemo(t)

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
	if got := countEvents(eventTypes(events), journal.EventStageStarted); got != 3 {
		t.Errorf("stage.started count = %d, want 3 (gather-signals, analyze, draft-change)", got)
	}

	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseCompleted {
		t.Fatalf("state.json phase = %q, want completed", st.Phase)
	}
}

func eventTypes(events []journal.Event) []journal.EventType {
	types := make([]journal.EventType, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}
