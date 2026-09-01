package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/testgit"
)

// #3990 / #4000 (one defect, filed twice): every fetch into a managed mirror
// ends with `git maintenance run --auto`, which git detaches by default. The
// orphan outlives the git process this package waited on and keeps creating
// files under the mirror's git dir — in the reported runs, a repack of the
// loose objects the fetch had just written, about fifty milliseconds later —
// so whatever tears the mirror down next walks a tree a live writer is still
// changing: "unlinkat .../<key>/repo.git: directory not empty" from
// t.TempDir's RemoveAll there, and the same race against Reap/FinalizeRun in
// production. hardenedGitArgs pins the maintenance run to the foreground;
// these tests hold that seam in place from three sides — the arguments, the
// invocation git actually spawns, and the teardown that lost the race.

// detachedMaintenanceConfig is the mirror-side configuration that makes the
// orphan both wanted and busy: it asks for the detached form (the default the
// reported runs got) and enables the housekeeping tasks whose --auto
// conditions a freshly cloned mirror always meets. It is the shape a host
// running `git maintenance start`, or any inherited global config, hands this
// package — none of which may leave a writer alive in a mirror after the git
// call the daemon waited on has returned.
var detachedMaintenanceConfig = [][2]string{
	{"gc.autoDetach", "true"},
	{"maintenance.autoDetach", "true"},
	{"maintenance.loose-objects.enabled", "true"},
	{"maintenance.loose-objects.auto", "1"},
	{"maintenance.commit-graph.enabled", "true"},
}

// wideOriginBlobs is how many extra blobs newWideSourceRepo commits. It is
// the one tuning knob these tests have: enough objects that a detached
// repack of them takes long enough to still be running after the fetch that
// spawned it returned (and long enough to overlap the removal of the mirror),
// while staying cheap enough to build for every iteration.
const wideOriginBlobs = 2000

// The version-independent half of the guard: whatever a given git does with
// auto maintenance, every invocation this package issues carries the pin.
// gc.autoDetach is the older key newer git falls back to, so both are named.
func TestHardenedGitArgsPinAutoMaintenanceToTheForeground(t *testing.T) {
	args := strings.Join(hardenedGitArgs([]string{"fetch", "--prune", "origin"}), " ")
	for _, want := range []string{"maintenance.autoDetach=false", "gc.autoDetach=false"} {
		if !strings.Contains(args, want) {
			t.Errorf("hardenedGitArgs = %q, want %s — without it git detaches "+
				"housekeeping that outlives the command and writes into a mirror "+
				"its caller is tearing down (#3990/#4000)", args, want)
		}
	}
}

// A mirror fetch must never spawn its auto-maintenance child detached, so the
// housekeeping is over before the fetch this package waited on returns. This
// is the deterministic half of the #3990 guard: it reads what git was actually
// asked to do rather than trying to win a race.
//
// What it may read differs by git. `git maintenance run` only learned
// `--[no-]detach` in 2.44; on an older git — 2.39.5 ships in the cluster's
// runner image — the decision is configuration only, and hardenedGitArgs pins
// it from both keys, which the parent propagates to the child through
// GIT_CONFIG_PARAMETERS. Demanding the flag there failed a build whose
// maintenance was already in the foreground (#4146), so the flag is required
// only of a git that offers it, while opting INTO detaching is a failure on
// every git. The property itself is held from two other sides regardless:
// TestHardenedGitArgsPinAutoMaintenanceToTheForeground reads the pin, and
// TestMirrorStopsChangingWhenFetchReturns reads the effect.
func TestMirrorFetchRunsAutoMaintenanceInForeground(t *testing.T) {
	ctx := context.Background()
	origin := newSourceRepo(t)
	m, _ := detachedMaintenanceMirror(t, origin)

	tracePath := filepath.Join(t.TempDir(), "git-trace.log")
	t.Setenv("GIT_TRACE", tracePath)
	if _, err := m.WorkingCopy(ctx, origin); err != nil {
		t.Fatalf("WorkingCopy (fetch): %v", err)
	}

	runs := maintenanceInvocations(t, tracePath)
	if len(runs) == 0 {
		t.Skip("this git does not run auto maintenance after fetch; nothing to detach")
	}
	wantExplicitFlag := gitMaintenanceOffersDetachFlag(t, origin)
	for _, run := range runs {
		if strings.Contains(run, " --detach") {
			t.Errorf("auto maintenance ran as %q, want the foreground: a detached "+
				"housekeeping process outlives the fetch and keeps writing into "+
				"the mirror while its caller tears the mirror down (#3990/#4000)", run)
			continue
		}
		if wantExplicitFlag && !strings.Contains(run, "--no-detach") {
			t.Errorf("auto maintenance ran as %q, want --no-detach: this git "+
				"offers the flag, so the pin must reach the child explicitly "+
				"rather than through inherited configuration (#3990/#4000)", run)
		}
	}
}

// gitMaintenanceOffersDetachFlag reports whether this git's `maintenance run`
// accepts --[no-]detach, read from that git's own usage output rather than a
// parsed version number — the option is what matters, and a distribution's
// version string is not a reliable proxy for which patches it carries.
func gitMaintenanceOffersDetachFlag(t *testing.T, repository string) bool {
	t.Helper()
	// `-h` is a usage request: git prints to stdout and exits 129, which is
	// not an error worth reporting here.
	output, _ := testgit.Command("-C", repository, "maintenance", "run", "-h").CombinedOutput()
	return strings.Contains(string(output), "detach")
}

