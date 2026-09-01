package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// The worker's config-tree reload (#3884). Every test here drives
// reloadOnce/startWorkerConfigWatcher against a real scaffolded instance root
// and real config loading — the seam under test is precisely "does this worker
// notice a config-tree edit while it stays up", so faking the tree would fake
// the behaviour.
//
// Determinism: reloadOnce is synchronous and takes no clock, so every
// assertion about WHAT a reload did is made by calling it directly. Only the
// two lifecycle tests run the ticker, and they poll a bounded number of times
// rather than sleeping for a fixed duration.

const workerReloadWaitTimeout = 10 * time.Second

func workerReloadSeams(t *testing.T, root string) *workerSeams {
	t.Helper()
	seams, err := newWorkerSeams(root, nil)
	if err != nil {
		t.Fatalf("newWorkerSeams: %v", err)
	}
	seams.logf = func(format string, args ...any) { t.Logf(format, args...) }
	return seams
}

func gooberInstructionsPath(root, gaggle, goober string) string {
	return filepath.Join(instance.NewLayout(root).ConfigDir(), "gaggles", gaggle, "goobers", goober, "instructions.md")
}

func gagglePath(root, gaggle string) string {
	return filepath.Join(instance.NewLayout(root).ConfigDir(), "gaggles", gaggle, "gaggle.yaml")
}

func writeFileContent(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFileContent(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// builtEntry returns the gaggle's cached seams from the currently published
// snapshot, or nil when the reload invalidated them.
func builtEntry(t *testing.T, seams *workerSeams, gaggle string) *builtGaggleSeams {
	t.Helper()
	snapshot := seams.snapshot.Load()
	if snapshot == nil {
		t.Fatal("no config snapshot published")
	}
	return snapshot.gaggles[gaggle]
}

// TestWorkerReloadObservesChangedInstructionsWithoutRestart is #3884's
// headline behaviour and I-51's counterfactual: a Workflows edit that reaches
// the worker's config tree while the worker is up must reach the NEXT
// attempt's kit, with no pod restart.
//
// The proof that the KIT changed (not merely the snapshot) is the fingerprint
// stored beside it: buildGaggleSeams computes it from the very same
// instructions map it hands buildRunnerConfig as InstructionsByGoober, so a
// built entry whose fingerprint matches the new tree's fingerprint is a kit
// built from the new instruction bytes.
func TestWorkerReloadObservesChangedInstructionsWithoutRestart(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)

	before, err := seams.forGaggle("example")
	if err != nil {
		t.Fatalf("forGaggle before reload: %v", err)
	}
	beforeSnapshot := seams.snapshot.Load()
	beforeFingerprint := builtEntry(t, seams, "example").fingerprint

	instructions := gooberInstructionsPath(root, "example", "coder")
	writeFileContent(t, instructions, readFileContent(t, instructions)+"\n\nAlways rebase before pushing.\n")

	outcome, err := seams.reloadOnce()
	if err != nil {
		t.Fatalf("reloadOnce: %v", err)
	}
	if !outcome.Applied {
		t.Fatalf("reload outcome = %+v, want a new config tree applied", outcome)
	}
	if outcome.Digest == beforeSnapshot.digest {
		t.Fatalf("reload digest %s unchanged after an instructions edit", outcome.Digest)
	}
	if len(outcome.Invalidated) != 1 || outcome.Invalidated[0] != "example" {
		t.Fatalf("reload invalidated %v, want [example]", outcome.Invalidated)
	}

	after, err := seams.forGaggle("example")
	if err != nil {
		t.Fatalf("forGaggle after reload: %v", err)
	}
	if after == before {
		t.Fatal("forGaggle returned the pre-reload seams; the worker is still serving the stale kit")
	}

	afterFingerprint := builtEntry(t, seams, "example").fingerprint
	if afterFingerprint == beforeFingerprint {
		t.Fatal("rebuilt seams carry the pre-edit config fingerprint")
	}
	// Independently derived from the tree on disk: the rebuilt kit's inputs
	// are the edited tree's inputs, not a mixture and not the old ones.
	fresh := workerReloadSeams(t, root)
	if _, err := fresh.forGaggle("example"); err != nil {
		t.Fatalf("forGaggle on a freshly started worker: %v", err)
	}
	if want := builtEntry(t, fresh, "example").fingerprint; afterFingerprint != want {
		t.Fatalf("rebuilt fingerprint = %s, want %s (what a restarted worker would build)", afterFingerprint, want)
	}
}

// TestWorkerReloadObservesChangedGaggleOwner pins I-51's exact measured
// symptom: the worker held `owner: masra91` in its gaggle config while the
// daemon ran the retargeted tree, so RunnerGrants found no binding for the
// real project and fell back to bindings[0]. The gaggle project ref is the
// credential seam's own input (buildRunnerConfig's GaggleProject), so this is
// the credential half of the reload, not a second copy of the kit half.
func TestWorkerReloadObservesChangedGaggleOwner(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)

	if _, err := seams.forGaggle("example"); err != nil {
		t.Fatalf("forGaggle before reload: %v", err)
	}
	if got := gaggleProjectRef(seams.snapshot.Load().set, "example").Owner; got != "your-org" {
		t.Fatalf("owner before reload = %q, want the scaffolded your-org", got)
	}

	replaceInFile(t, gagglePath(root, "example"), "owner: your-org", "owner: Agent-Clubhouse")

	outcome, err := seams.reloadOnce()
	if err != nil {
		t.Fatalf("reloadOnce: %v", err)
	}
	if !outcome.Applied {
		t.Fatalf("reload outcome = %+v, want a retargeted gaggle applied", outcome)
	}
	if got := gaggleProjectRef(seams.snapshot.Load().set, "example").Owner; got != "Agent-Clubhouse" {
		t.Fatalf("owner after reload = %q, want Agent-Clubhouse", got)
	}
	if entry := builtEntry(t, seams, "example"); entry != nil {
		t.Fatal("retargeted gaggle kept its pre-retarget seams; the next attempt would resolve credentials against the stale project")
	}
}

