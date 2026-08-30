package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// reviewerDiffJournal creates a real run journal (satisfying executionJournal)
// and returns it with its run directory.
func reviewerDiffJournal(t *testing.T, runID string) (*journal.Run, string) {
	t.Helper()
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	t.Cleanup(func() { _ = jr.Close() })
	return jr, filepath.Join(runsDir, runID)
}

func reviewerDiffAnnotations(t *testing.T, dir string) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var out []journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventRunnerAnnotation && ev.Name == ReviewerDiffRedactionAnnotation {
			out = append(out, ev)
		}
	}
	return out
}

// TestReviewerDiffRedactionRecorded is the #3135 correlation regression: when
// scrubbing transforms the diff a review gate reads, the run journal must say
// so and carry both digests, so a reviewer finding about redacted content can
// be told apart from a finding about the branch's authoritative raw diff.
func TestReviewerDiffRedactionRecorded(t *testing.T) {
	jr, dir := reviewerDiffJournal(t, "run-reviewer-diff-redacted")

	raw := []byte("+\tconst want = \"Bearer abcdefghijklmnopqrstuvwxyz0123456789\"\n")
	ref, err := jr.RecordArtifact("reviewer-diff", raw)
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	rawDigest := journal.Digest(raw)
	if ref.Digest == rawDigest {
		t.Fatal("journal recorded the credential unscrubbed; the fixture no longer exercises redaction")
	}
	if err := recordReviewerDiffRedaction(jr, "review", ref, rawDigest, len(raw)); err != nil {
		t.Fatalf("recordReviewerDiffRedaction: %v", err)
	}

	hits := reviewerDiffAnnotations(t, dir)
	if len(hits) != 1 {
		t.Fatalf("annotation events = %d, want 1", len(hits))
	}
	ev := hits[0]
	if got := ev.Runner["rawDigest"]; got != rawDigest {
		t.Fatalf("rawDigest = %v, want %v", got, rawDigest)
	}
	if got := ev.Runner["evidenceDigest"]; got != ref.Digest {
		t.Fatalf("evidenceDigest = %v, want %v", got, ref.Digest)
	}
	if got := ev.Runner["rawBytes"]; got != float64(len(raw)) {
		t.Fatalf("rawBytes = %v, want %d", got, len(raw))
	}
	if ev.Stage != "review" {
		t.Fatalf("stage = %q, want %q", ev.Stage, "review")
	}
}

// TestReviewerDiffRedactionSilentWhenUntransformed keeps the annotation
// meaningful: evidence that is byte-identical to the raw diff records nothing.
func TestReviewerDiffRedactionSilentWhenUntransformed(t *testing.T) {
	jr, dir := reviewerDiffJournal(t, "run-reviewer-diff-clean")

	raw := []byte("+\treturn fmt.Errorf(\"no credentials here\")\n")
	ref, err := jr.RecordArtifact("reviewer-diff", raw)
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	if err := recordReviewerDiffRedaction(jr, "review", ref, journal.Digest(raw), len(raw)); err != nil {
		t.Fatalf("recordReviewerDiffRedaction: %v", err)
	}
	if hits := reviewerDiffAnnotations(t, dir); len(hits) != 0 {
		t.Fatalf("annotation events = %d, want 0", len(hits))
	}
}

// TestRecordReviewerDiffJournalsRedaction exercises the annotation through
// recordReviewerDiff itself — the wiring, not just the helper — so a refactor
// that drops the call from the reviewer-evidence path fails here: a diff whose
// commit captured a registered secret must reach the gate scrubbed AND leave
// the correlation record behind.
func TestRecordReviewerDiffJournalsRedaction(t *testing.T) {
	jr, dir := reviewerDiffJournal(t, "run-reviewer-diff-wiring")

	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main", repo)
	runGit(t, repo, "config", "user.name", "t")
	runGit(t, repo, "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "seed.txt")
	runGit(t, repo, "commit", "-q", "-m", "seed")
	runGit(t, repo, "checkout", "-q", "-b", "goobers/run-reviewer-diff-wiring")
	if err := os.WriteFile(filepath.Join(repo, "changed.txt"), []byte("const token = \"secret-value-0123456789\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "changed.txt")
	runGit(t, repo, "commit", "-q", "-m", "change")

	reg := journal.NewRegistryScrubber()
	reg.Register([]byte("secret-value-0123456789"))
	ex := newExecutors(Config{}, jr, reg)
	wt := &worktree.Worktree{RunID: "run-reviewer-diff-wiring", Path: repo, Branch: "goobers/run-reviewer-diff-wiring"}

	r := &Runner{}
	ptr, err := r.recordReviewerDiff(context.Background(), jr, ex, StartInput{
		RunID:   "run-reviewer-diff-wiring",
		RepoRef: apiv1.RepoRef{Branch: "main"},
	}, "review", wt)
	if err != nil || ptr == nil || ptr.Artifact == nil {
		t.Fatalf("recordReviewerDiff: pointer %+v err %v, want the diff evidence", ptr, err)
	}

	hits := reviewerDiffAnnotations(t, dir)
	if len(hits) != 1 {
		t.Fatalf("reviewer-diff-redacted events = %d, want 1", len(hits))
	}
	if got := hits[0].Runner["evidenceDigest"]; got != ptr.Artifact.Digest {
		t.Fatalf("evidenceDigest = %v, want the pointer's digest %s", got, ptr.Artifact.Digest)
	}
	raw, _ := hits[0].Runner["rawDigest"].(string)
	if raw == "" || raw == ptr.Artifact.Digest {
		t.Fatalf("rawDigest = %q, want the pre-scrub digest, distinct from the evidence digest", raw)
	}
}
