package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/workflow"
)

// liveEscalationReads builds the daemon-shaped reader: a readservice.Local
// whose list path is read.db, with every run directory already projected.
func liveEscalationReads(t *testing.T, layout instance.Layout) *readservice.Local {
	t.Helper()
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dirs, err := layout.RunDirs()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				if err := store.ProjectRunDir(context.Background(), filepath.Join(dir, entry.Name())); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	reads, err := readservice.NewLocal(readservice.LocalSources{Layout: layout, Definitions: &instance.ConfigSet{Manifest: &apiv1.Manifest{}}, ReadModel: store}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return reads
}

// spyEscalationReads records the list options and delegates to the live reader.
type spyEscalationReads struct {
	*readservice.Local
	mu    sync.Mutex
	lists []readservice.RunListOptions
}

func (s *spyEscalationReads) ListRuns(ctx context.Context, o readservice.RunListOptions) (readservice.RunList, error) {
	s.mu.Lock()
	s.lists = append(s.lists, o)
	s.mu.Unlock()
	return s.Local.ListRuns(ctx, o)
}

func seedLiveEscalationFixture(t *testing.T) instance.Layout {
	t.Helper()
	layout := crossRunTestLayout(t)
	askingRun, err := journal.Create(layout.ForGaggle(crossRunTestGaggle).RunsDir(), journal.RunIdentity{
		RunID: "asking-run", Workflow: "implementation", WorkflowVersion: 1, Gaggle: crossRunTestGaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := askingRun.Close(); err != nil {
		t.Fatal(err)
	}
	seedEscalatedRun(t, layout, crossRunTestGaggle, "escalated-a", "501")
	seedEscalatedRun(t, layout, crossRunTestGaggle, "escalated-b", "502")
	seedEscalatedRun(t, layout, "other-gaggle", "escalated-theirs", "999")
	seedBranchOwningRun(t, layout, crossRunTestGaggle, "completed-run", "implementation", "goobers/x")
	return layout
}

// The daemon route must list escalated runs through the live read model, scoped
// to the gaggle by phase, and never through the offline journal scan.
func TestDaemonEscalationCandidatesUsesLiveReadModel(t *testing.T) {
	layout := seedLiveEscalationFixture(t)
	spy := &spyEscalationReads{Local: liveEscalationReads(t, layout)}

	// Corrupt a NON-escalated run's journal after projection. The offline scan
	// opens every run journal and would fail on it; the read-model list never
	// opens it.
	if err := os.WriteFile(filepath.Join(layout.ForGaggle(crossRunTestGaggle).RunsDir(), "completed-run", "run.yaml"), []byte("::not yaml::"), 0o644); err != nil {
		t.Fatal(err)
	}

	service := newDaemonRunJournalService(layout, nil)
	service.reads = spy
	ask := journalclient.EscalationCandidatesRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle}
	response, err := service.EscalationCandidates(context.Background(), ask)
	if err != nil {
		t.Fatalf("escalation candidates: %v", err)
	}
	if len(response.Candidates) != 2 || response.Candidates[0].ParentID != "501" || response.Candidates[1].ParentID != "502" {
		t.Fatalf("candidates = %+v, want parents 501 then 502", response.Candidates)
	}
	if len(spy.lists) == 0 {
		t.Fatal("the live read model was never asked to list runs")
	}
	for _, o := range spy.lists {
		if o.Phase != journal.PhaseEscalated || o.Gaggle != crossRunTestGaggle {
			t.Fatalf("list options = %+v, want phase escalated scoped to %s", o, crossRunTestGaggle)
		}
	}

	// Control: the offline scan really does trip over that journal.
	if _, err := journalclient.NewFileCrossRun(layout).EscalationCandidates(context.Background(), ask); err == nil {
		t.Fatal("control failed: the offline scan did not touch the corrupted run")
	}
}

// The live path and the file-backed path must select exactly the same
// candidates, in the same order.
func TestDaemonEscalationCandidatesParityWithFileCrossRun(t *testing.T) {
	layout := seedLiveEscalationFixture(t)
	service := newDaemonRunJournalService(layout, nil)
	service.reads = liveEscalationReads(t, layout)
	ask := journalclient.EscalationCandidatesRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle}

	live, err := service.EscalationCandidates(context.Background(), ask)
	if err != nil {
		t.Fatal(err)
	}
	file, err := journalclient.NewFileCrossRun(layout).EscalationCandidates(context.Background(), ask)
	if err != nil {
		t.Fatal(err)
	}
	// Same instants; the read model hands back UTC, the journal its local zone.
	for i := range file {
		file[i].StartedAt = file[i].StartedAt.UTC()
	}
	for i := range live.Candidates {
		live.Candidates[i].StartedAt = live.Candidates[i].StartedAt.UTC()
	}
	if len(file) == 0 || !reflect.DeepEqual(live.Candidates, file) {
		t.Fatalf("live = %+v\nfile = %+v", live.Candidates, file)
	}
}

func TestDaemonEscalationCandidatesRefusesWithoutLiveReads(t *testing.T) {
	layout := seedLiveEscalationFixture(t)
	service := newDaemonRunJournalService(layout, nil)
	if _, err := service.EscalationCandidates(context.Background(), journalclient.EscalationCandidatesRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle}); err == nil {
		t.Fatal("daemon served escalation candidates without a live read model (silent offline fallback)")
	}
}