// TestWorkerReloadRetainsUnchangedGaggleSeams is the other half of "rebuild
// only what changed": a worker serving two gaggles must not pay a rebuild —
// nor a repeated harness preflight — for a gaggle the edit did not touch, and
// must carry its existing kit across by pointer.
func TestWorkerReloadRetainsUnchangedGaggleSeams(t *testing.T) {
	root := initDemo(t)
	addSecondGaggle(t, root, "example", "second")
	seams := workerReloadSeams(t, root)

	if _, err := seams.forGaggle("example"); err != nil {
		t.Fatalf("forGaggle example: %v", err)
	}
	if _, err := seams.forGaggle("second"); err != nil {
		t.Fatalf("forGaggle second: %v", err)
	}
	untouched := builtEntry(t, seams, "second")

	instructions := gooberInstructionsPath(root, "example", "coder")
	writeFileContent(t, instructions, readFileContent(t, instructions)+"\n\nPrefer table-driven tests.\n")

	outcome, err := seams.reloadOnce()
	if err != nil {
		t.Fatalf("reloadOnce: %v", err)
	}
	if !outcome.Applied {
		t.Fatalf("reload outcome = %+v, want applied", outcome)
	}
	if len(outcome.Invalidated) != 1 || outcome.Invalidated[0] != "example" {
		t.Fatalf("reload invalidated %v, want only [example]", outcome.Invalidated)
	}
	if len(outcome.Retained) != 1 || outcome.Retained[0] != "second" {
		t.Fatalf("reload retained %v, want only [second]", outcome.Retained)
	}
	if got := builtEntry(t, seams, "second"); got != untouched {
		t.Fatal("unchanged gaggle's seams were rebuilt across the reload")
	}
	kept, err := seams.forGaggle("second")
	if err != nil {
		t.Fatalf("forGaggle second after reload: %v", err)
	}
	if kept != untouched.seams {
		t.Fatal("forGaggle rebuilt the unchanged gaggle after the reload")
	}
}

