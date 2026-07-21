package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
)

// churnRepo is a temp git repo whose commits are stamped at controlled times so
// the watermark + buffer window math is exercised deterministically.
type churnRepo struct {
	t   *testing.T
	dir string
}

func newChurnRepo(t *testing.T) *churnRepo {
	t.Helper()
	r := &churnRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.email", "docs@example.test")
	r.git("config", "user.name", "docs")
	return r
}

func (r *churnRepo) git(args ...string) {
	r.t.Helper()
	r.gitAt(time.Time{}, args...)
}

// gitAt runs a git command with author/committer dates pinned to when (when
// non-zero) so commits land at a known point on the timeline.
func (r *churnRepo) gitAt(when time.Time, args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = os.Environ()
	if !when.IsZero() {
		stamp := when.UTC().Format(time.RFC3339)
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// commit writes files and commits them at time when, returning the new HEAD SHA.
func (r *churnRepo) commit(when time.Time, msg string, files map[string]string) string {
	r.t.Helper()
	for rel, content := range files {
		full := filepath.Join(r.dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.gitAt(when, "commit", "-q", "-m", msg)
	out, err := exec.Command("git", "-C", r.dir, "rev-parse", "HEAD").Output()
	if err != nil {
		r.t.Fatalf("rev-parse HEAD: %v", err)
	}
	return string(out[:len(out)-1]) // trim trailing newline
}

// runChurn invokes the docs-churn stage against repo r, writing the digest to a
// result file it then decodes. instanceRoot holds the watermark.
func (r *churnRepo) runChurn(t *testing.T, instanceRoot string, extraArgs ...string) (int, docsChurnDigest, string) {
	t.Helper()
	resultFile := filepath.Join(t.TempDir(), "docs-churn.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)
	args := append([]string{"docs-churn", "--repo", r.dir, "--workflow", "docs-updater", "--gaggle", "goobers"}, extraArgs...)
	args = append(args, instanceRoot)
	code, _, stderr := runArgs(t, args...)
	var digest docsChurnDigest
	if data, err := os.ReadFile(resultFile); err == nil {
		if err := json.Unmarshal(data, &digest); err != nil {
			t.Fatalf("decode digest: %v\n%s", err, data)
		}
	}
	return code, digest, stderr
}

func changedContains(digest docsChurnDigest, path string) bool {
	for _, f := range digest.ChangedFiles {
		if f == path {
			return true
		}
	}
	return false
}

func watermarkPath(root string) string {
	return instance.NewLayout(root).DocsWatermarkPath("goobers", "docs-updater")
}

// TestDocsChurnFirstRunBoundedWindowAndAdvancesWatermark: with no watermark the
// stage bounds itself to the since-floor window (not all history), reports the
// churn in it, and advances the watermark to HEAD so the next run starts here.
func TestDocsChurnFirstRunBoundedWindowAndAdvancesWatermark(t *testing.T) {
	unsetRunContext(t)
	now := time.Now().UTC()
	r := newChurnRepo(t)
	r.commit(now.Add(-200*time.Hour), "old base", map[string]string{"README.md": "base\n"})
	r.commit(now.Add(-10*time.Hour), "recent docs", map[string]string{"docs/guide.md": "guide\n"})
	head := r.commit(now.Add(-1*time.Hour), "recent code", map[string]string{"internal/svc/y.go": "package svc\n"})

	instanceRoot := t.TempDir()
	code, digest, stderr := r.runChurn(t, instanceRoot, "--since", "168h")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
	}
	if !digest.FirstRun {
		t.Errorf("firstRun = false; want true (no watermark yet)")
	}
	if !changedContains(digest, "docs/guide.md") || !changedContains(digest, "internal/svc/y.go") {
		t.Errorf("changedFiles = %v; want the two in-window changes", digest.ChangedFiles)
	}
	if changedContains(digest, "README.md") {
		t.Errorf("changedFiles = %v; the 200h-old base is outside the 168h window", digest.ChangedFiles)
	}
	if digest.NoWork {
		t.Errorf("noWork = true; want false (there is churn)")
	}
	// Watermark advanced to HEAD.
	wm, have, err := readDocsWatermark(watermarkPath(instanceRoot))
	if err != nil || !have {
		t.Fatalf("watermark not written: have=%v err=%v", have, err)
	}
	if wm.SHA != head {
		t.Errorf("watermark sha = %q; want HEAD %q", wm.SHA, head)
	}
	if wm.Workflow != "docs-updater" || wm.Gaggle != "goobers" {
		t.Errorf("watermark identity = %q/%q; want goobers/docs-updater", wm.Gaggle, wm.Workflow)
	}
}

// TestDocsChurnSecondRunAnchorsWindowOnWatermark: a pre-seeded (older) watermark
// drives the window — the far edge is watermark.refreshedAt − buffer, so the run
// reaches back across the watermark (buffer overlap, no gap) yet stays bounded
// well short of full history, and it advances the watermark to the new HEAD.
func TestDocsChurnSecondRunAnchorsWindowOnWatermark(t *testing.T) {
	unsetRunContext(t)
	now := time.Now().UTC()
	r := newChurnRepo(t)
	r.commit(now.Add(-24*time.Hour), "base", map[string]string{"README.md": "base\n"})
	r.commit(now.Add(-10*time.Hour), "too old", map[string]string{"docs/old.md": "old\n"})
	r.commit(now.Add(-4*time.Hour), "before watermark, within buffer", map[string]string{"docs/mid.md": "mid\n"})
	head := r.commit(now.Add(-2*time.Hour), "after watermark", map[string]string{"docs/new.md": "new\n"})

	instanceRoot := t.TempDir()
	// Seed a watermark refreshed 3h ago. floor 1h, multiplier 1 ->
	// buffer = max(1 * ~3h, 1h) ~= 3h -> window edge ~= now-6h. So the 4h and 2h
	// commits are in-window; the 10h commit is not.
	seed := docsWatermark{
		Schema:      docsWatermarkSchemaVersion,
		Gaggle:      "goobers",
		Workflow:    "docs-updater",
		SHA:         "0000000000000000000000000000000000000000",
		RefreshedAt: now.Add(-3 * time.Hour),
	}
	if err := writeDocsWatermark(watermarkPath(instanceRoot), seed); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	code, digest, stderr := r.runChurn(t, instanceRoot, "--since", "1h", "--buffer-multiplier", "1")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
	}
	if digest.FirstRun {
		t.Errorf("firstRun = true; want false (watermark seeded)")
	}
	if digest.Watermark == nil || digest.Watermark.RefreshedAt.UTC() != seed.RefreshedAt {
		t.Errorf("digest watermark = %+v; want the seeded refreshedAt %v", digest.Watermark, seed.RefreshedAt)
	}
	if !changedContains(digest, "docs/new.md") {
		t.Errorf("changedFiles = %v; the after-watermark change must be in-window (no gap)", digest.ChangedFiles)
	}
	if !changedContains(digest, "docs/mid.md") {
		t.Errorf("changedFiles = %v; the before-watermark change is within the buffer overlap", digest.ChangedFiles)
	}
	if changedContains(digest, "docs/old.md") {
		t.Errorf("changedFiles = %v; the 10h-old change is outside the ~6h buffer window", digest.ChangedFiles)
	}
	// Watermark advanced past the seed to the current HEAD.
	wm, _, err := readDocsWatermark(watermarkPath(instanceRoot))
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if wm.SHA != head {
		t.Errorf("watermark sha = %q; want advanced to HEAD %q", wm.SHA, head)
	}
	if !wm.RefreshedAt.After(seed.RefreshedAt) {
		t.Errorf("watermark refreshedAt = %v; want advanced past seed %v", wm.RefreshedAt, seed.RefreshedAt)
	}
}

// TestDocsChurnEmptyChurnReportsNoWork: when the window holds no churn, the
// digest is emitted with noWork=true (the ResultNoWork signal shell.go reads),
// exit 0.
func TestDocsChurnEmptyChurnReportsNoWork(t *testing.T) {
	unsetRunContext(t)
	now := time.Now().UTC()
	r := newChurnRepo(t)
	r.commit(now.Add(-48*time.Hour), "only old work", map[string]string{"README.md": "base\n"})

	instanceRoot := t.TempDir()
	resultFile := filepath.Join(t.TempDir(), "docs-churn.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)
	code, _, stderr := runArgs(t, "docs-churn", "--repo", r.dir, "--workflow", "docs-updater",
		"--gaggle", "goobers", "--since", "1h", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
	}
	raw, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	// Assert the raw JSON carries noWork:true — the exact key shell.go reads to
	// downgrade the stage to ResultNoWork.
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if nw, _ := out[executor.OutputNoWork].(bool); !nw {
		t.Errorf("result %s has no noWork:true; keys=%v", raw, out)
	}
	if len(out["changedFiles"].([]any)) != 0 {
		t.Errorf("changedFiles = %v; want empty", out["changedFiles"])
	}
}

// TestDocsChurnGroupsDocsRootChanges: with docsRoots declared as an input, the
// digest flags which churn landed under a documentation root and groups the
// rest by area.
func TestDocsChurnGroupsDocsRootChanges(t *testing.T) {
	unsetRunContext(t)
	now := time.Now().UTC()
	r := newChurnRepo(t)
	r.commit(now.Add(-30*time.Hour), "base", map[string]string{"README.md": "base\n"})
	r.commit(now.Add(-2*time.Hour), "mixed change", map[string]string{
		"docs/guide.md":     "guide\n",
		"internal/svc/y.go": "package svc\n",
		"README.md":         "updated\n",
	})

	instanceRoot := t.TempDir()
	t.Setenv(executor.InputEnvVar("docsRoots"), "docs, README.md")
	code, digest, stderr := r.runChurn(t, instanceRoot, "--since", "168h")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
	}
	wantDocs := map[string]bool{"README.md": true, "docs/guide.md": true}
	if len(digest.DocsRootChanges) != len(wantDocs) {
		t.Fatalf("docsRootChanges = %v; want %v", digest.DocsRootChanges, wantDocs)
	}
	for _, f := range digest.DocsRootChanges {
		if !wantDocs[f] {
			t.Errorf("docsRootChanges has unexpected %q", f)
		}
	}
	if _, ok := digest.Areas["internal"]; !ok {
		t.Errorf("areas = %v; want an 'internal' bucket", digest.Areas)
	}
	if _, ok := digest.Areas["docs"]; !ok {
		t.Errorf("areas = %v; want a 'docs' bucket", digest.Areas)
	}
}

// TestDocsChurnAdvanceWatermarkFalseLeavesWatermark: advanceWatermark=false is
// the opt-out for a caller that advances the watermark elsewhere — the stage
// emits the digest but never writes the watermark.
func TestDocsChurnAdvanceWatermarkFalseLeavesWatermark(t *testing.T) {
	unsetRunContext(t)
	now := time.Now().UTC()
	r := newChurnRepo(t)
	r.commit(now.Add(-2*time.Hour), "change", map[string]string{"docs/x.md": "x\n"})

	instanceRoot := t.TempDir()
	t.Setenv(executor.InputEnvVar("advanceWatermark"), "false")
	code, _, stderr := r.runChurn(t, instanceRoot, "--since", "168h")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
	}
	if _, have, _ := readDocsWatermark(watermarkPath(instanceRoot)); have {
		t.Errorf("watermark written despite advanceWatermark=false")
	}
}

// TestDocsChurnRequiresWorkflow: the watermark is per-workflow, so the stage
// fails closed (usage error) when neither --workflow nor $GOOBERS_WORKFLOW names
// one.
func TestDocsChurnRequiresWorkflow(t *testing.T) {
	unsetRunContext(t)
	r := newChurnRepo(t)
	r.commit(time.Now().UTC().Add(-1*time.Hour), "c", map[string]string{"docs/x.md": "x\n"})
	code, _, stderr := runArgs(t, "docs-churn", "--repo", r.dir, t.TempDir())
	if code != 2 {
		t.Fatalf("code = %d, want 2 (workflow required); stderr = %q", code, stderr)
	}
}

// TestDocsChurnRejectsBadFlags: usage errors for a non-positive since, a
// sub-1 multiplier, and an unknown format.
func TestDocsChurnRejectsBadFlags(t *testing.T) {
	unsetRunContext(t)
	r := newChurnRepo(t)
	r.commit(time.Now().UTC().Add(-1*time.Hour), "c", map[string]string{"docs/x.md": "x\n"})
	for _, args := range [][]string{
		{"docs-churn", "--repo", r.dir, "--workflow", "w", "--since", "0s", t.TempDir()},
		{"docs-churn", "--repo", r.dir, "--workflow", "w", "--buffer-multiplier", "0.5", t.TempDir()},
		{"docs-churn", "--repo", r.dir, "--workflow", "w", "--format", "bogus", t.TempDir()},
	} {
		code, _, stderr := runArgs(t, args...)
		if code != 2 {
			t.Errorf("args %v: code = %d, want 2; stderr = %q", args, code, stderr)
		}
	}
}

// TestDocsWatermarkPathSanitizesIdentity: the watermark file name is a single,
// safe path component even for an empty gaggle or a name with separators, so a
// bad env var can never redirect it out of its directory.
func TestDocsWatermarkPathSanitizesIdentity(t *testing.T) {
	l := instance.NewLayout("/inst")
	got := l.DocsWatermarkPath("", "docs-updater")
	want := filepath.Join("/inst", "scheduler", "docs-updater", "___docs-updater.json")
	if got != want {
		t.Errorf("DocsWatermarkPath(empty gaggle) = %q; want %q", got, want)
	}
	got = l.DocsWatermarkPath("a/b", "c/d")
	if dir := filepath.Dir(got); dir != filepath.Join("/inst", "scheduler", "docs-updater") {
		t.Errorf("watermark path %q escaped its dir (%q)", got, dir)
	}
}
