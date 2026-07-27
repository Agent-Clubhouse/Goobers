package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testFileURL renders a local path as a file:// URL. The scheme matters for
// partial-clone tests: git silently ignores --filter on plain-path (--local)
// clones, while file:// exercises the packfile transport that honors it.
func testFileURL(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// newFilterableSourceRepo is newSourceRepo plus the server-side opt-in that
// lets a file:// clone of it request an object filter.
func newFilterableSourceRepo(t *testing.T) (dir, url string) {
	t.Helper()
	dir = newSourceRepo(t)
	runTestGit(t, dir, "config", "uploadpack.allowfilter", "true")
	return dir, testFileURL(t, dir)
}

// hardenedGitPrefix restates the exact override prefix hardenedGitArgs
// prepends to every package git invocation, so the byte-identical invocation
// pins below fail loudly if the hardening prefix ever changes shape — the
// safe-bare-repo opt-in (#247) plus the hook/fsmonitor neutralization that
// keeps agent-plantable repo state inert under daemon-side git (S3/#166).
var hardenedGitPrefix = "-c safe.bareRepository=all -c core.hooksPath=" + os.DevNull + " -c core.fsmonitor=false"

// missingObjectCount counts objects reachable from the repo's refs that are
// not present locally — non-zero exactly when a promisor mirror is holding
// back blobs for on-demand fetch.
func missingObjectCount(t *testing.T, dir string) int {
	t.Helper()
	out := runTestGit(t, dir, "rev-list", "--objects", "--missing=print", "--all")
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "?") {
			count++
		}
	}
	return count
}

func advanceOrigin(t *testing.T, repo, filename string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, filename), []byte(filename+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-m", "advance "+filename)
}

func TestManager_WorkingCopy_PartialCloneMirrorIsBlobless(t *testing.T) {
	ctx := context.Background()
	repo, url := newFilterableSourceRepo(t)
	m, err := NewManager(t.TempDir(), WithPartialClone())
	if err != nil {
		t.Fatal(err)
	}

	mirror, err := m.WorkingCopy(ctx, url)
	if err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}
	if !mirrorIsPartial(ctx, mirror) {
		t.Fatal("mirror is not a promisor partial clone under WithPartialClone")
	}
	if missing := missingObjectCount(t, mirror); missing == 0 {
		t.Fatal("blobless mirror holds every object — the clone filter did not apply")
	}

	// The worktree a stage receives must be a complete, content-identical
	// tree: bloblessness is a mirror storage property, never a stage-visible
	// one.
	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: url, RunID: "run-1", BaseRef: "main", Branch: "goobers/wf/run-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt.Path, "README.md"))
	if err != nil {
		t.Fatalf("read provisioned README.md: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(repo, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("provisioned content = %q, want %q", got, want)
	}
	if status := runTestGit(t, wt.Path, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("fresh partial-clone worktree is dirty:\n%s", status)
	}
}

// TestManager_WorkingCopy_PartialCloneNarrowsRefreshFetch pins the narrowed
// refspec by effect: after the blobless clone, a refresh fetch brings over new
// heads and tags but not provider-namespace refs outside refs/heads//refs/tags,
// and keeps holding blobs back.
func TestManager_WorkingCopy_PartialCloneNarrowsRefreshFetch(t *testing.T) {
	ctx := context.Background()
	repo, url := newFilterableSourceRepo(t)
	m, err := NewManager(t.TempDir(), WithPartialClone())
	if err != nil {
		t.Fatal(err)
	}

	mirror, err := m.WorkingCopy(ctx, url)
	if err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}
	missingAfterClone := missingObjectCount(t, mirror)

	advanceOrigin(t, repo, "next.txt")
	runTestGit(t, repo, "update-ref", "refs/tags/v1.0", "refs/heads/main")
	runTestGit(t, repo, "update-ref", "refs/odd/thing", "refs/heads/main")

	if _, err := m.WorkingCopy(ctx, url); err != nil {
		t.Fatalf("WorkingCopy (fetch): %v", err)
	}

	if !mirrorRefExists(t, m, url, "main") {
		t.Fatal("main missing after refresh fetch")
	}
	if got, want := runTestGit(t, mirror, "rev-parse", "refs/tags/v1.0"), runTestGit(t, repo, "rev-parse", "refs/heads/main"); strings.TrimSpace(got) != strings.TrimSpace(want) {
		t.Fatalf("refs/tags/v1.0 = %q, want origin main %q — tags must stay fetched for pinned tag bases", got, want)
	}
	if out := runTestGit(t, mirror, "for-each-ref", "refs/odd/"); strings.TrimSpace(out) != "" {
		t.Fatalf("refs/odd/thing arrived through a narrowed refspec:\n%s", out)
	}
	if missing := missingObjectCount(t, mirror); missing <= missingAfterClone {
		t.Fatalf("missing objects = %d after fetching new commits, want > %d — refresh fetch stopped filtering blobs", missing, missingAfterClone)
	}
}