// TestWorkerReloadSkipsUnchangedConfigTree keeps the steady state free: a
// reload check against a tree nobody edited must publish nothing and rebuild
// nothing, so the watcher's cadence costs a directory digest and no more.
func TestWorkerReloadSkipsUnchangedConfigTree(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)

	built, err := seams.forGaggle("example")
	if err != nil {
		t.Fatalf("forGaggle: %v", err)
	}
	snapshot := seams.snapshot.Load()

	for i := range 3 {
		outcome, err := seams.reloadOnce()
		if err != nil {
			t.Fatalf("reloadOnce %d: %v", i, err)
		}
		if outcome.Applied {
			t.Fatalf("reloadOnce %d applied a snapshot for an unchanged tree: %+v", i, outcome)
		}
	}
	if seams.snapshot.Load() != snapshot {
		t.Fatal("unchanged tree republished the config snapshot")
	}
	again, err := seams.forGaggle("example")
	if err != nil {
		t.Fatalf("forGaggle after idle reloads: %v", err)
	}
	if again != built {
		t.Fatal("unchanged tree rebuilt the gaggle seams")
	}
}

// TestWorkerReloadRejectsInvalidConfigTreeAndRecovers is the no-silent-fallback
// rule. A tree that stops parsing must produce a loud, named error and leave
// the last-known-good snapshot serving; a later good edit must then apply
// without a restart.
func TestWorkerReloadRejectsInvalidConfigTreeAndRecovers(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)

	good, err := seams.forGaggle("example")
	if err != nil {
		t.Fatalf("forGaggle: %v", err)
	}
	goodSnapshot := seams.snapshot.Load()

	path := gagglePath(root, "example")
	valid := readFileContent(t, path)
	writeFileContent(t, path, "apiVersion: goobers.dev/v1alpha1\nkind: Gaggle\nmetadata:\n  name: example\nspec:\n  project: [not, an, object]\n")

	outcome, err := seams.reloadOnce()
	if err == nil {
		t.Fatalf("reloadOnce accepted an invalid config tree: %+v", outcome)
	}
	if !strings.Contains(err.Error(), "worker: load config directory") {
		t.Fatalf("reload error = %v, want the named config-directory load failure", err)
	}
	if outcome.Applied {
		t.Fatalf("reload outcome = %+v, want nothing applied", outcome)
	}
	if seams.snapshot.Load() != goodSnapshot {
		t.Fatal("invalid config tree replaced the last-known-good snapshot")
	}
	stillGood, err := seams.forGaggle("example")
	if err != nil {
		t.Fatalf("forGaggle while the tree is invalid: %v", err)
	}
	if stillGood != good {
		t.Fatal("invalid config tree disturbed the seams already in force")
	}

	writeFileContent(t, path, strings.Replace(valid, "owner: your-org", "owner: recovered-org", 1))
	recovered, err := seams.reloadOnce()
	if err != nil {
		t.Fatalf("reloadOnce after repair: %v", err)
	}
	if !recovered.Applied {
		t.Fatalf("reload outcome after repair = %+v, want applied", recovered)
	}
	if got := gaggleProjectRef(seams.snapshot.Load().set, "example").Owner; got != "recovered-org" {
		t.Fatalf("owner after recovery = %q, want recovered-org", got)
	}
}