// A mirror must be quiescent the moment WorkingCopy returns: the caller's
// next step is free to reap, bundle from, or delete it. Sampling for a
// settle window can only expose a writer that is genuinely still running —
// with the foreground pin there is no such process, so the assertion cannot
// fail intermittently; before it, the detached repack lands inside the window
// every time.
func TestMirrorStopsChangingWhenFetchReturns(t *testing.T) {
	ctx := context.Background()
	origin := newWideSourceRepo(t, wideOriginBlobs)
	m, mirror := detachedMaintenanceMirror(t, origin)
	if _, err := m.WorkingCopy(ctx, origin); err != nil {
		t.Fatalf("WorkingCopy (fetch): %v", err)
	}

	const settle = 1500 * time.Millisecond
	quiescent := mirrorContents(t, mirror)
	for deadline := time.Now().Add(settle); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		current := mirrorContents(t, mirror)
		if added := addedEntries(quiescent, current); len(added) > 0 {
			t.Fatalf("%s gained %v after the fetch that produced it returned — a "+
				"detached git maintenance process is still writing into the mirror "+
				"the caller is entitled to tear down (#3990/#4000)", mirror, added)
		}
	}
}

// The reported failure, reproduced: fetch a managed mirror and delete the
// tree straight away, the way t.TempDir's cleanup and Reap both do. The
// origin is wide enough that removing the mirror's thousands of hard-linked
// loose objects is still in progress when a detached repack would start, so
// the walk and the writer overlap and the removal fails with "directory not
// empty". Repeated because a race is a distribution, not an event; each
// iteration gets a fresh root so one cannot mask the next.
func TestMirrorRemovalAfterFetchIsNotRacedByAutoMaintenance(t *testing.T) {
	ctx := context.Background()
	origin := newWideSourceRepo(t, wideOriginBlobs)
	const iterations = 3
	for i := range iterations {
		root := filepath.Join(t.TempDir(), "workcopies")
		m, err := NewManager(root)
		if err != nil {
			t.Fatalf("iteration %d: NewManager: %v", i, err)
		}
		repoDir, err := m.WorkingCopy(ctx, origin)
		if err != nil {
			t.Fatalf("iteration %d: WorkingCopy (clone): %v", i, err)
		}
		configureDetachedMaintenance(t, repoDir)
		if _, err := m.WorkingCopy(ctx, origin); err != nil {
			t.Fatalf("iteration %d: WorkingCopy (fetch): %v", i, err)
		}
		if err := os.RemoveAll(root); err != nil {
			t.Fatalf("iteration %d: remove the mirror root straight after the fetch "+
				"that refreshed it: %v — a detached git maintenance process is still "+
				"writing under it (#3990/#4000)", i, err)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("iteration %d: mirror root still present after RemoveAll: %v", i, err)
		}
	}
}

// detachedMaintenanceMirror returns a manager whose mirror of origin is warm
// and configured to ask for detached background housekeeping.
func detachedMaintenanceMirror(t *testing.T, origin string) (*Manager, string) {
	t.Helper()
	m, err := NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mirror, err := m.WorkingCopy(context.Background(), origin)
	if err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}
	configureDetachedMaintenance(t, mirror)
	return m, mirror
}

// configureDetachedMaintenance writes detachedMaintenanceConfig into the
// mirror itself. The command-line -c overrides that hardenedGitArgs carries
// win over all of it, which is the property under test: no repo-level or
// inherited configuration can put a writer behind this package's back.
func configureDetachedMaintenance(t *testing.T, mirror string) {
	t.Helper()
	for _, setting := range detachedMaintenanceConfig {
		runTestGit(t, mirror, "config", setting[0], setting[1])
	}
}

// newWideSourceRepo is newSourceRepo with count extra blobs, so a local clone
// hardlinks thousands of loose objects into the mirror: enough that removing
// the mirror takes long enough to overlap a detached repack, and enough
// loose-object work for that repack to be worth starting.
func newWideSourceRepo(t *testing.T, count int) string {
	t.Helper()
	repo := newSourceRepo(t)
	for i := range count {
		mustWriteFile(t, filepath.Join(repo, "blobs", fmt.Sprintf("blob-%04d.txt", i)), fmt.Sprintf("blob %d\n", i))
	}
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-q", "-m", "wide tree")
	return repo
}

// maintenanceInvocations returns every `git maintenance run` command recorded
// in a GIT_TRACE log, in order.
func maintenanceInvocations(t *testing.T, tracePath string) []string {
	t.Helper()
	raw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read git trace %s: %v", tracePath, err)
	}
	var runs []string
	for _, line := range strings.Split(string(raw), "\n") {
		_, command, found := strings.Cut(line, "trace: run_command:")
		if !found {
			continue
		}
		command = strings.TrimSpace(command)
		if strings.Contains(command, "maintenance run") {
			runs = append(runs, command)
		}
	}
	return runs
}

// mirrorContents is the set of paths under dir, relative to it.
func mirrorContents(t *testing.T, dir string) map[string]bool {
	t.Helper()
	contents := map[string]bool{}
	err := filepath.Walk(dir, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			// A file the writer under test deleted mid-walk is not a
			// reportable condition here; the assertion is about additions.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		contents[rel] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return contents
}

func addedEntries(before, after map[string]bool) []string {
	var added []string
	for path := range after {
		if !before[path] {
			added = append(added, path)
		}
	}
	return added
}