// TestManager_WorkingCopy_PartialClonePreservesRunBranchNamespaces restates
// the #133/#965 invariant under the narrowed refspec: a local-only run branch
// in a configured namespace survives the refresh prune, while remote-deleted
// branches and stale local branches outside the namespace are still pruned.
func TestManager_WorkingCopy_PartialClonePreservesRunBranchNamespaces(t *testing.T) {
	ctx := context.Background()
	repo, url := newFilterableSourceRepo(t)
	runTestGit(t, repo, "branch", "doomed", "main")
	m, err := NewManager(t.TempDir(), WithPartialClone(), WithRunBranchNamespaces("acme/"))
	if err != nil {
		t.Fatal(err)
	}

	mirror, err := m.WorkingCopy(ctx, url)
	if err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}
	head := strings.TrimSpace(runTestGit(t, mirror, "rev-parse", "refs/heads/main"))
	runTestGit(t, mirror, "update-ref", "refs/heads/acme/remediation/run1", head)
	runTestGit(t, mirror, "update-ref", "refs/heads/goobers/remediation/run1", head)

	runTestGit(t, repo, "branch", "-D", "doomed")
	advanceOrigin(t, repo, "next.txt")

	if _, err := m.WorkingCopy(ctx, url); err != nil {
		t.Fatalf("WorkingCopy (fetch): %v", err)
	}

	if !mirrorRefExists(t, m, url, "acme/remediation/run1") {
		t.Error("configured-namespace run branch was pruned by the narrowed refresh fetch (#133/#965)")
	}
	if mirrorRefExists(t, m, url, "goobers/remediation/run1") {
		t.Error("branch outside the configured namespace survived — the narrowed refspec lost prune semantics")
	}
	if mirrorRefExists(t, m, url, "doomed") {
		t.Error("remote-deleted branch survived — the narrowed refspec lost prune semantics")
	}
}

// TestManager_Create_PartialClonePinnedBaseRefKindsResolve covers the
// WorkingCopy guarantee under partial clone: branch, tag, and reachable-sha
// pinned bases each still provision a worktree.
func TestManager_Create_PartialClonePinnedBaseRefKindsResolve(t *testing.T) {
	ctx := context.Background()
	repo, url := newFilterableSourceRepo(t)
	runTestGit(t, repo, "branch", "feature/x", "main")
	runTestGit(t, repo, "update-ref", "refs/tags/v1.0", "refs/heads/main")
	sha := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "main"))

	m, err := NewManager(t.TempDir(), WithPartialClone())
	if err != nil {
		t.Fatal(err)
	}
	for i, baseRef := range []string{"feature/x", "v1.0", sha} {
		wt, err := m.Create(ctx, CreateOptions{
			RepoURL: url, RunID: fmt.Sprintf("run-%d", i), BaseRef: baseRef,
		})
		if err != nil {
			t.Fatalf("Create(BaseRef=%q): %v", baseRef, err)
		}
		if _, err := os.Stat(filepath.Join(wt.Path, "README.md")); err != nil {
			t.Fatalf("Create(BaseRef=%q) provisioned no tree: %v", baseRef, err)
		}
		if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
			t.Fatalf("Remove(BaseRef=%q): %v", baseRef, err)
		}
	}
}