// --- conflict-touches / unpushed-work / run-phase / branch-ownership (#6968) ---

// seedLiveAskingRun creates a schema-valid asking run the read model can project.
func seedLiveAskingRun(t *testing.T, layout instance.Layout) {
	t.Helper()
	run, err := journal.Create(layout.ForGaggle(crossRunTestGaggle).RunsDir(), journal.RunIdentity{
		RunID: "asking-run", Workflow: "implementation", WorkflowVersion: 1, Gaggle: crossRunTestGaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

// staleClock stamps a run's events well before any window the tests ask about.
func staleClock() journal.Option {
	at := time.Now().Add(-72 * time.Hour)
	return journal.WithClock(func() time.Time { at = at.Add(time.Second); return at })
}

// corruptJournal appends an unparseable line, so any reader that opens and
// decodes the run's events fails on it.
func corruptJournal(t *testing.T, layout instance.Layout, gaggle, runID string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(layout.ForGaggle(gaggle).RunsDir(), runID, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertWindowedLists(t *testing.T, spy *spyEscalationReads) {
	t.Helper()
	if len(spy.lists) == 0 {
		t.Fatal("the live read model was never asked to list runs")
	}
	for _, o := range spy.lists {
		if o.Gaggle != crossRunTestGaggle || o.Since.IsZero() || !o.OrderByActivity {
			t.Fatalf("list options = %+v, want a gaggle-scoped, windowed, activity-ordered query", o)
		}
	}
}

func seedConflictFixture(t *testing.T) instance.Layout {
	t.Helper()
	layout := crossRunTestLayout(t)
	seedLiveAskingRun(t, layout)
	seedConflictRunWith(t, layout, crossRunTestGaggle, "conflict-a", "internal/a.go", false)
	seedConflictRunWith(t, layout, crossRunTestGaggle, "conflict-b", "internal/b.go", false)
	seedConflictRunWith(t, layout, crossRunTestGaggle, "conflict-old", "internal/old.go", false, staleClock())
	seedConflictRunWith(t, layout, "other-gaggle", "conflict-theirs", "internal/theirs.go", false)
	return layout
}

func TestDaemonConflictTouchesUsesLiveReadModel(t *testing.T) {
	layout := seedConflictFixture(t)
	spy := &spyEscalationReads{Local: liveEscalationReads(t, layout)}
	service := newDaemonRunJournalService(layout, nil)
	service.reads = spy
	service.definitions = crossRunTestDefinitions(t)
	since := time.Now().UTC().Add(-24 * time.Hour)
	ask := journalclient.ConflictTouchRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since}

	// Parity first, over several windows (all, recent only, none).
	for _, window := range []time.Duration{-96 * time.Hour, -24 * time.Hour, -time.Minute, time.Hour} {
		ask.Since = time.Now().UTC().Add(window)
		live, err := service.ConflictTouches(context.Background(), ask)
		if err != nil {
			t.Fatalf("window %v: %v", window, err)
		}
		file, err := journalclient.NewFileCrossRun(layout).ConflictTouches(context.Background(), ask)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(live.Touches, file) {
			t.Fatalf("window %v: live = %+v, file = %+v", window, live.Touches, file)
		}
	}
	ask.Since = since
	live, err := service.ConflictTouches(context.Background(), ask)
	if err != nil || len(live.Touches) != 2 || live.Touches[0].RunID != "conflict-a" || live.Touches[1].RunID != "conflict-b" {
		t.Fatalf("touches = %+v, err = %v; want conflict-a then conflict-b", live.Touches, err)
	}
	assertWindowedLists(t, spy)

	// A run with no activity inside the window is never opened: corrupt its
	// journal and the daemon route still answers, while the directory scan fails.
	corruptJournal(t, layout, crossRunTestGaggle, "conflict-old")
	again, err := service.ConflictTouches(context.Background(), ask)
	if err != nil || !reflect.DeepEqual(again.Touches, live.Touches) {
		t.Fatalf("after corrupting a non-candidate: %+v, %v", again.Touches, err)
	}
	if _, err := journalclient.NewFileCrossRun(layout).ConflictTouches(context.Background(), ask); err == nil {
		t.Fatal("control failed: the directory scan did not touch the corrupted run")
	}
}

func TestDaemonUnpushedWorkUsesLiveReadModel(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedLiveAskingRun(t, layout)
	seedStrandedDiffRunWith(t, layout, crossRunTestGaggle, "prior-1", "42", "first diff for item 42", false)
	seedStrandedDiffRunWith(t, layout, crossRunTestGaggle, "prior-2", "42", "second diff for item 42", false)
	seedStrandedDiffRunWith(t, layout, crossRunTestGaggle, "prior-old", "42", "ancient diff", false, staleClock())
	seedStrandedDiffRunWith(t, layout, crossRunTestGaggle, "other-item", "99", "diff for item 99", false)
	seedStrandedDiffRunWith(t, layout, "other-gaggle", "theirs", "42", "foreign diff", false)
	claimItemForRun(t, layout, "42", "asking-run")

	spy := &spyEscalationReads{Local: liveEscalationReads(t, layout)}
	service := newDaemonRunJournalService(layout, nil)
	service.reads = spy
	service.definitions = crossRunTestDefinitions(t)
	ask := journalclient.UnpushedWorkRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle}

	for _, window := range []time.Duration{-96 * time.Hour, -24 * time.Hour, time.Hour} {
		ask.Since = time.Now().UTC().Add(window)
		live, err := service.UnpushedWork(context.Background(), ask)
		if err != nil {
			t.Fatalf("window %v: %v", window, err)
		}
		fileAsk := ask
		fileAsk.ItemIDs = []string{"42"}
		file, err := journalclient.NewFileCrossRun(layout).UnpushedWork(context.Background(), fileAsk)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(live.Work, file) {
			t.Fatalf("window %v: live = %+v, file = %+v", window, live.Work, file)
		}
	}
	ask.Since = time.Now().UTC().Add(-24 * time.Hour)
	live, err := service.UnpushedWork(context.Background(), ask)
	if err != nil || live.Work == nil || (live.Work.RunID != "prior-1" && live.Work.RunID != "prior-2") {
		t.Fatalf("work = %+v, err = %v; want a fresh prior run for item 42", live.Work, err)
	}
	assertWindowedLists(t, spy)

	corruptJournal(t, layout, crossRunTestGaggle, "prior-old")
	corruptJournal(t, layout, crossRunTestGaggle, "other-item") // fresh, so it IS a candidate: skipped with a warning, not fatal
	again, err := service.UnpushedWork(context.Background(), ask)
	if err != nil || !reflect.DeepEqual(again.Work, live.Work) {
		t.Fatalf("after corrupting runs: %+v, %v", again.Work, err)
	}
}

// The unpushed-work scan reports a failure it cannot localise; prove the
// non-candidate corruption above is irrelevant by making the offline scan
// observe it (it warns on every unreadable run it opens).
func TestDaemonUnpushedWorkDoesNotOpenNonCandidateJournals(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedLiveAskingRun(t, layout)
	seedStrandedDiffRunWith(t, layout, crossRunTestGaggle, "prior", "42", "diff", false)
	seedStrandedDiffRunWith(t, layout, crossRunTestGaggle, "prior-old", "42", "ancient", false, staleClock())
	claimItemForRun(t, layout, "42", "asking-run")
	reads := liveEscalationReads(t, layout)
	corruptJournal(t, layout, crossRunTestGaggle, "prior-old")
	workflows := []string{"implementation"}

	var warnings []string
	file := journalclient.NewFileCrossRun(layout)
	file.Warn = func(msg string) { warnings = append(warnings, msg) }
	ask := journalclient.UnpushedWorkRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, ItemIDs: []string{"42"}, Since: time.Now().Add(-24 * time.Hour)}

	if _, err := file.UnpushedWorkFromReads(context.Background(), reads, workflows, ask); err != nil || len(warnings) != 0 {
		t.Fatalf("narrowed scan: err=%v warnings=%v; want none (the corrupt run is outside the window)", err, warnings)
	}
	if _, err := file.UnpushedWork(context.Background(), ask); err != nil || len(warnings) == 0 {
		t.Fatalf("control: err=%v warnings=%v; the directory scan should have hit the corrupt run", err, warnings)
	}
}

func TestDaemonWindowedRoutesRefuseWithoutLiveReadsOrWindow(t *testing.T) {
	layout := seedConflictFixture(t)
	seedStrandedDiffRunWith(t, layout, crossRunTestGaggle, "prior", "42", "diff", false)
	claimItemForRun(t, layout, "42", "asking-run")
	service := newDaemonRunJournalService(layout, nil)
	service.definitions = crossRunTestDefinitions(t)
	since := time.Now().Add(-time.Hour)

	if _, err := service.ConflictTouches(context.Background(), journalclient.ConflictTouchRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since}); err == nil {
		t.Fatal("conflict touches served without a live read model (silent offline fallback)")
	}
	if _, err := service.UnpushedWork(context.Background(), journalclient.UnpushedWorkRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since}); err == nil {
		t.Fatal("unpushed work served without a live read model (silent offline fallback)")
	}
	service.reads = liveEscalationReads(t, layout)
	service.definitions = nil
	if _, err := service.ConflictTouches(context.Background(), journalclient.ConflictTouchRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since}); err == nil {
		t.Fatal("conflict touches served without workflow definitions (unfiltered scan)")
	}
	if _, err := service.UnpushedWork(context.Background(), journalclient.UnpushedWorkRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since}); err == nil {
		t.Fatal("unpushed work served without workflow definitions (unfiltered scan)")
	}
	service.definitions = crossRunTestDefinitions(t)
	if _, err := service.ConflictTouches(context.Background(), journalclient.ConflictTouchRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle}); err == nil {
		t.Fatal("an unbounded conflict-history read was served")
	}
	if _, err := service.UnpushedWork(context.Background(), journalclient.UnpushedWorkRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle}); err == nil {
		t.Fatal("an unbounded unpushed-work read was served")
	}
}