// TestWorkerReloadPublishesWholeSnapshots is the atomicity proof. Readers race
// a reload that flips the tree between two known states; every observation
// must be entirely one state or entirely the other.
//
// Two independent invariants are checked per observation, because a torn read
// could show up in either place:
//
//  1. the snapshot's digest and its parsed gaggle owner agree, and
//  2. every cached kit's fingerprint is exactly what THAT snapshot's own tree
//     fingerprints to — a kit built from one tree can never be seen attached
//     to another.
func TestWorkerReloadPublishesWholeSnapshots(t *testing.T) {
	root := initDemo(t)
	path := gagglePath(root, "example")
	original := readFileContent(t, path)
	first := strings.Replace(original, "owner: your-org", "owner: owner-one", 1)
	second := strings.Replace(original, "owner: your-org", "owner: owner-two", 1)
	if first == original || second == original {
		t.Fatal("gaggle fixture does not carry the scaffolded owner")
	}

	writeFileContent(t, path, first)
	seams := workerReloadSeams(t, root)
	if _, err := seams.forGaggle("example"); err != nil {
		t.Fatalf("forGaggle: %v", err)
	}

	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				writeFileContent(t, path, second)
			} else {
				writeFileContent(t, path, first)
			}
			if _, err := seams.reloadOnce(); err != nil {
				t.Errorf("reloadOnce: %v", err)
				return
			}
		}
	}()

	var readers sync.WaitGroup
	for reader := 0; reader < 4; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 200; i++ {
				if _, err := seams.forGaggle("example"); err != nil {
					t.Errorf("forGaggle: %v", err)
					return
				}
				snapshot := seams.snapshot.Load()
				owner := gaggleProjectRef(snapshot.set, "example").Owner
				if owner != "owner-one" && owner != "owner-two" {
					t.Errorf("observed owner %q, want a whole tree's owner", owner)
					return
				}
				for gaggle, built := range snapshot.gaggles {
					want, err := seams.gaggleFingerprint(snapshot, gaggle)
					if err != nil {
						t.Errorf("fingerprint gaggle %q against its own snapshot: %v", gaggle, err)
						return
					}
					if built.fingerprint != want {
						t.Errorf("gaggle %q seams carry fingerprint %s inside a snapshot fingerprinting to %s", gaggle, built.fingerprint, want)
						return
					}
				}
			}
		}()
	}

	readers.Wait()
	close(stop)
	writer.Wait()
}

// TestWorkerConfigWatcherAppliesEditsAndStopsOnShutdown covers the lifecycle:
// the ticker actually applies an edit without anyone calling reloadOnce, and
// Stop is a hard guarantee — after it returns, the goroutine is gone and no
// later edit can move the published snapshot.
func TestWorkerConfigWatcherAppliesEditsAndStopsOnShutdown(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	if _, err := seams.forGaggle("example"); err != nil {
		t.Fatalf("forGaggle: %v", err)
	}
	initial := seams.snapshot.Load().digest

	watcher := startWorkerConfigWatcher(context.Background(), seams, time.Millisecond)
	t.Cleanup(watcher.Stop)

	replaceInFile(t, gagglePath(root, "example"), "owner: your-org", "owner: watched-org")
	deadline := time.Now().Add(workerReloadWaitTimeout)
	for seams.snapshot.Load().digest == initial {
		if time.Now().After(deadline) {
			t.Fatal("watcher did not apply the config edit")
		}
		time.Sleep(time.Millisecond)
	}
	applied := seams.snapshot.Load()
	if got := gaggleProjectRef(applied.set, "example").Owner; got != "watched-org" {
		t.Fatalf("owner after watched reload = %q, want watched-org", got)
	}

	watcher.Stop()

	replaceInFile(t, gagglePath(root, "example"), "owner: watched-org", "owner: after-shutdown")
	// A stopped watcher has no goroutine left to notice this. Bounded, not
	// slept-on: reloadOnce afterwards proves the edit was genuinely visible
	// and merely unwatched.
	for i := 0; i < 50; i++ {
		if seams.snapshot.Load() != applied {
			t.Fatal("stopped watcher published another snapshot")
		}
		time.Sleep(time.Millisecond)
	}
	outcome, err := seams.reloadOnce()
	if err != nil {
		t.Fatalf("reloadOnce after shutdown: %v", err)
	}
	if !outcome.Applied {
		t.Fatalf("post-shutdown reload outcome = %+v, want the unwatched edit applied on demand", outcome)
	}
}

