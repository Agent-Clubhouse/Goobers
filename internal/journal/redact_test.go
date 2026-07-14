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

// sameSizeLeak is a 10-byte secret: replacing it with the 10-byte "[REDACTED]"
// placeholder leaves the enclosing blob byte-for-byte the same length. It is
// shaped so the pattern net does not catch it, so it genuinely reaches disk.
const sameSizeLeak = "LEAK123456" // len 10 == len("[REDACTED]")

// TestRedactSameSizeReplacementRemovesSecret is the #113 negative control for the
// same-size dedup skip: an in-place input redaction whose scrubbed form is the
// SAME size as the leaked original. writeContentScrubbed used to skip the write
// on a size match alone, so the raw secret survived at rest while Redact reported
// success. The redaction must actually land and the secret must be gone.
func TestRedactSameSizeReplacementRemovesSecret(t *testing.T) {
	if len(Redacted) != len(sameSizeLeak) {
		t.Fatalf("test precondition: placeholder %q (len %d) must match leak len %d — "+
			"otherwise this does not exercise the same-size path", Redacted, len(Redacted), len(sameSizeLeak))
	}
	reg, scrub := DefaultScrubber() // secret not registered yet → it leaks into the input snapshot
	root := t.TempDir()
	run, err := Create(root, testIdentity(), map[string][]byte{
		// "token: <10-byte secret>\n" scrubs to "token: [REDACTED]\n" — identical length.
		"creds.env": []byte("token: " + sameSizeLeak + "\n"),
	}, WithScrubber(scrub), WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	oldRef := run.id.Inputs[0].Ref
	if isArtifactPath(oldRef.Path) {
		t.Fatalf("precondition: input must be an in-place (non-artifact) path, got %q", oldRef.Path)
	}

	dir := filepath.Join(root, testIdentity().RunID)
	if hits := filesContaining(t, dir, []byte(sameSizeLeak)); len(hits) == 0 {
		t.Fatal("precondition failed: leak should be at rest before redaction")
	}

	reg.Register([]byte(sameSizeLeak))
	newRef, err := run.Redact(oldRef, "same-size secret leaked into creds.env")
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if newRef.Digest == oldRef.Digest {
		t.Fatalf("redaction did not change the digest")
	}
	if newRef.Size != oldRef.Size {
		t.Fatalf("precondition: expected a same-size replacement, got old=%d new=%d", oldRef.Size, newRef.Size)
	}
	_ = run.Close()

	// The whole point: the raw secret must be gone from every file at rest.
	if hits := filesContaining(t, dir, []byte(sameSizeLeak)); len(hits) > 0 {
		t.Fatalf("same-size redaction skipped the write; leak survived in: %v", hits)
	}
	// And the bytes at rest actually commit to the new digest.
	got, err := os.ReadFile(filepath.Join(dir, newRef.Path))
	if err != nil {
		t.Fatalf("read redacted input: %v", err)
	}
	if Digest(got) != newRef.Digest {
		t.Fatalf("redacted blob digest %s does not match Ref %s", Digest(got), newRef.Digest)
	}
}

// TestRedactRejectsEscapingBlobPath is the #113 negative control for missing path
// validation: a target Ref whose Path climbs out of the run directory must be
// rejected before Redact reads, overwrites, or removes anything. Without the
// check, Redact would follow "../…" to a file outside the journal and rewrite it.
func TestRedactRejectsEscapingBlobPath(t *testing.T) {
	reg, scrub := DefaultScrubber()
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithScrubber(scrub), WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A file OUTSIDE the run dir (a sibling under root). run dir is root/<RunID>,
	// so "../outside.txt" from the run dir resolves to root/outside.txt.
	const outsideSecret = "OUTSIDE-SECRET-VALUE-123"
	outsideContent := []byte("keep me: " + outsideSecret)
	outsidePath := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outsidePath, outsideContent, 0o644); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}
	reg.Register([]byte(outsideSecret)) // so an unchecked redaction WOULD rewrite it

	// Digest deliberately not the scrubbed digest, so the "nothing changed"
	// short-circuit cannot mask a missing containment check.
	escaping := Ref{Path: "../outside.txt", Digest: Digest([]byte("unrelated")), Size: int64(len(outsideContent))}
	if _, err := run.Redact(escaping, "attempted traversal"); err == nil {
		t.Fatal("Redact must reject a blob path that escapes the run directory")
	}

	// The file outside the journal must be untouched.
	after, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatalf("read outside file: %v", err)
	}
	if !bytes.Equal(after, outsideContent) {
		t.Fatalf("Redact modified a file outside the run dir: %q", after)
	}
}

// TestWriteContentSameSizeDifferentContentRewrites isolates the dedup fix at the
// storage layer: a blob already on disk with the SAME size but DIFFERENT content
// must be overwritten, not skipped. This is the mechanism the same-size redaction
// leak (above) depends on.
func TestWriteContentSameSizeDifferentContentRewrites(t *testing.T) {
	dir := t.TempDir()
	const rel = "inputs/x"

	old := []byte("AAAAAAAAAA") // 10 bytes
	if _, err := writeContentScrubbed(dir, rel, old, Digest(old)); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	fresh := []byte("BBBBBBBBBB") // 10 bytes, same size, different content
	ref, err := writeContentScrubbed(dir, rel, fresh, Digest(fresh))
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, fresh) {
		t.Fatalf("same-size different-content write was skipped: on disk %q, wanted %q", got, fresh)
	}
	if ref.Digest != Digest(fresh) {
		t.Fatalf("ref digest %s does not commit to written bytes %s", ref.Digest, Digest(fresh))
	}
}

// TestVerifyBlobDigestRejectsMismatch certifies the fail-closed check Redact runs
// after writing: a blob whose bytes do not hash to the claimed digest is an error,
// never a silent success.
func TestVerifyBlobDigestRejectsMismatch(t *testing.T) {
	dir := t.TempDir()
	content := []byte("hello journal world")
	if err := os.WriteFile(filepath.Join(dir, "blob"), content, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := verifyBlobDigest(dir, Ref{Path: "blob", Digest: Digest(content)}); err != nil {
		t.Fatalf("matching digest must verify: %v", err)
	}
	if err := verifyBlobDigest(dir, Ref{Path: "blob", Digest: Digest([]byte("something else"))}); err == nil {
		t.Fatal("verifyBlobDigest must reject bytes that do not match the claimed digest")
	}
}
