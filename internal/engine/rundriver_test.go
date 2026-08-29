package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
)

// TestEngineRunPinsEngineDriverInRunYAML walks the pinning path the daemon
// actually reads — newRunJournal -> JournalProjection.Identity -> ProjectRun
// -> run.yaml — and asserts the file names its driver.
//
// run.yaml is the far side that matters. cmd/goobers' resume scan, stall
// sweep and operator paths open the run directory and read the identity;
// nothing else about an engine-authored journal distinguishes it from a
// runner-authored one (the WF-016 pins, the definition snapshot and the
// event shapes are deliberately identical), so this field is the only thing
// standing between a goobers-api restart and a second in-process driver for
// a run the worker is still executing.
func TestEngineRunPinsEngineDriverInRunYAML(t *testing.T) {
	in := projectionInput("run-driver-projected", crSpec("implement",
		[]apiv1.Task{crTask("implement", "")}, nil))

	proj := executeForProjection(t, in, &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)

	if proj.Identity.Driver != journal.DriverEngine {
		t.Fatalf("projection identity Driver = %q, want %q", proj.Identity.Driver, journal.DriverEngine)
	}

	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("open projected journal: %v", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatalf("read run.yaml identity: %v", err)
	}
	if !identity.EngineDriven() {
		t.Fatalf("run.yaml identity Driver = %q, want %q — the daemon's resume scan would treat this as its own run and walk it a second time",
			identity.Driver, journal.DriverEngine)
	}

	// The daemon reads the serialized file, not the in-memory struct.
	raw, err := os.ReadFile(filepath.Join(dir, "run.yaml"))
	if err != nil {
		t.Fatalf("read run.yaml: %v", err)
	}
	if !strings.Contains(string(raw), "driver: engine") {
		t.Fatalf("run.yaml on disk does not carry driver: engine:\n%s", raw)
	}
}

// TestLiveJournalRunYAMLPinsEngineDriverBeforeTheRunCloses is the verified
// hazard's exact shape: `goobers engine-start --live-journal` authors the run
// journal on the daemon's own PVC while the workflow executes, so a
// goobers-api restart mid-run scans that directory. The marker must therefore
// be in run.yaml from the FIRST emit (the live writer's OpenHeader carries
// the identity), not stamped in later by the projection writer at close —
// otherwise the run is indistinguishable from a runner-driven one during
// exactly the window the restart happens in.
func TestLiveJournalRunYAMLPinsEngineDriverBeforeTheRunCloses(t *testing.T) {
	writer, runsDir := newLiveWriter(t)
	spec := crSpec("implement",
		[]apiv1.Task{crTask("implement", "review")},
		[]apiv1.Gate{crGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})})
	in := projectionInput("run-driver-live", spec)
	in.LiveJournal = true

	// The peek reads run.yaml from the live directory WHILE the stage runs —
	// the mid-flight observer standing in for the restarted daemon's scan.
	peek := &runYAMLPeekRunner{dir: filepath.Join(runsDir, in.RunID)}
	executeLive(t, in, &Activities{
		Det:        peek,
		Auto:       gate.NewAutomatedEvaluator(),
		Workspaces: testWorkspaces(t),
		Journal:    writer,
	}, false)

	if !peek.peeked {
		t.Fatal("stage executor never ran, so nothing observed the live run.yaml")
	}
	if peek.err != nil {
		t.Fatalf("mid-run run.yaml peek: %v", peek.err)
	}
	if !peek.identity.EngineDriven() {
		t.Fatalf("mid-run run.yaml Driver = %q, want %q — a daemon restarting at this instant would resume the run in-process while the worker keeps driving it",
			peek.identity.Driver, journal.DriverEngine)
	}
}

// runYAMLPeekRunner reads the live run's run.yaml identity from inside a
// stage, mirroring journalPeekRunner's mid-flight observation of events.
type runYAMLPeekRunner struct {
	dir string

	peeked   bool
	identity journal.RunIdentity
	err      error
}

func (p *runYAMLPeekRunner) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	p.peeked = true
	rd, err := journal.OpenRead(p.dir)
	if err != nil {
		p.err = err
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	p.identity, p.err = rd.Identity()
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

// TestEngineRunPinsGateGooberCapabilitiesForTheDaemonToReadBack is the far
// side of `goobers engine-start`'s gate-goober pin (#294/#3528): the map the
// starter resolves has to land in the run's immutable input snapshot, because
// that snapshot — not the currently-served config — is what the daemon's
// credential plane reads when a gate stage needs its reviewer's grants. The
// reader here is the production one (runner.PinnedGateGooberCapabilities), so
// the assertion is about the bytes the credential plane will actually see.
func TestEngineRunPinsGateGooberCapabilitiesForTheDaemonToReadBack(t *testing.T) {
	in := projectionInput("run-gate-caps", crSpec("implement",
		[]apiv1.Task{crTask("implement", "")}, nil))
	in.GateGooberCapabilities = map[string][]string{
		"reviewer": {"repo:read", "model:invoke"},
	}

	proj := executeForProjection(t, in, &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)
	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("open projected journal: %v", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatalf("read run.yaml identity: %v", err)
	}
	pinned, found, err := runner.PinnedGateGooberCapabilities(reader, identity)
	if err != nil {
		t.Fatalf("PinnedGateGooberCapabilities: %v", err)
	}
	if !found {
		t.Fatal("run pinned no gate-goober capability snapshot; the credential plane's gate branch would resolve no reviewer grants at all")
	}
	if got := pinned["reviewer"]; len(got) != 2 || got[0] != "repo:read" || got[1] != "model:invoke" {
		t.Fatalf("pinned reviewer capabilities = %v, want [repo:read model:invoke]", got)
	}
}
