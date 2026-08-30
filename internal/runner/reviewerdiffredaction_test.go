package runner

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
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
