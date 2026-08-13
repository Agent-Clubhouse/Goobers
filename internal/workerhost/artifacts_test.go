package workerhost

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type fixedScrubber struct{}

func (fixedScrubber) Scrub(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), "s3cret", "«redacted»"))
}

func TestRecordArtifactScrubsAndDeduplicates(t *testing.T) {
	dir := t.TempDir()
	s := NewStagingArtifacts(dir, fixedScrubber{}, nil)

	ref, err := s.RecordArtifact("stdout.log", []byte("token=s3cret done"))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	if ref.Digest == "" || !strings.HasPrefix(ref.Digest, "sha256:") {
		t.Fatalf("digest = %q, want sha256:…", ref.Digest)
	}
	if ref.Integrity != apiv1.IntegrityDerived {
		t.Fatalf("integrity = %q, want derived", ref.Integrity)
	}

	// Same SCRUBBED content must deduplicate to the same blob: the digest
	// commits to what was stored, not to what was handed in.
	again, err := s.RecordArtifact("other.log", []byte("token=s3cret done"))
	if err != nil {
		t.Fatalf("RecordArtifact again: %v", err)
	}
	if again.Digest != ref.Digest {
		t.Fatalf("identical content produced different digests: %s vs %s", ref.Digest, again.Digest)
	}
}

func TestRecordArtifactBoundedTruncatesAfterScrubbing(t *testing.T) {
	s := NewStagingArtifacts(t.TempDir(), fixedScrubber{}, nil)
	ref, err := s.RecordArtifactBounded("big.log", []byte("aaaaaaaaaaaaaaaaaaaa"), 4)
	if err != nil {
		t.Fatalf("RecordArtifactBounded: %v", err)
	}
	if ref.Size != 4 {
		t.Fatalf("Size = %d, want 4 — the digest must commit to the stored bytes", ref.Size)
	}
	if _, err := s.RecordArtifactBounded("bad.log", []byte("x"), 0); err == nil {
		t.Fatal("a non-positive byte limit must be rejected, matching journal.Run")
	}
}