// TestManager_WorkingCopy_PartialCloneLeavesExistingFullMirrorAlone pins the
// no-migration contract: a mirror cloned before the option was configured
// keeps its full-mirror refresh fetch (every ref, no filter) even under a
// partial-clone Manager.
func TestManager_WorkingCopy_PartialCloneLeavesExistingFullMirrorAlone(t *testing.T) {
	ctx := context.Background()
	repo, url := newFilterableSourceRepo(t)
	root := t.TempDir()

	before, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := before.WorkingCopy(ctx, url)
	if err != nil {
		t.Fatalf("WorkingCopy (full clone): %v", err)
	}
	if missing := missingObjectCount(t, mirror); missing != 0 {
		t.Fatalf("full mirror missing %d objects before the option exists", missing)
	}

	runTestGit(t, repo, "update-ref", "refs/odd/thing", "refs/heads/main")
	after, err := NewManager(root, WithPartialClone())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := after.WorkingCopy(ctx, url); err != nil {
		t.Fatalf("WorkingCopy (refresh under WithPartialClone): %v", err)
	}
	if out := runTestGit(t, mirror, "for-each-ref", "refs/odd/"); strings.TrimSpace(out) == "" {
		t.Fatal("pre-existing full mirror stopped fetching every ref — the option migrated an existing mirror")
	}
	if missing := missingObjectCount(t, mirror); missing != 0 {
		t.Fatalf("pre-existing full mirror is missing %d objects after refresh", missing)
	}
}

// TestManager_Create_PartialCloneBlobFetchFailureFailsClosedAndTransient: the
// checkout-time blob backfill is the new network-dependent step (#646). When
// the promisor remote cannot serve a blob, Create must fail closed (no
// worktree handed out) with an error the runner's bounded infrastructure
// retry recognizes.
func TestManager_Create_PartialCloneBlobFetchFailureFailsClosedAndTransient(t *testing.T) {
	ctx := context.Background()
	repo, url := newFilterableSourceRepo(t)
	m, err := NewManager(t.TempDir(), WithPartialClone())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.WorkingCopy(ctx, url); err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}

	// Remove the README blob from the source's object store. The refresh
	// fetch inside Create still succeeds (refs are unchanged, no objects
	// negotiated), so the failure surfaces exactly where production would
	// hit it: the promisor blob fetch during `git worktree add`'s checkout.
	blobOID := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "main:README.md"))
	objPath := filepath.Join(repo, ".git", "objects", blobOID[:2], blobOID[2:])
	// Loose objects are written read-only; lift that first so the removal
	// also works on Windows, where unlink honors the read-only attribute.
	if err := os.Chmod(objPath, 0o644); err != nil {
		t.Fatalf("chmod source blob object: %v", err)
	}
	if err := os.Remove(objPath); err != nil {
		t.Fatalf("remove source blob object: %v", err)
	}

	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: url, RunID: "run-1", BaseRef: "main", Branch: "goobers/wf/run-1",
	})
	if err == nil {
		t.Fatal("Create succeeded with an unservable blob — a partial tree was handed out")
	}
	if wt != nil {
		t.Fatalf("Create returned a worktree alongside its error: %+v", wt)
	}
	if !IsTransientProvisionError(err) {
		t.Fatalf("blob-fetch failure not classified for infrastructure retry: %v", err)
	}
}

// gitTraceEnv returns a WithGitEnvironment resolver whose environment carries
// GIT_TRACE into every credential-seam git invocation: a command appears in
// the trace file iff it ran with the resolver-provided environment, which is
// how these tests pin that blob-materializing operations route through the
// credential seam rather than the ambient environment.
func gitTraceEnv(traceFile string) func(context.Context, string) ([]string, error) {
	return func(context.Context, string) ([]string, error) {
		return append(os.Environ(), "GIT_TRACE="+traceFile), nil
	}
}

func readTrace(t *testing.T, traceFile string) string {
	t.Helper()
	raw, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatalf("read GIT_TRACE file: %v", err)
	}
	return string(raw)
}