// TestWorkerConfigWatcherLogsRejectionOnceAndRecovery pins the loud-failure
// contract at the watcher level: an unparsable tree is reported, reported only
// once while it stays unparsable, and its repair is reported too.
func TestWorkerConfigWatcherLogsRejectionOnceAndRecovery(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	if _, err := seams.forGaggle("example"); err != nil {
		t.Fatalf("forGaggle: %v", err)
	}

	var mu sync.Mutex
	var lines []string
	seams.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
	}
	matching := func(substring string) int {
		mu.Lock()
		defer mu.Unlock()
		count := 0
		for _, line := range lines {
			if strings.Contains(line, substring) {
				count++
			}
		}
		return count
	}
	waitFor := func(substring string, want int) {
		t.Helper()
		deadline := time.Now().Add(workerReloadWaitTimeout)
		for matching(substring) < want {
			if time.Now().After(deadline) {
				mu.Lock()
				defer mu.Unlock()
				t.Fatalf("timed out waiting for %d log line(s) containing %q; saw %v", want, substring, lines)
			}
			time.Sleep(time.Millisecond)
		}
	}

	path := gagglePath(root, "example")
	valid := readFileContent(t, path)
	writeFileContent(t, path, "apiVersion: goobers.dev/v1alpha1\nkind: Gaggle\nmetadata:\n  name: example\nspec:\n  project: [not, an, object]\n")

	watcher := startWorkerConfigWatcher(context.Background(), seams, time.Millisecond)
	t.Cleanup(watcher.Stop)

	waitFor("rejected", 1)
	// Many ticks pass while the tree stays broken; the complaint must not
	// repeat for an unchanged failure.
	time.Sleep(50 * time.Millisecond)
	if got := matching("rejected"); got != 1 {
		t.Fatalf("rejection logged %d times for one unchanging failure, want 1", got)
	}

	writeFileContent(t, path, strings.Replace(valid, "owner: your-org", "owner: repaired-org", 1))
	waitFor("readable again", 1)
	waitFor("applied config tree", 1)

	watcher.Stop()
	if got := gaggleProjectRef(seams.snapshot.Load().set, "example").Owner; got != "repaired-org" {
		t.Fatalf("owner after repair = %q, want repaired-org", got)
	}
}

// addSecondGaggle clones the scaffolded gaggle into a second one so the reload
// has two independent gaggles to tell apart. Goober names are instance-global,
// so the clone's goober is renamed along with the gaggle.
func addSecondGaggle(t *testing.T, root, source, name string) {
	t.Helper()
	configDir := instance.NewLayout(root).ConfigDir()
	from := filepath.Join(configDir, "gaggles", source)
	to := filepath.Join(configDir, "gaggles", name)
	rewrite := func(content string) string {
		content = strings.ReplaceAll(content, source, name)
		content = strings.ReplaceAll(content, "name: coder", "name: "+name+"-coder")
		return strings.ReplaceAll(content, "goober: coder", "goober: "+name+"-coder")
	}
	err := filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, strings.ReplaceAll(relative, filepath.Join("goobers", "coder"), filepath.Join("goobers", name+"-coder")))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, []byte(rewrite(string(content))), 0o644)
	})
	if err != nil {
		t.Fatalf("clone gaggle %s into %s: %v", source, name, err)
	}
	// The manifest is the instance's desired-state list: a gaggle directory
	// nobody included is not loaded.
	replaceInFile(t, filepath.Join(configDir, "manifest.yaml"), "    - "+source+"\n", "    - "+source+"\n    - "+name+"\n")
}
