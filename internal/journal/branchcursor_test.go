package journal

import (
	"path/filepath"
	"testing"
)

func branchEvent(seq uint64, typ EventType, branch int, parallel, branchName, stage string) Event {
	return Event{
		Schema: EventSchema, Seq: seq, Type: typ, Branch: branch,
		Parallel: parallel, BranchName: branchName, Stage: stage,
	}
}

// A run that never forks must reconstruct to no cursors at all — the whole
// back-compat guarantee of FO-3 rests on this.
func TestReconstructBranchCursorsIgnoresSequentialRun(t *testing.T) {
	events := []Event{
		{Schema: EventSchema, Seq: 1, Type: EventRunStarted},
		{Schema: EventSchema, Seq: 2, Type: EventStageStarted, Stage: "implement"},
		{Schema: EventSchema, Seq: 3, Type: EventStageFinished, Stage: "implement"},
		{Schema: EventSchema, Seq: 4, Type: EventRunFinished},
	}
	if cursors, ok := reconstructBranchCursors(events); ok {
		t.Fatalf("sequential run produced cursors %+v, want none", cursors)
	}
}

// Mid-parallel: cursors reflect each branch's own position, at different depths.
func TestReconstructBranchCursorsMidParallel(t *testing.T) {
	events := []Event{
		{Schema: EventSchema, Seq: 1, Type: EventRunStarted},
		{Schema: EventSchema, Seq: 2, Type: EventParallelStarted, Parallel: "fan"},
		branchEvent(3, EventBranchStarted, 1, "fan", "security", "review-security"),
		branchEvent(4, EventBranchStarted, 2, "fan", "perf", "review-perf"),
		// security advances two stages; perf stays where it started.
		branchEvent(5, EventStageStarted, 1, "fan", "security", "deep-security"),
		branchEvent(6, EventStageFinished, 1, "fan", "security", "deep-security"),
	}
	cursors, ok := reconstructBranchCursors(events)
	if !ok {
		t.Fatal("mid-parallel run should produce cursors")
	}
	if len(cursors) != 2 {
		t.Fatalf("cursors = %+v, want 2", cursors)
	}
	if cursors[0].Branch != 1 || cursors[0].Name != "security" || cursors[0].MachineState != "deep-security" {
		t.Errorf("branch 1 cursor = %+v, want id 1 security at deep-security", cursors[0])
	}
	if cursors[1].Branch != 2 || cursors[1].Name != "perf" || cursors[1].MachineState != "review-perf" {
		t.Errorf("branch 2 cursor = %+v, want id 2 perf at review-perf", cursors[1])
	}
}

// A sibling's interleaved events must never move another branch's cursor —
// the exact failure mode a totally-ordered "last X wins" scan would produce.
func TestReconstructBranchCursorsIgnoresSiblingInterleaving(t *testing.T) {
	interleaved := []Event{
		{Schema: EventSchema, Seq: 1, Type: EventParallelStarted, Parallel: "fan"},
		branchEvent(2, EventBranchStarted, 1, "fan", "a", "a1"),
		branchEvent(3, EventBranchStarted, 2, "fan", "b", "b1"),
		branchEvent(4, EventStageStarted, 2, "fan", "b", "b2"),
		branchEvent(5, EventStageStarted, 1, "fan", "a", "a2"),
		branchEvent(6, EventStageStarted, 2, "fan", "b", "b3"),
	}
	cursors, ok := reconstructBranchCursors(interleaved)
	if !ok {
		t.Fatal("want cursors")
	}
	if cursors[0].MachineState != "a2" {
		t.Errorf("branch a cursor = %q, want a2 (b's later events must not move it)", cursors[0].MachineState)
	}
	if cursors[1].MachineState != "b3" {
		t.Errorf("branch b cursor = %q, want b3", cursors[1].MachineState)
	}
}

// A settled branch keeps its status and carries no resume position.
func TestReconstructBranchCursorsSettledBranch(t *testing.T) {
	events := []Event{
		{Schema: EventSchema, Seq: 1, Type: EventParallelStarted, Parallel: "fan"},
		branchEvent(2, EventBranchStarted, 1, "fan", "a", "a1"),
		branchEvent(3, EventBranchStarted, 2, "fan", "b", "b1"),
		func() Event {
			e := branchEvent(4, EventBranchFinished, 1, "fan", "a", "")
			e.BranchStatus = BranchSucceeded
			return e
		}(),
	}
	cursors, ok := reconstructBranchCursors(events)
	if !ok {
		t.Fatal("want cursors")
	}
	if cursors[0].Status != BranchSucceeded || cursors[0].MachineState != "" {
		t.Errorf("settled branch cursor = %+v, want succeeded with no resume position", cursors[0])
	}
	if cursors[1].Status != "" || cursors[1].MachineState != "b1" {
		t.Errorf("live branch cursor = %+v, want still running at b1", cursors[1])
	}
}