// TestManager_Create_PartialCloneSyncBaseMergeCarriesCredentialEnvironment:
// merging a base that advanced after the branch was cut materializes base-side
// blobs the narrowed refresh fetch withheld, so on a blobless mirror the
// SyncBase merge must run through the credential seam like the checkout does —
// on a private repo the promisor fetch fails on auth otherwise.
func TestManager_Create_PartialCloneSyncBaseMergeCarriesCredentialEnvironment(t *testing.T) {
	ctx := context.Background()
	repo, url := newFilterableSourceRepo(t)
	traceFile := filepath.Join(t.TempDir(), "trace")
	m, err := NewManager(t.TempDir(), WithPartialClone(), WithGitEnvironment(gitTraceEnv(traceFile)))
	if err != nil {
		t.Fatal(err)
	}

	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: url, RunID: "run-1", BaseRef: "main", Branch: "goobers/wf/run-1",
	})
	if err != nil {
		t.Fatalf("Create (cut branch): %v", err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatal(err)
	}

	advanceOrigin(t, repo, "advanced.txt")
	if err := os.WriteFile(traceFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err = m.Create(ctx, CreateOptions{
		RepoURL: url, RunID: "run-2", BaseRef: "main", Branch: "goobers/wf/run-1", SyncBase: true,
	})
	if err != nil {
		t.Fatalf("Create (SyncBase after base advanced): %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt.Path, "advanced.txt"))
	if err != nil {
		t.Fatalf("read merged base file — the merge did not backfill base-side blobs: %v", err)
	}
	if string(got) != "advanced.txt\n" {
		t.Fatalf("merged base file = %q, want %q", got, "advanced.txt\n")
	}
	if trace := readTrace(t, traceFile); !strings.Contains(trace, "merge --ff --no-edit main") {
		t.Fatalf("SyncBase merge did not run with the credential environment; trace:\n%s", trace)
	}
}

// preparePartialCloneReboundBranch builds the rebound-PR shape (#392 +
// RequireExistingBranch) on a blobless mirror: a PR branch fetched into the
// run-branch namespace, whose merge-base-side blob (main's README) no checkout
// ever materialized — the shape where Worktree.Diff must backfill from the
// promisor remote.
func preparePartialCloneReboundBranch(t *testing.T, ctx context.Context, m *Manager, repo, url string) *Worktree {
	t.Helper()
	mirror, err := m.WorkingCopy(ctx, url)
	if err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}
	runTestGit(t, repo, "switch", "-c", "pr")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("goodbye\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-m", "pr change")
	runTestGit(t, repo, "switch", "main")
	// An earlier stage's fetch of the PR branch honors the mirror's configured
	// partial-clone filter, so the branch arrives commits-and-trees only.
	runTestGit(t, mirror, "fetch", "origin", "+refs/heads/pr:refs/heads/goobers/wf/run-1")

	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: url, RunID: "run-1", BaseRef: "main",
		Branch: "goobers/wf/run-1", RequireExistingBranch: true,
	})
	if err != nil {
		t.Fatalf("Create (rebound branch): %v", err)
	}
	return wt
}

// TestWorktree_Diff_PartialCloneBackfillsBaseBlobsWithCredentialEnvironment:
// on a rebound-PR worktree only the PR tip's blobs were ever materialized, so
// `git diff base...HEAD` fetches the merge-base side from the promisor remote
// — it must succeed and must do so through the credential seam.
func TestWorktree_Diff_PartialCloneBackfillsBaseBlobsWithCredentialEnvironment(t *testing.T) {
	ctx := context.Background()
	repo, url := newFilterableSourceRepo(t)
	traceFile := filepath.Join(t.TempDir(), "trace")
	m, err := NewManager(t.TempDir(), WithPartialClone(), WithGitEnvironment(gitTraceEnv(traceFile)))
	if err != nil {
		t.Fatal(err)
	}
	wt := preparePartialCloneReboundBranch(t, ctx, m, repo, url)

	if err := os.WriteFile(traceFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := wt.Diff(ctx, "main")
	if err != nil {
		t.Fatalf("Diff on a rebound partial-clone worktree: %v", err)
	}
	if !strings.Contains(string(diff), "-hello") || !strings.Contains(string(diff), "+goodbye") {
		t.Fatalf("diff does not carry the base...HEAD change:\n%s", diff)
	}
	if trace := readTrace(t, traceFile); !strings.Contains(trace, "diff main...HEAD") {
		t.Fatalf("Diff did not run with the credential environment; trace:\n%s", trace)
	}
}

// TestWorktree_Diff_PartialClonePromisorFailureClassifiesTransient: when the
// promisor remote cannot serve the merge-base blob mid-diff, the error must
// classify through IsTransientProvisionError (a typed *gitCommandError), not
// surface as an opaque exec failure the runner can only fail the run on.
func TestWorktree_Diff_PartialClonePromisorFailureClassifiesTransient(t *testing.T) {
	ctx := context.Background()
	repo, url := newFilterableSourceRepo(t)
	m, err := NewManager(t.TempDir(), WithPartialClone())
	if err != nil {
		t.Fatal(err)
	}
	blobOID := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "main:README.md"))
	wt := preparePartialCloneReboundBranch(t, ctx, m, repo, url)

	// Remove the merge-base README blob from the source's object store, same
	// mechanics as the Create-time fail-closed test: the next promisor fetch
	// for it cannot be served.
	objPath := filepath.Join(repo, ".git", "objects", blobOID[:2], blobOID[2:])
	if err := os.Chmod(objPath, 0o644); err != nil {
		t.Fatalf("chmod source blob object: %v", err)
	}
	if err := os.Remove(objPath); err != nil {
		t.Fatalf("remove source blob object: %v", err)
	}

	diff, err := wt.Diff(ctx, "main")
	if err == nil {
		t.Fatalf("Diff succeeded with an unservable merge-base blob:\n%s", diff)
	}
	if !IsTransientProvisionError(err) {
		t.Fatalf("mid-diff promisor failure not classified for infrastructure retry: %v", err)
	}
}

