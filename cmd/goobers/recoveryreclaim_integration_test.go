//go:build integration

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// reclaimEntry describes one seeded inventory entry. files is the snapshot's
// content relative to the shared base; an empty map produces a capture whose
// tree matches its base exactly (the stored no-diff case, #5095).
type reclaimEntry struct {
	runID     string
	files     map[string]string
	terminal  bool
	abandon   bool
	ageHours  int
	published recovery.Record
}

// reclaimFixture drives the real cleanup guard against an inventory that is
// already at its configured cap, so every assertion is about what the
// capacity-pressure eviction hook actually did on disk.
type reclaimFixture struct {
	t          *testing.T
	layout     instance.Layout
	cfg        *instance.Config
	source     string
	mirror     string
	workcopies string
	manager    *worktree.Manager
	cloneURL   func(apiv1.RepoRef) (string, error)
	key        string
	base       string
	root       string
	cap        int
}

func newReclaimFixture(t *testing.T, capacity int) *reclaimFixture {
	t.Helper()
	return newReclaimFixtureAt(t, capacity, false)
}

// newReclaimFixtureAt builds the fixture with the instance root's workcopies
// entry either as its own directory or, when aliased is set, as an alias to
// the gaggle's directory — the layout an instance migrated from the pre-gaggle
// runtime keeps.
func newReclaimFixtureAt(t *testing.T, capacity int, aliased bool) *reclaimFixture {
	t.Helper()
	testdep.Require(t, "git")
	f := &reclaimFixture{t: t, layout: instance.NewLayout(t.TempDir()), source: t.TempDir(), cap: capacity}
	recoveryCLIGit(t, f.source, "init", "--initial-branch=main")
	recoveryCLIGit(t, f.source, "commit", "--allow-empty", "-m", "base")
	f.base = recoveryCLIGit(t, f.source, "rev-parse", "HEAD")

	f.workcopies = filepath.Join(f.layout.Root, "workcopies")
	var options []worktree.ManagerOption
	if aliased {
		f.workcopies, options = aliasedWorkcopies(t, f.layout.Root)
	}
	manager, err := worktree.NewManager(f.workcopies, options...)
	if err != nil {
		t.Fatal(err)
	}
	f.manager = manager
	f.cloneURL = func(apiv1.RepoRef) (string, error) { return f.source, nil }
	previous := repoCloneURL
	repoCloneURL = f.cloneURL
	t.Cleanup(func() { repoCloneURL = previous })

	f.cfg = &instance.Config{
		Repos:     []instance.RepoRef{{Provider: "github", Owner: "team", Name: "repo"}},
		Retention: instance.RetentionConfig{Recovery: &instance.RecoverySnapshotConfig{MaxSnapshots: capacity}},
	}
	f.key = (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}).CanonicalKey()
	root, err := prepareRecoveryInventory(f.layout.Root)
	if err != nil {
		t.Fatal(err)
	}
	f.root = root
	return f
}

// seed fills the inventory to its cap. Every snapshot commit is created before
// the managed mirror, because WithRecoveryRepositories only ever visits that
// mirror or a pinned clone — a commit added to source afterwards is invisible
// to eviction.
func (f *reclaimFixture) seed(entries []reclaimEntry) []reclaimEntry {
	f.t.Helper()
	snapshots := make([]string, len(entries))
	for i, entry := range entries {
		snapshots[i] = f.commitSnapshot(entry)
	}
	mirror, err := f.manager.WorkingCopy(f.t.Context(), f.source)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mirror = mirror
	for i := range entries {
		entries[i].published = f.publish(entries[i], snapshots[i])
	}
	return entries
}

