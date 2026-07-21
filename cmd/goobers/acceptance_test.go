// This file is the V0 acceptance check at the CLI level (issue #30): it drives
// the agentic build loop — implement -> reviewer gate (needs-changes repass) ->
// local-ci — through the real `goobers run` entrypoint (buildRunnerConfig ->
// runner.New -> Start), offline. It extends #29's walking skeleton
// (test/e2e/walking_skeleton_test.go), which proves the same loop at the
// runner *API* level, up to the *CLI* level that the #30 acceptance runbook
// (docs/V0-ACCEPTANCE.md) invokes — closing the gap between #96's daemon tests
// (CLI-level but deterministic-only, no fake-Copilot seam) and #29 (full
// agentic loop but in-process). Two test seams keep it network-free and
// Copilot-free: repoCloneURL (worktrees -> a local bare git fixture) and
// newAgenticAdapter (the harness -> a scripted fake), both in runnerwiring.go.
//
// The provider-backed stages of the real dogfood workflows (backlog-query,
// open-pr, ci-poll, issue-close-out) are wired into the CLI runner now
// (#131/#132) — each has its own CLI-level integration test
// (backlogquery_test.go/openpr_test.go/issuecloseout_test.go) plus a
// runner-level test proving the ci-poll output->input threading
// (internal/runner's TestRunnerThreadsInputsFromUpstreamOutputs) — but they
// are deliberately not chained together into one single mega end-to-end run
// here: open-pr/issue-close-out dispatch as real `goobers <subcommand>` OS
// subprocesses (DeterministicRun.Command, not an in-process seam the way
// implement/review are), so exercising them through the real runner in this
// same test binary would need either a separately built `goobers` binary on
// PATH or a new production-facing GitHub-API-base-URL override added purely
// to make that possible — scope this PR intentionally didn't take on. This
// check still covers the runnable-today agentic core.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
)