// RunPhase and BranchOwnership were reviewed for the same defect (#6968): both
// resolve one run directory by id and read that single journal. Corrupting an
// unrelated run proves neither enumerates the others, and the answers match
// the file-backed reader.
func TestDaemonRunPhaseAndBranchOwnershipReadOnlyTheTargetJournal(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedLiveAskingRun(t, layout)
	seedBranchOwningRun(t, layout, crossRunTestGaggle, "owner", "implementation", "goobers/implementation/owner")
	seedBranchOwningRun(t, layout, crossRunTestGaggle, "bystander", "implementation", "goobers/implementation/bystander")
	corruptJournal(t, layout, crossRunTestGaggle, "bystander")
	service := newDaemonRunJournalService(layout, nil)
	ctx := context.Background()
	file := journalclient.NewFileCrossRun(layout)

	phase, err := service.RunPhase(ctx, journalclient.RunPhaseRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, TargetRunID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	wantPhase, err := file.RunPhase(ctx, "owner")
	if err != nil || phase.Phase != string(wantPhase) || phase.RunID != "owner" {
		t.Fatalf("phase = %+v, file = %q, %v", phase, wantPhase, err)
	}

	req := journalclient.BranchOwnershipRequest{
		RunID: "asking-run", Gaggle: crossRunTestGaggle, TargetRunID: "owner",
		Workflow: "implementation", Branch: "goobers/implementation/owner",
	}
	got, err := service.BranchOwnership(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	want, err := file.BranchOwnership(ctx, req)
	if err != nil || got.Owner == nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ownership = %+v, file = %+v, %v", got, want, err)
	}
}

// crossRunTestDefinitions is the daemon's definition registry for the windowed
// routes' workflow filter (#6968): "implementation" can both strand a diff
// (writable agentic task) and record a base-sync conflict (syncBase task);
// "remediation" can only strand a diff; "merge-review" and "researcher" can
// neither (an agentic gate and a read-only agentic task).
func crossRunTestDefinitions(t *testing.T) *interventionDefinitionRegistry {
	t.Helper()
	agentic := func(name string, mode apiv1.WorkspaceMode, next string) apiv1.Task {
		return apiv1.Task{Name: name, Type: apiv1.TaskAgentic, Goober: "dev", Goal: name, Workspace: mode, Next: next}
	}
	deterministic := func(name string, syncBase bool) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, SyncBase: syncBase}, Next: workflow.TerminalComplete,
		}
	}
	specs := map[string]apiv1.WorkflowSpec{
		"implementation": {Start: "implement", Tasks: []apiv1.Task{agentic("implement", "", "sync-base"), deterministic("sync-base", true)}},
		"remediation":    {Start: "remediate", Tasks: []apiv1.Task{agentic("remediate", apiv1.WorkspaceRepo, workflow.TerminalComplete)}},
		"researcher":     {Start: "research", Tasks: []apiv1.Task{agentic("research", apiv1.WorkspaceRepoReadOnly, workflow.TerminalComplete)}},
		"merge-review": {
			Start: "select", Tasks: []apiv1.Task{func() apiv1.Task { task := deterministic("select", false); task.Next = "review"; return task }()},
			Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer", Workspace: apiv1.WorkspaceRepo},
				Branches: map[string]string{"pass": workflow.TerminalComplete, "fail": workflow.TargetAbort, "needs-changes": workflow.TargetAbort},
			}},
		},
	}
	machines := make(map[localscheduler.WorkflowIdentity]*workflow.Machine, len(specs))
	for name, spec := range specs {
		spec.Gaggle = crossRunTestGaggle
		spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}
		machine, err := workflow.Compile(workflow.Definition{Name: name, Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
		if err != nil {
			t.Fatalf("compile %s: %v", name, err)
		}
		machines[localscheduler.WorkflowIdentity{Gaggle: crossRunTestGaggle, Workflow: name}] = machine
	}
	return newInterventionDefinitionRegistry(interventionDefinitionSet{machines: machines})
}