// Once the parallel finishes the run is single-cursor again.
func TestReconstructBranchCursorsAfterParallelFinished(t *testing.T) {
	events := []Event{
		{Schema: EventSchema, Seq: 1, Type: EventParallelStarted, Parallel: "fan"},
		branchEvent(2, EventBranchStarted, 1, "fan", "a", "a1"),
		branchEvent(3, EventBranchStarted, 2, "fan", "b", "b1"),
		{Schema: EventSchema, Seq: 4, Type: EventParallelFinished, Parallel: "fan"},
	}
	if cursors, ok := reconstructBranchCursors(events); ok {
		t.Fatalf("finished parallel produced cursors %+v, want none", cursors)
	}
}

// Recover must heal a checkpoint whose cursors the log contradicts, because
// the log is the source of truth and state.json is derived.
func TestRecoverRebuildsBranchCursorsFromLog(t *testing.T) {
	dir := t.TempDir()
	run, err := Create(dir, RunIdentity{RunID: "0af7651916cd43dd8448eb211c80319c", Workflow: "wf", Gaggle: "g"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []Event{
		{Type: EventParallelStarted, Parallel: "fan"},
		branchEvent(0, EventBranchStarted, 1, "fan", "a", "a1"),
		branchEvent(0, EventBranchStarted, 2, "fan", "b", "b1"),
		branchEvent(0, EventStageStarted, 1, "fan", "a", "a2"),
	} {
		ev.Seq = 0
		if err := run.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	runDir := run.Dir()
	// Strand the checkpoint: no cursors recorded at all.
	run.SetBranchCursors(nil)
	if err := run.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, _, err := Recover(runDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recovered.Close() }()

	cursors := recovered.BranchCursors()
	if len(cursors) != 2 {
		t.Fatalf("recovered cursors = %+v, want 2 rebuilt from the log", cursors)
	}
	if cursors[0].MachineState != "a2" || cursors[1].MachineState != "b1" {
		t.Errorf("recovered cursors = %+v, want a at a2 and b at b1", cursors)
	}

	// The heal must reach state.json too, not just memory.
	rd, err := OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := rd.State()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Branches) != 2 {
		t.Fatalf("state.json branches = %+v, want the healed cursors persisted (%s)", st.Branches, filepath.Join(runDir, fileState))
	}
}

// A pre-FO-3 journal — no parallel events anywhere — must recover exactly as
// it did before, with no cursors invented.
func TestRecoverLeavesSequentialRunSingleCursor(t *testing.T) {
	dir := t.TempDir()
	run, err := Create(dir, RunIdentity{RunID: "1af7651916cd43dd8448eb211c80319c", Workflow: "wf", Gaggle: "g"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runDir := run.Dir()
	if err := run.Append(Event{Type: EventStageStarted, Stage: "implement"}); err != nil {
		t.Fatal(err)
	}
	run.SetMachineState("implement")
	if err := run.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, _, err := Recover(runDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recovered.Close() }()

	if cursors := recovered.BranchCursors(); len(cursors) != 0 {
		t.Fatalf("sequential run recovered with cursors %+v, want none", cursors)
	}
}

// The completeness record is normative and its encoding must be stable and in
// declaration order — sorting it would destroy the ordering that assigns ids.
func TestConformanceProjectsCompletenessInDeclarationOrder(t *testing.T) {
	e := Event{
		Schema: EventSchema, Seq: 1, Type: EventParallelFinished, Parallel: "fan",
		Completeness: []BranchOutcome{
			{Branch: 2, Name: "perf", Status: BranchFailed, Artifacts: 0},
			{Branch: 1, Name: "security", Status: BranchSucceeded, Artifacts: 3},
		},
	}
	got := projectNormative(e).Completeness
	want := "2:perf:failed:0,1:security:succeeded:3"
	if got != want {
		t.Errorf("completeness = %q, want %q (record order preserved, not sorted)", got, want)
	}
}

// Grouping by branch is the comparison surface for a forking run: absolute seq
// is not comparable across branches, but within-branch order is total.
func TestConformanceBranchesGroupsByBranch(t *testing.T) {
	events := []Event{
		{Schema: EventSchema, Seq: 1, Type: EventParallelStarted, Parallel: "fan"},
		branchEvent(2, EventStageStarted, 1, "fan", "a", "a1"),
		branchEvent(3, EventStageStarted, 2, "fan", "b", "b1"),
		branchEvent(4, EventStageFinished, 1, "fan", "a", "a1"),
	}
	byBranch := ConformanceBranches(events)
	if len(byBranch[1]) != 2 {
		t.Errorf("branch 1 events = %d, want 2", len(byBranch[1]))
	}
	if len(byBranch[2]) != 1 {
		t.Errorf("branch 2 events = %d, want 1", len(byBranch[2]))
	}
	if len(byBranch[0]) != 1 {
		t.Errorf("root branch events = %d, want 1 (parallel.started)", len(byBranch[0]))
	}
}