// acceptanceWorkflowYAML is the agentic build loop the dogfood implementation
// workflow (selfhost/gaggles/goobers/workflows/implementation.yaml) is built
// around, trimmed to the stages runnable without the not-yet-wired provider
// built-ins (backlog-query/open-pr/ci-poll/issue-close-out): a manual `goobers
// run` starts it directly at `implement`. It mirrors #29's skeletonMachine.
const acceptanceWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: acceptance
spec:
  gaggle: example
  displayName: Acceptance (agentic core - issue #30)
  triggers:
    - type: backlog-item
      selector:
        goobers: "true"
  readiness:
    maxConcurrentRuns: 1
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: implementer
      goal: Implement the claimed issue in the run's worktree.
      capabilities:
        - repo:push
      retry:
        maxAttempts: 2
      next: review
    - name: local-ci
      type: deterministic
      goal: Run the project's local CI-equivalent in the worktree.
      run:
        command: ["true"]
  gates:
    - name: review
      evaluator: agentic
      agentic:
        goober: reviewer
      branches:
        pass: local-ci
        needs-changes: implement
        fail: "@abort"
`

func acceptanceGooberYAML(name, role string, capabilities []string) string {
	caps := "[]"
	if len(capabilities) > 0 {
		caps = "\n"
		for _, c := range capabilities {
			caps += "    - " + c + "\n"
		}
	}
	return `apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: ` + name + `
spec:
  gaggle: example
  role: ` + role + `
  displayName: ` + name + `
  instructions: instructions.md
  harness: copilot
  capabilities: ` + caps + `
  skills:
    - ` + role + `
  tools:
    - shell
  scaleFactor: 1
  workflows:
    - acceptance
`
}

// initAcceptanceDemo scaffolds an instance via `goobers init`, swaps the
// starter's single-goober workflow for the agentic acceptance loop above, and
// installs the implementer + reviewer goobers it references — the agentic
// analogue of daemon_test.go's initDeterministicDemo. It arms both test seams:
// repoCloneURL points worktrees at a local bare git fixture, and
// newAgenticAdapter scripts the fake harness (implementer: success; reviewer:
// needs-changes on the first pass, then approve), restored via t.Cleanup.
func initAcceptanceDemo(t *testing.T) string {
	t.Helper()
	// The starter instance.yaml grants repo:push from a token read out of this
	// env var; the fake harness never uses it, but the credential injector
	// materializes (and the journal scrubs) it, so a dummy value keeps the real
	// injection + redaction path in the loop without a real token or network.
	t.Setenv("GOOBERS_GITHUB_TOKEN", "ghp_acceptance_fixture_dummy_token")
	root := initDemo(t)

	gaggleDir := filepath.Join(root, "config", "gaggles", "example")
	// Replace the starter workflow + coder goober with the acceptance loop.
	if err := os.RemoveAll(filepath.Join(gaggleDir, "workflows")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(gaggleDir, "goobers")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(gaggleDir, "workflows", "acceptance.yaml"), acceptanceWorkflowYAML)
	for _, g := range []struct {
		name, role string
		caps       []string
	}{
		{"implementer", "implementer", []string{"repo:push"}},
		{"reviewer", "reviewer", nil},
	} {
		dir := filepath.Join(gaggleDir, "goobers", g.name)
		writeFixture(t, filepath.Join(dir, "goober.yaml"), acceptanceGooberYAML(g.name, g.role, g.caps))
		writeFixture(t, filepath.Join(dir, "instructions.md"), "You are the "+g.name+" fixture goober for the #30 acceptance check.\n")
		writeFixture(t, filepath.Join(dir, "assets", "identity.txt"), g.name)
	}

	fixtureRepo := newDaemonFixtureRepo(t)
	prevRepo := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil }

	// Per-goober call counts live in test scope (not inside the returned
	// adapter) so the reviewer's needs-changes -> pass progression is robust to
	// how often the runner reconstructs the adapter, mirroring #29's coderAct/
	// reviewerAct(callNum) contract.
	var mu sync.Mutex
	calls := map[string]int{}
	prevAdapter := newAgenticAdapter
	newAgenticAdapter = func(gooberName string, _ map[string]string) harness.Adapter {
		return &harness.FakeAdapter{
			Transcript: []byte("fake harness session for " + gooberName + "\n"),
			Act: func(_ context.Context, req harness.RunRequest) error {
				asset, err := os.ReadFile(filepath.Join(req.Workspace, ".goober-assets", "identity.txt"))
				if err != nil {
					return err
				}
				if string(asset) != gooberName {
					return fmt.Errorf("goober asset = %q, want %q", asset, gooberName)
				}
				mu.Lock()
				calls[gooberName]++
				n := calls[gooberName]
				mu.Unlock()
				payload := acceptanceAct(gooberName, n)
				// The implementer's deliverable is a committed diff on the run
				// branch; since #415 an empty diff fast-fails at the review gate
				// before the reviewer runs. Commit a change on a successful
				// implement result — unique per call so the repass produces a
				// different diff (also clearing the #316 identical-diff guard).
				// The reviewer commits nothing.
				if gooberName != "reviewer" {
					if env, ok := payload.(apiv1.ResultEnvelope); ok && env.Status == apiv1.ResultSuccess {
						if err := commitFixtureChange(req.Workspace, n); err != nil {
							return err
						}
					}
				}
				return harness.WriteCompletion(req.Workspace, req.CompletionPath, payload)
			},
		}
	}

	t.Cleanup(func() {
		repoCloneURL = prevRepo
		newAgenticAdapter = prevAdapter
	})
	return root
}

// acceptanceAct is the scripted fake-harness behaviour: the reviewer requests
// changes on its first evaluation (driving one repass back to implement) then
// approves; every other goober (the implementer) reports success. It returns
// the completion payload the harness writes to the run's CompletionPath — a
// Verdict for the reviewer gate, a ResultEnvelope for a task.
func acceptanceAct(gooberName string, call int) interface{} {
	if gooberName == "reviewer" {
		if call == 1 {
			return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Rationale: "add a test for the new branch"}
		}
		return apiv1.Verdict{Decision: apiv1.VerdictPass, Rationale: "looks good"}
	}
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Summary: "implemented",
		Outputs: map[string]interface{}{"changedFileCount": 1},
	}
}

// commitFixtureChange commits a unique change to the run branch in workspace,
// standing in for the implementer's real committed diff so the review gate has
// a non-empty diff to evaluate (an empty diff fast-fails since #415). Explicit
// -c identity keeps it independent of any ambient git config on the runner.
func commitFixtureChange(workspace string, call int) error {
	if err := os.WriteFile(filepath.Join(workspace, "impl.txt"), []byte(fmt.Sprintf("coder change %d\n", call)), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-m", fmt.Sprintf("coder impl %d", call)},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = workspace
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w\n%s", args, err, out)
		}
	}
	return nil
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAcceptanceAgenticLoopViaCLI is issue #30's executable acceptance check
// for the runnable-today agentic core: `goobers run` drives a single item
// through the REAL runner across the implement -> reviewer-gate(repass) ->
// local-ci loop, offline, and the on-disk journal proves the loop happened —
// asserting on the journal (not runner internals), the same contract #29
// asserts one layer down. This is the CLI-level bridge the #30 runbook's Run/
// Observe steps invoke.
func TestAcceptanceAgenticLoopViaCLI(t *testing.T) {
	root := initAcceptanceDemo(t)

	code, stdout, stderr := runArgs(t, "run", "acceptance", root)
	if code != 0 {
		t.Fatalf("goobers run acceptance: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "phase=completed") {
		t.Fatalf("expected the real runner to complete the agentic loop via the CLI, stdout = %q", stdout)
	}

	// Recover the run id the CLI generated, to open its journal.
	runID := runIDFromRunStdout(t, stdout)
	rd, err := journal.OpenRead(filepath.Join(root, "runs", runID))
	if err != nil {
		t.Fatalf("OpenRead(%s): %v", runID, err)
	}

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

	// The agentic loop actually ran through the CLI: the reviewer gate
	// evaluated twice (needs-changes then pass), implement ran twice, local-ci
	// ran once, and the fake harness left a real transcript span + artifacts —
	// none of which the deterministic-only daemon tests reach.
	if got := countEvents(types, journal.EventGateEvaluated); got != 2 {
		t.Errorf("gate.evaluated count = %d, want 2 (needs-changes then pass)", got)
	}
	if got := countEvents(types, journal.EventStageStarted); got != 3 {
		t.Errorf("stage.started count = %d, want 3 (implement x2, local-ci x1)", got)
	}
	if !sawSpan {
		t.Error("expected at least one span.recorded event (the fake harness transcript) — the agentic path was not exercised")
	}
	if !sawArtifact {
		t.Error("expected at least one artifact.recorded event")
	}
	if len(types) == 0 || types[0] != journal.EventRunStarted || types[len(types)-1] != journal.EventRunFinished {
		t.Errorf("event sequence must start with run.started and end with run.finished, got %v", types)
	}

	// Terminal state, and every recorded artifact/span round-trips (the same
	// resolution a downstream stage or the portal would do) — #29's convention.
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseCompleted || st.MachineState != "" {
		t.Fatalf("state.json = %+v, want completed with empty machineState", st)
	}
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

	// `goobers status` sees the completed run (the runbook's Observe step).
	code, statusOut, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("goobers status: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(statusOut, runID) {
		t.Errorf("status output %q does not mention run %s", statusOut, runID)
	}
}

// runIDFromRunStdout extracts the run id from `goobers run`'s "created run
// <id> (...)" line.
func runIDFromRunStdout(t *testing.T, stdout string) string {
	t.Helper()
	const marker = "created run "
	i := strings.Index(stdout, marker)
	if i < 0 {
		t.Fatalf("no %q line in run stdout: %q", marker, stdout)
	}
	rest := stdout[i+len(marker):]
	if j := strings.IndexAny(rest, " \n"); j >= 0 {
		return rest[:j]
	}
	t.Fatalf("could not parse run id from %q", stdout)
	return ""
}

func countEvents(types []journal.EventType, typ journal.EventType) int {
	n := 0
	for _, ty := range types {
		if ty == typ {
			n++
		}
	}
	return n
}