func (f *reclaimFixture) commitSnapshot(entry reclaimEntry) string {
	f.t.Helper()
	recoveryCLIGit(f.t, f.source, "checkout", "-b", "seed-"+entry.runID, f.base)
	if len(entry.files) == 0 {
		// An empty commit reproduces exactly what a pre-guard capture left
		// behind: a real snapshot commit whose tree equals its base.
		recoveryCLIGit(f.t, f.source, "commit", "--allow-empty", "-m", "no-diff "+entry.runID)
	} else {
		for name, body := range entry.files {
			path := filepath.Join(f.source, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				f.t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				f.t.Fatal(err)
			}
		}
		recoveryCLIGit(f.t, f.source, "add", "--all")
		recoveryCLIGit(f.t, f.source, "commit", "-m", "snapshot "+entry.runID)
	}
	snapshot := recoveryCLIGit(f.t, f.source, "rev-parse", "HEAD")
	recoveryCLIGit(f.t, f.source, "checkout", "main")
	return snapshot
}

func (f *reclaimFixture) publish(entry reclaimEntry, snapshot string) recovery.Record {
	f.t.Helper()
	ctx := f.t.Context()
	ref, err := recovery.RefForSnapshot(entry.runID, snapshot)
	if err != nil {
		f.t.Fatal(err)
	}
	digest, err := recovery.WriteSnapshotPatch(ctx, f.mirror, f.base, snapshot, io.Discard)
	if err != nil {
		f.t.Fatal(err)
	}
	createdAt := time.Now().Add(-time.Duration(entry.ageHours) * time.Hour)
	record := recovery.Record{
		Version: 1, RunID: entry.runID, RepositoryKey: f.key, Ref: ref,
		BaseSHA: f.base, SnapshotSHA: snapshot, PatchDigest: digest,
		CreatedAt: createdAt, RetainUntil: createdAt.Add(29 * 24 * time.Hour),
	}
	published, _, err := recovery.PublishToInventoryWithEviction(ctx, f.mirror, f.root, []string{f.manager.Root}, record, f.cap, 1<<20, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.seedRun(entry)
	if entry.abandon {
		f.abandon(published)
	}
	return published
}

func (f *reclaimFixture) seedRun(entry reclaimEntry) {
	f.t.Helper()
	if !entry.terminal {
		return
	}
	run, err := journal.Create(f.layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: entry.runID, Workflow: "implementation",
		WorkflowVersion: 1, StartedAt: time.Now().Add(-time.Duration(entry.ageHours+1) * time.Hour),
	}, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		f.t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		f.t.Fatal(err)
	}
}

// abandon replays exactly what `goobers recovery-abandon` appends: an operator
// decision about one exact published record, on the instance log.
func (f *reclaimFixture) abandon(record recovery.Record) {
	f.t.Helper()
	event, err := recovery.AbandonedEvent(record)
	if err != nil {
		f.t.Fatal(err)
	}
	log := recoveryCleanupJournal{directory: f.layout.SchedulerDir(), scrubber: journal.NewRegistryScrubber()}
	if err := log.Append(event); err != nil {
		f.t.Fatal(err)
	}
}