// seedBystanderRuns writes a schema-valid, projectable run of workflow whose
// journal is corrupted afterwards: any reader that opens it fails.
func seedBystanderRuns(t *testing.T, layout instance.Layout, workflowName string, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s-%03d", workflowName, i)
		run, err := journal.Create(layout.ForGaggle(crossRunTestGaggle).RunsDir(), journal.RunIdentity{
			RunID: id, Workflow: workflowName, WorkflowVersion: 1, Gaggle: crossRunTestGaggle,
			Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// Runs of workflows that cannot record the artifacts a route reads are never
// listed as candidates and never opened, however many are recently active, and
// the answers stay equal to the full directory scan (#6968 follow-up).
func TestDaemonWindowedRoutesConsiderOnlyContributingWorkflows(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedLiveAskingRun(t, layout)
	seedConflictRunWith(t, layout, crossRunTestGaggle, "conflict-a", "internal/a.go", false)
	seedStrandedDiffRunWith(t, layout, crossRunTestGaggle, "prior-1", "42", "diff for item 42", false)
	seedStrandedDiffRunWith(t, layout, crossRunTestGaggle, "prior-old", "42", "ancient", false, staleClock())
	claimItemForRun(t, layout, "42", "asking-run")
	var bystanders []string
	for _, name := range []string{"merge-review", "researcher"} {
		bystanders = append(bystanders, seedBystanderRuns(t, layout, name, 30)...)
	}

	spy := &spyEscalationReads{Local: liveEscalationReads(t, layout)}
	service := newDaemonRunJournalService(layout, nil)
	service.reads = spy
	service.definitions = crossRunTestDefinitions(t)
	for _, id := range bystanders {
		corruptJournal(t, layout, crossRunTestGaggle, id)
	}
	ctx := context.Background()
	file := journalclient.NewFileCrossRun(layout)

	since := time.Now().UTC().Add(-24 * time.Hour)
	conflicts, err := service.ConflictTouches(ctx, journalclient.ConflictTouchRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since})
	if err != nil || len(conflicts.Touches) != 1 || conflicts.Touches[0].RunID != "conflict-a" {
		t.Fatalf("conflict touches = %+v, err = %v; want conflict-a despite 60 corrupt bystanders", conflicts.Touches, err)
	}
	assertListedOnly(t, spy, "implementation")
	spy.lists = nil
	work, err := service.UnpushedWork(ctx, journalclient.UnpushedWorkRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since})
	if err != nil || work.Work == nil || work.Work.RunID != "prior-1" {
		t.Fatalf("unpushed work = %+v, err = %v; want prior-1 despite 60 corrupt bystanders", work.Work, err)
	}
	assertListedOnly(t, spy, "implementation", "remediation")

	// Control: the directory scan does open the bystanders and trips on them.
	if _, err := file.ConflictTouches(ctx, journalclient.ConflictTouchRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since}); err == nil {
		t.Fatal("control failed: the directory scan never opened a corrupted bystander")
	}
}