// TestManager_WorkingCopy_PartialCloneOffIsByteIdentical records every git
// invocation through a PATH shim and pins the flag-off clone and refresh
// fetch to today's exact command lines — the #646 acceptance criterion that
// an unconfigured instance issues byte-identical git invocations.
func TestManager_WorkingCopy_PartialCloneOffIsByteIdentical(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX git shim")
	}
	ctx := context.Background()
	repo := newSourceRepo(t)
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}

	log := installRecordingGitShim(t)
	mirror, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}
	if _, err := m.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("WorkingCopy (fetch): %v", err)
	}

	recorded := recordedGitLines(t, log)
	wantClone := hardenedGitPrefix + " clone --mirror " + repo + " " + mirror
	wantFetch := hardenedGitPrefix + " fetch --prune origin +refs/*:refs/* ^refs/heads/goobers/*"
	if got := findRecordedLine(recorded, " clone "); got != wantClone {
		t.Errorf("flag-off clone invocation:\n got %q\nwant %q", got, wantClone)
	}
	if got := findRecordedLine(recorded, " fetch "); got != wantFetch {
		t.Errorf("flag-off fetch invocation:\n got %q\nwant %q", got, wantFetch)
	}
	for _, line := range recorded {
		if strings.Contains(line, "--filter") || strings.Contains(line, "promisor") {
			t.Errorf("flag-off path issued a partial-clone invocation: %q", line)
		}
	}
}

// TestManager_WorkingCopy_PartialCloneOnInvocations is the companion shim
// test for the flag-on shape: blobless clone, heads+tags refresh refspec, and
// the unchanged namespace exclusion.
func TestManager_WorkingCopy_PartialCloneOnInvocations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX git shim")
	}
	ctx := context.Background()
	_, url := newFilterableSourceRepo(t)
	m, err := NewManager(t.TempDir(), WithPartialClone())
	if err != nil {
		t.Fatal(err)
	}

	log := installRecordingGitShim(t)
	mirror, err := m.WorkingCopy(ctx, url)
	if err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}
	if _, err := m.WorkingCopy(ctx, url); err != nil {
		t.Fatalf("WorkingCopy (fetch): %v", err)
	}

	recorded := recordedGitLines(t, log)
	wantClone := hardenedGitPrefix + " clone --mirror --filter=blob:none " + url + " " + mirror
	wantFetch := hardenedGitPrefix + " fetch --prune origin +refs/heads/*:refs/heads/* +refs/tags/*:refs/tags/* ^refs/heads/goobers/*"
	if got := findRecordedLine(recorded, " clone "); got != wantClone {
		t.Errorf("flag-on clone invocation:\n got %q\nwant %q", got, wantClone)
	}
	if got := findRecordedLine(recorded, " fetch "); got != wantFetch {
		t.Errorf("flag-on fetch invocation:\n got %q\nwant %q", got, wantFetch)
	}
}

// installRecordingGitShim prepends a git wrapper to PATH that appends each
// invocation's arguments to the returned log file before delegating to the
// real git binary.
func installRecordingGitShim(t *testing.T) (logPath string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	logPath = filepath.Join(binDir, "git-invocations.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GOOBERS_GIT_SHIM_LOG\"\nexec \"$GOOBERS_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_GIT_SHIM_LOG", logPath)
	t.Setenv("GOOBERS_REAL_GIT", realGit)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func recordedGitLines(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read git shim log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func findRecordedLine(lines []string, marker string) string {
	for _, line := range lines {
		if strings.Contains(line, marker) {
			return line
		}
	}
	return ""
}