// cleanupNewRun drives the "cap + 1"th cleanup: a brand new run whose worktree
// carries real, unique work and therefore needs a free inventory slot.
func (f *reclaimFixture) cleanupNewRun(runID string) error {
	f.t.Helper()
	ctx := context.Background()
	option, err := recoveryCleanupOption(f.layout, f.cfg, f.workcopies, f.cloneURL, journal.NewRegistryScrubber(), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	option(f.manager)
	run, err := journal.Create(f.layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: runID, Workflow: "implementation",
		WorkflowVersion: 1, StartedAt: time.Now(),
	}, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	workspace, err := f.manager.Create(ctx, worktree.CreateOptions{
		RepoURL: f.source, RunID: runID + "-stage", OwnerRunID: runID,
		BaseRef: "main", Branch: "goobers/implementation/" + runID,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "new.txt"), []byte("new agent work"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return workspace.Remove(ctx, worktree.RemoveOptions{})
}

func (f *reclaimFixture) activeRunIDs() map[string]bool {
	f.t.Helper()
	entries, err := recovery.ReadInventory(f.t.Context(), f.root, recovery.MaxInventoryEntries)
	if err != nil {
		f.t.Fatal(err)
	}
	active := make(map[string]bool, len(entries))
	for _, entry := range entries {
		active[entry.Record.RunID] = true
	}
	return active
}

func (f *reclaimFixture) instanceLogContains(code string) bool {
	f.t.Helper()
	events, err := journal.ReadInstanceLog(f.layout.SchedulerDir())
	if err != nil {
		f.t.Fatal(err)
	}
	for _, event := range events {
		if event.Error != nil && event.Error.Code == code {
			return true
		}
	}
	return false
}

// TestIntegrationRecoveryReclaimsNoDiffEntriesUnderCapacityPressure is #5095's
// acceptance fixture: an inventory filled entirely by stored no-diff captures
// admits a new publish. Those entries hold no patch bytes at all, so freeing
// one discards nothing.
func TestIntegrationRecoveryReclaimsNoDiffEntriesUnderCapacityPressure(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 2)
	f.seed([]reclaimEntry{{runID: "nodiff-a", ageHours: 3}, {runID: "nodiff-b", ageHours: 2}})
	if err := f.cleanupNewRun("new-run"); err != nil {
		t.Fatalf("cleanup was refused although every retained entry stored a no-diff capture: %v", err)
	}
	active := f.activeRunIDs()
	if !active["new-run"] {
		t.Fatal("the new run's snapshot was not published after reclamation")
	}
	if active["nodiff-a"] && active["nodiff-b"] {
		t.Fatal("no no-diff entry was reclaimed")
	}
}

// TestIntegrationRecoveryReclaimsBookkeepingOnlyEntries covers #5119's shape:
// the retained patch is non-empty, so the empty-diff guard let it through, but
// every path it touches is a Goobers stage artifact rather than repository
// content.
func TestIntegrationRecoveryReclaimsBookkeepingOnlyEntries(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 2)
	f.seed([]reclaimEntry{
		{runID: "bookkeeping-a", ageHours: 3, files: map[string]string{"mutations.jsonl": "{\"a\":1}\n"}},
		{runID: "bookkeeping-b", ageHours: 2, files: map[string]string{"claimed-item.json": "{}\n", ".goobers/state.json": "{}\n"}},
	})
	if err := f.cleanupNewRun("new-run"); err != nil {
		t.Fatalf("cleanup was refused although every retained patch touched only harness artifacts: %v", err)
	}
	if !f.activeRunIDs()["new-run"] {
		t.Fatal("the new run's snapshot was not published after reclamation")
	}
}

// TestIntegrationRecoveryReclaimsSupersededDuplicate is #5110's supersession:
// identical content retained many times over. The oldest copy is reclaimable
// because a newer entry protects the same bytes; the newest never is.
func TestIntegrationRecoveryReclaimsSupersededDuplicate(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 3)
	content := map[string]string{"duplicate.txt": "identical agent work\n"}
	f.seed([]reclaimEntry{
		{runID: "duplicate-oldest", ageHours: 5, files: content},
		{runID: "duplicate-middle", ageHours: 4, files: content},
		{runID: "duplicate-newest", ageHours: 3, files: content},
	})
	if err := f.cleanupNewRun("new-run"); err != nil {
		t.Fatalf("cleanup was refused although two newer copies of the same content were retained: %v", err)
	}
	active := f.activeRunIDs()
	if !active["duplicate-newest"] {
		t.Fatal("the newest member of a duplicate set was reclaimed; it is the copy that protects the content")
	}
	if active["duplicate-oldest"] {
		t.Fatal("a superseded duplicate was kept instead of the slot being freed")
	}
	if !active["new-run"] {
		t.Fatal("the new run's snapshot was not published after reclamation")
	}
}

