//go:build integration

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// aliasedWorkcopies reproduces the wiring the daemon builds for a gaggle on an
// instance migrated from the pre-gaggle runtime layout: the manager is rooted
// at the gaggle's own real directory, while the node-wide pinned root it is
// given is the instance root's entry, which that migration leaves as an alias
// to the same place. It returns the manager root and the options carrying that
// pinned root.
func aliasedWorkcopies(t *testing.T, root string) (string, []worktree.ManagerOption) {
	t.Helper()
	testdep.RequireSymlinks(t, root)
	target := filepath.Join(root, "gaggles", "one", "workcopies")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "workcopies")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	return target, []worktree.ManagerOption{worktree.WithPinnedRoot(alias)}
}

// runExpirySweepUnderGrace runs the retention pass with the first-enable grace
// window left to start on this very pass and the operator's own dryRun off —
// the state every instance is in the first time it runs a build that enables
// retention. Contentless retirements are gated by the operator's decision
// alone, so the window must not hold them back.
func (f *reclaimFixture) runExpirySweepUnderGrace() (string, error) {
	f.t.Helper()
	cfg := *f.cfg
	cfg.Retention.Enabled = boolPtr(true)
	setup := &schedulerSetup{Config: &cfg, LegacyWorktrees: f.manager}
	var stdout, stderr bytes.Buffer
	err := pruneConfiguredRetention(f.t.Context(), f.layout, setup, &stdout, &stderr)
	if err != nil {
		f.t.Logf("sweep stderr: %s", stderr.String())
	}
	return stdout.String(), err
}

// TestIntegrationRecoverySweepRetiresDuplicatesUnderAliasedWorkcopies
// reproduces a live instance that retired nothing at all. Its inventory was
// well over its cap and held ten sets of byte-identical snapshots captured by
// DIFFERENT terminal runs, every one of which the superseded-duplicate rule
// should have retired; the pass ran, reported its worktree candidates, and
// left all of them. The daemon discards the pass's text output, so the reason
// was invisible: the node-wide workcopies root it hands the worktree manager is
// the instance root's entry, which on such an instance is an alias, and every
// recovery repository visit was refused before a single rule was consulted.
//
// The assertion is the whole duplicate set minus its newest member: a sweep
// that cannot reach the repositories retires nothing, and one that reaches them
// must still never retire the entry the others' content depends on.
func TestIntegrationRecoverySweepRetiresDuplicatesUnderAliasedWorkcopies(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixtureAt(t, 4, true)
	duplicate := map[string]string{"duplicate.txt": "identical agent work\n"}
	f.seed([]reclaimEntry{
		{runID: "aliased-dup-oldest", ageHours: 9, terminal: true, files: duplicate},
		{runID: "aliased-dup-middle", ageHours: 8, terminal: true, files: duplicate},
		{runID: "aliased-dup-newest", ageHours: 7, terminal: true, files: duplicate},
		{runID: "aliased-unique", ageHours: 6, terminal: true,
			files: map[string]string{"unique.txt": "distinct unlanded work\n"}},
	})
	f.lowerCap(2)

	if _, err := f.runExpirySweepUnderGrace(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	active := f.activeRunIDs()
	for _, superseded := range []string{"aliased-dup-oldest", "aliased-dup-middle"} {
		if active[superseded] {
			t.Fatalf("the sweep retired nothing through an aliased workcopies root: %s survived", superseded)
		}
		if recorded, found := f.reclamationJustification(superseded); !found || recorded != reclaimSupersededDuplicate {
			t.Fatalf("%s was retired without the superseded-duplicate justification: found=%v justification=%q", superseded, found, recorded)
		}
	}
	if !active["aliased-dup-newest"] {
		t.Fatal("the newest member of a duplicate set was retired instead of the older copies it protects")
	}
	if !active["aliased-unique"] {
		t.Fatal("a unique, unlanded patch was discarded by a pass that should only have retired duplicates")
	}
}
