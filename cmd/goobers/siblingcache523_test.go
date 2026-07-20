package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #523's CLI-level tests for gather-sibling-context's cross-run sibling
// memo (siblingcache.go): the per-sibling API cost — one files request plus
// two check-state requests — must drop to zero for a sibling whose head SHA
// is unchanged since the last gather, while any change (push/rebase, new PR,
// closed PR) is picked up fresh via the always-fresh list probe.

// gatherSiblingContext runs one `goobers gather-sibling-context` invocation
// in its own fresh working dir (mirroring the live runner: each stage
// dispatch gets its own worktree, only the instance root persists) and
// returns the decoded result plus the stage's stdout.
func gatherSiblingContext(t *testing.T, root string, extraArgs ...string) (siblings []siblingPR, stdout string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	args := append([]string{"gather-sibling-context"}, append(extraArgs, root)...)
	code, out, stderr := runArgs(t, args...)
	if code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, out, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var result struct {
		Siblings []siblingPR `json:"siblings"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal sibling-context.json: %v", err)
	}
	return result.Siblings, out
}

// seedSiblingFixture stands up the shared fixture: selected PR #10 plus two
// green siblings #11 (one file) and #12 (one file).
func seedSiblingFixture(t *testing.T) (root string, server *fakeGitHubServer) {
	t.Helper()
	root = initDemo(t)
	server = newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(10, "goobers/implementation/run-10", "main", "sha10head", "shamainbase",
		false, nil, []fakePRFile{{path: "cmd/goobers/main.go", status: "modified", additions: 3, deletions: 1}})
	server.addOpenPR(11, "goobers/implementation/run-11", "main", "sha11head", "shamainbase",
		false, nil, []fakePRFile{{path: "internal/runner/run.go", status: "modified", additions: 5, deletions: 1}})
	server.addOpenPR(12, "goobers/implementation/run-12", "main", "sha12head", "shamainbase",
		false, nil, []fakePRFile{{path: "providers/github.go", status: "modified", additions: 2, deletions: 0}})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	return root, server
}

// TestGatherSiblingContextUnchangedSiblingsCostZeroCalls is #523's core
// acceptance criterion: a second gather with no sibling changes makes zero
// new per-sibling provider calls — files AND check state both come from the
// memo, so the run's whole API cost is the single list probe.
func TestGatherSiblingContextUnchangedSiblingsCostZeroCalls(t *testing.T) {
	root, server := seedSiblingFixture(t)

	first, _ := gatherSiblingContext(t, root)
	if len(first) != 2 {
		t.Fatalf("first gather siblings = %+v, want #11 and #12", first)
	}
	filesN, checksN := server.requestCounts()
	if filesN != 3 || checksN != 4 {
		// 3 files = 2 siblings + the selected PR's own files, now fetched for
		// deterministic overlap (#989); 4 check-state = 2 per sibling.
		t.Fatalf("first gather cost = %d files + %d check-state requests, want 3 + 4 (nothing cached yet)", filesN, checksN)
	}

	server.resetRequestCounts()
	second, stdout := gatherSiblingContext(t, root)
	filesN, checksN = server.requestCounts()
	if filesN != 0 || checksN != 0 {
		t.Fatalf("unchanged second gather cost = %d files + %d check-state requests, want 0 + 0", filesN, checksN)
	}
	if !strings.Contains(stdout, "2 reused from cache") {
		t.Fatalf("stdout = %q, want it to report 2 reused from cache", stdout)
	}
	// The memoized result must be byte-equivalent evidence, not just cheap:
	// same siblings, same files, same terminal check state.
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("cached gather = %s, want identical to fresh gather %s", secondJSON, firstJSON)
	}
	if second[0].CheckState != "passing" {
		t.Fatalf("cached sibling checkState = %q, want the terminal state recorded on the first gather (passing)", second[0].CheckState)
	}
}

// TestGatherSiblingContextChangedHeadInvalidatesOnlyThatSibling: a sibling
// that was pushed/rebased (new head SHA) is re-fetched; its unchanged
// neighbor still costs nothing.
func TestGatherSiblingContextChangedHeadInvalidatesOnlyThatSibling(t *testing.T) {
	root, server := seedSiblingFixture(t)
	gatherSiblingContext(t, root)

	server.setPRHead(11, "sha11head-v2", []fakePRFile{
		{path: "internal/runner/run.go", status: "modified", additions: 9, deletions: 2},
		{path: "internal/runner/resume.go", status: "added", additions: 40, deletions: 0},
	})
	server.resetRequestCounts()

	siblings, _ := gatherSiblingContext(t, root)
	filesN, checksN := server.requestCounts()
	if filesN != 1 || checksN != 2 {
		t.Fatalf("post-push gather cost = %d files + %d check-state requests, want 1 + 2 (only #11 re-fetched)", filesN, checksN)
	}
	for _, s := range siblings {
		if s.Number == 11 {
			if len(s.Files) != 2 || s.Files[1] != "internal/runner/resume.go" {
				t.Fatalf("sibling #11 files = %v, want the new head's two files", s.Files)
			}
		}
	}
}

// TestGatherSiblingContextRepollsNonTerminalCheckState: files stay memoized
// on an unchanged head, but a pending check state is never reused — it is
// re-polled each run until it settles, then the settled state is reused.
func TestGatherSiblingContextRepollsNonTerminalCheckState(t *testing.T) {
	root, server := seedSiblingFixture(t)
	server.setPRCheckState(11, "pending")
	gatherSiblingContext(t, root)

	// CI finishes on the same head SHA between runs.
	server.setPRCheckState(11, "success")
	server.resetRequestCounts()
	siblings, _ := gatherSiblingContext(t, root)
	filesN, checksN := server.requestCounts()
	if filesN != 0 || checksN != 2 {
		t.Fatalf("pending-sibling gather cost = %d files + %d check-state requests, want 0 + 2 (#11 re-polled, #12 reused)", filesN, checksN)
	}
	for _, s := range siblings {
		if s.Number == 11 && s.CheckState != "passing" {
			t.Fatalf("sibling #11 checkState = %q, want the freshly-polled passing", s.CheckState)
		}
	}

	// Now terminal — the third run reuses it like any other settled sibling.
	server.resetRequestCounts()
	gatherSiblingContext(t, root)
	filesN, checksN = server.requestCounts()
	if filesN != 0 || checksN != 0 {
		t.Fatalf("settled third gather cost = %d files + %d check-state requests, want 0 + 0", filesN, checksN)
	}
}

// TestGatherSiblingContextNoCacheFlagForcesFreshGather: the --no-cache
// escape hatch neither reads nor trusts the memo — every sibling is
// re-fetched as if the cache did not exist.
func TestGatherSiblingContextNoCacheFlagForcesFreshGather(t *testing.T) {
	root, server := seedSiblingFixture(t)
	gatherSiblingContext(t, root)
	server.resetRequestCounts()

	_, stdout := gatherSiblingContext(t, root, "--no-cache")
	filesN, checksN := server.requestCounts()
	if filesN != 3 || checksN != 4 {
		// 3 files = 2 siblings + the selected PR's own files (#989); --no-cache
		// bypasses the memo so nothing is reused.
		t.Fatalf("--no-cache gather cost = %d files + %d check-state requests, want the full fresh 3 + 4", filesN, checksN)
	}
	if !strings.Contains(stdout, "0 reused from cache") {
		t.Fatalf("stdout = %q, want it to report 0 reused from cache", stdout)
	}
}

// TestGatherSiblingContextCorruptCacheDegradesToFreshGather: an unreadable
// memo is an optimization loss, never a stage failure — the gather succeeds
// fresh and the next save repairs the file.
func TestGatherSiblingContextCorruptCacheDegradesToFreshGather(t *testing.T) {
	root, server := seedSiblingFixture(t)
	gatherSiblingContext(t, root)

	cachePath := filepath.Join(layoutFor(root).SchedulerDir(), siblingCacheFileName)
	if err := os.WriteFile(cachePath, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt cache file: %v", err)
	}
	server.resetRequestCounts()

	siblings, _ := gatherSiblingContext(t, root)
	if len(siblings) != 2 {
		t.Fatalf("post-corruption gather siblings = %+v, want #11 and #12", siblings)
	}
	filesN, _ := server.requestCounts()
	if filesN != 3 {
		// 2 siblings + the selected PR's own files (#989); a corrupt cache
		// degrades to a full fresh gather, reusing nothing.
		t.Fatalf("post-corruption gather files requests = %d, want the full fresh 3", filesN)
	}

	// The successful gather rewrote a valid memo — the next run reuses it.
	server.resetRequestCounts()
	gatherSiblingContext(t, root)
	filesN, checksN := server.requestCounts()
	if filesN != 0 || checksN != 0 {
		t.Fatalf("post-repair gather cost = %d files + %d check-state requests, want 0 + 0", filesN, checksN)
	}
}

// TestGatherSiblingContextMemoSurvivesSelectionRotation: merge-review cycles
// through which PR it selects, and the selected PR is a *sibling* from every
// other run's perspective — its still-valid memo must survive the save's
// prune-to-open-set rather than being evicted and re-fetched every rotation.
func TestGatherSiblingContextMemoSurvivesSelectionRotation(t *testing.T) {
	root, server := seedSiblingFixture(t)
	gatherSiblingContext(t, root) // selected #10: memoizes #11, #12

	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "11")
	gatherSiblingContext(t, root) // selected #11: fetches #10, must keep #11's memo

	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	server.resetRequestCounts()
	gatherSiblingContext(t, root)
	filesN, checksN := server.requestCounts()
	if filesN != 0 || checksN != 0 {
		t.Fatalf("post-rotation gather cost = %d files + %d check-state requests, want 0 + 0 (every open PR already memoized)", filesN, checksN)
	}
}

// TestGatherSiblingContextPrunesClosedSiblings: a sibling that closed since
// the last gather drops out of both the result and the memo, and a brand-new
// sibling is fetched fresh — the always-fresh list probe is the source of
// truth for the open set.
func TestGatherSiblingContextPrunesClosedSiblings(t *testing.T) {
	root, server := seedSiblingFixture(t)
	gatherSiblingContext(t, root)

	server.setPRClosed(12)
	server.addOpenPR(13, "goobers/implementation/run-13", "main", "sha13head", "shamainbase",
		false, nil, []fakePRFile{{path: "cmd/goobers/daemon.go", status: "modified", additions: 1, deletions: 1}})
	server.resetRequestCounts()

	siblings, _ := gatherSiblingContext(t, root)
	if len(siblings) != 2 || siblings[0].Number != 11 || siblings[1].Number != 13 {
		t.Fatalf("siblings = %+v, want #11 (cached) and #13 (new)", siblings)
	}
	filesN, checksN := server.requestCounts()
	if filesN != 1 || checksN != 2 {
		t.Fatalf("gather cost = %d files + %d check-state requests, want 1 + 2 (only new #13 fetched)", filesN, checksN)
	}

	data, err := os.ReadFile(filepath.Join(layoutFor(root).SchedulerDir(), siblingCacheFileName))
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	var cache siblingCacheFile
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatalf("unmarshal cache file: %v", err)
	}
	if _, ok := cache.Entries["12"]; ok {
		t.Fatalf("cache entries = %v, want closed #12 pruned", cache.Entries)
	}
	if _, ok := cache.Entries["13"]; !ok {
		t.Fatalf("cache entries = %v, want new #13 memoized", cache.Entries)
	}
}