// TestIntegrationRecoveryReclaimsAbandonedEntryOnDemand is #5110's operator
// complaint: `goobers recovery-abandon` freed nothing, because only the
// periodic sweep acted on the decision and that sweep is report-only for the
// whole first-enable grace window. On-demand eviction has no grace window, so
// the decision takes effect at the moment capacity is needed.
func TestIntegrationRecoveryReclaimsAbandonedEntryOnDemand(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 2)
	f.seed([]reclaimEntry{
		{runID: "abandoned-run", ageHours: 4, terminal: true, abandon: true,
			files: map[string]string{"abandoned.txt": "real but abandoned work\n"}},
		{runID: "in-flight-run", ageHours: 1,
			files: map[string]string{"inflight.txt": "still working\n"}},
	})
	if err := f.cleanupNewRun("new-run"); err != nil {
		t.Fatalf("cleanup was refused although an explicitly abandoned entry was retained: %v", err)
	}
	active := f.activeRunIDs()
	if active["abandoned-run"] {
		t.Fatal("the explicitly abandoned entry was not reclaimed")
	}
	if !active["in-flight-run"] {
		t.Fatal("an in-flight entry holding unique work was reclaimed instead")
	}
	if !active["new-run"] {
		t.Fatal("the new run's snapshot was not published after reclamation")
	}
}

// TestIntegrationRecoveryRefusesToReclaimUniqueWork is the safety half of
// #5354's acceptance, unchanged in what it protects: when every retained entry
// holds a real, unique, unlanded patch, the hook reclaims NOTHING. Choosing a
// victim among these is #5096's policy decision, which #5370 closed as
// won't-do.
//
// What #5370 changed is what happens to the capture that found no room. It no
// longer refuses the cleanup - refusing protected nothing and stopped the
// instance - it overflows to the ref tier. So this asserts the same invariant
// from the other side: both retained entries survive untouched, and the new
// work is retained too, one tier down. The original refusal is still
// reproduced under onFull: refuse, covered in
// recoveryoverflow_integration_test.go.
func TestIntegrationRecoveryRefusesToReclaimUniqueWork(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 2)
	f.seed([]reclaimEntry{
		{runID: "unique-a", ageHours: 4, files: map[string]string{"a.txt": "distinct work a\n"}},
		{runID: "unique-b", ageHours: 3, files: map[string]string{"b.txt": "distinct work b\n"}},
	})
	if err := f.cleanupNewRun("new-run"); err != nil {
		t.Fatalf("cleanup was refused although the capture could overflow: %v", err)
	}
	active := f.activeRunIDs()
	if !active["unique-a"] || !active["unique-b"] {
		t.Fatal("agent-authored work was discarded to free a slot")
	}
	if active["new-run"] {
		t.Fatal("the new capture took an inventory slot that was not free")
	}
	if _, found := f.reclamationJustification("unique-a"); found {
		t.Fatal("a unique, unlanded entry was reclaimed under capacity pressure")
	}
	if overflow := f.overflowRunIDs(); !overflow["new-run"] {
		t.Fatalf("the new capture was neither retained nor held at the ref tier: %v", overflow)
	}
}

// TestIntegrationRecoveryReportsUnreadableReservations covers the gap #5354
// records: ReadInventoryTolerant's unreadable list had exactly one caller and
// that caller discarded it, so reservations that consume capacity and that no
// reclamation path can free stayed invisible.
func TestIntegrationRecoveryReportsUnreadableReservations(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 3)
	f.seed([]reclaimEntry{{runID: "nodiff-a", ageHours: 3}, {runID: "nodiff-b", ageHours: 2}})
	// A crashed publish leaves a reservation directory holding only lock
	// files. It counts toward the cap and can never be interpreted.
	debris := filepath.Join(f.root, strings.Repeat("a", 64))
	if err := os.Mkdir(debris, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(debris, ".publish.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.cleanupNewRun("new-run"); err != nil {
		t.Fatalf("cleanup was refused although reclaimable no-diff entries were retained: %v", err)
	}
	if !f.instanceLogContains("recovery_inventory_unreadable") {
		t.Fatal("the unreadable reservation was discarded instead of reported")
	}
}