// With the bystanders intact, the filtered daemon answers equal the directory
// scan's over several windows.
func TestDaemonWindowedRoutesParityWithBystanderWorkflows(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedLiveAskingRun(t, layout)
	seedConflictRunWith(t, layout, crossRunTestGaggle, "conflict-a", "internal/a.go", false)
	seedConflictRunWith(t, layout, crossRunTestGaggle, "conflict-b", "internal/b.go", false)
	seedStrandedDiffRunWith(t, layout, crossRunTestGaggle, "prior-1", "42", "diff for item 42", false)
	seedBystanderRuns(t, layout, "merge-review", 25)
	claimItemForRun(t, layout, "42", "asking-run")
	service := newDaemonRunJournalService(layout, nil)
	service.reads = liveEscalationReads(t, layout)
	service.definitions = crossRunTestDefinitions(t)
	file := journalclient.NewFileCrossRun(layout)
	ctx := context.Background()
	for _, window := range []time.Duration{-96 * time.Hour, -24 * time.Hour, time.Hour} {
		since := time.Now().UTC().Add(window)
		gotC, err := service.ConflictTouches(ctx, journalclient.ConflictTouchRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since})
		if err != nil {
			t.Fatal(err)
		}
		wantC, err := file.ConflictTouches(ctx, journalclient.ConflictTouchRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since})
		if err != nil || !reflect.DeepEqual(gotC.Touches, wantC) {
			t.Fatalf("window %v: conflicts = %+v, file = %+v, %v", window, gotC.Touches, wantC, err)
		}
		gotU, err := service.UnpushedWork(ctx, journalclient.UnpushedWorkRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since})
		if err != nil {
			t.Fatal(err)
		}
		wantU, err := file.UnpushedWork(ctx, journalclient.UnpushedWorkRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since, ItemIDs: []string{"42"}})
		if err != nil || !reflect.DeepEqual(gotU.Work, wantU) {
			t.Fatalf("window %v: unpushed = %+v, file = %+v, %v", window, gotU.Work, wantU, err)
		}
	}
}

func assertListedOnly(t *testing.T, spy *spyEscalationReads, workflows ...string) {
	t.Helper()
	assertWindowedLists(t, spy)
	want := map[string]bool{}
	for _, name := range workflows {
		want[name] = true
	}
	got := map[string]bool{}
	for _, o := range spy.lists {
		if !want[o.Workflow] {
			t.Fatalf("listed workflow %q, want only %v", o.Workflow, workflows)
		}
		got[o.Workflow] = true
	}
	if len(got) != len(want) {
		t.Fatalf("listed workflows %v, want exactly %v", got, want)
	}
}
