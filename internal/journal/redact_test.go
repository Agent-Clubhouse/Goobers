package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// leak is a plaintext secret shaped so the default pattern net does NOT catch it
// — simulating a miss that reaches disk and must be remediated by `goobers
// journal redact`.
const leak = "PLAINTEXT-LEAK-do-not-store-2f0a"

func TestRedactRemediatesLeakedArtifact(t *testing.T) {
	reg, scrub := DefaultScrubber() // secret not registered yet → it leaks through
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithScrubber(scrub), WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	oldRef, err := run.RecordArtifact("config.env", []byte("TOKEN="+leak+"\n"))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	dir := filepath.Join(root, testIdentity().RunID)
	if hits := filesContaining(t, dir, []byte(leak)); len(hits) == 0 {
		t.Fatalf("precondition failed: leak should be at rest before redaction")
	}

	// The operator learns the secret and registers it, then redacts.
	reg.Register([]byte(leak))
	newRef, err := run.Redact(oldRef, "token leaked into config.env")
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if newRef.Digest == oldRef.Digest {
		t.Fatalf("redaction did not change the digest")
	}
	_ = run.Close()

	// Leak is gone everywhere, including the old blob path.
	if hits := filesContaining(t, dir, []byte(leak)); len(hits) > 0 {
		t.Fatalf("leak survived redaction in: %v", hits)
	}
	if _, err := os.Stat(filepath.Join(dir, oldRef.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old leaked blob not removed: %v", err)
	}

	// A redaction event records the old→new digests, so the exception is traced.
	rd, _ := OpenRead(dir)
	events, _ := rd.Events()
	last := events[len(events)-1]
	if last.Type != EventRedaction || last.Redaction == nil {
		t.Fatalf("no redaction event appended: %+v", last)
	}
	if last.Redaction.OldDigest != oldRef.Digest || last.Redaction.NewDigest != newRef.Digest {
		t.Fatalf("redaction event digests wrong: %+v", last.Redaction)
	}

	// The redacted blob verifies and no longer holds the leak.
	got, err := rd.ArtifactBytes(newRef)
	if err != nil {
		t.Fatalf("ArtifactBytes(new): %v", err)
	}
	if bytes.Contains(got, []byte(leak)) {
		t.Fatalf("redacted blob still contains leak")
	}
}

// TestRedactRefusesWhenNothingChanges guards against a no-op redaction: if the
// scrubber does not recognize the material, Redact fails loudly rather than
// silently rewriting an identical blob.
func TestRedactRefusesWhenNothingChanges(t *testing.T) {
	_, scrub := DefaultScrubber()
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithScrubber(scrub), WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ref, err := run.RecordArtifact("clean.txt", []byte("nothing secret here"))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	if _, err := run.Redact(ref, "no reason"); !errors.Is(err, ErrNothingRedacted) {
		t.Fatalf("want ErrNothingRedacted, got %v", err)
	}
}
