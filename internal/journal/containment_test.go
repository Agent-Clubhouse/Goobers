package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestCreateRejectsTraversalRunID is #244's table-driven acceptance test for
// journal.Create: a run id that isn't a single, safe path segment must be
// refused before Mkdir ever joins it onto runsDir, and must touch nothing on
// disk.
func TestCreateRejectsTraversalRunID(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../../etc", "/abs", "/etc/passwd", "a/b", "a/../../b"} {
		t.Run(bad, func(t *testing.T) {
			root := t.TempDir()
			id := testIdentity()
			id.RunID = bad
			if _, err := Create(root, id, nil, WithClock(fixedClock())); err == nil {
				t.Fatalf("Create(RunID=%q) unexpectedly succeeded", bad)
			}
			entries, rerr := os.ReadDir(root)
			if rerr != nil {
				t.Fatalf("ReadDir: %v", rerr)
			}
			if len(entries) != 0 {
				t.Fatalf("Create(RunID=%q) left entries on disk: %v", bad, entries)
			}
		})
	}
}

// TestArtifactBytesRejectsEscapingRefPath is #244's acceptance test for
// Reader.ArtifactBytes: ref.Path round-trips through run.yaml/state, so a
// tampered InputRef.Path (e.g. "../…") must be refused before it steers a
// read outside the run directory — mirroring
// TestRedactRejectsEscapingBlobPath's identical containment check on the
// write side, applied here to the read side ArtifactBytes/SpanBytes share.
func TestArtifactBytesRejectsEscapingRefPath(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	dir := filepath.Join(root, testIdentity().RunID)

	// A file OUTSIDE the run dir (a sibling under root), digest-matching so
	// only the containment check (not the digest check) can be what refuses
	// this read.
	const outsideContent = "outside-run-dir-contents"
	outsidePath := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outsidePath, []byte(outsideContent), 0o644); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}

	rd, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	for _, bad := range []string{"../outside.txt", "/etc/passwd", "a/../../outside.txt"} {
		t.Run(bad, func(t *testing.T) {
			escaping := Ref{Path: bad, Digest: Digest([]byte(outsideContent))}
			if _, err := rd.ArtifactBytes(escaping); err == nil {
				t.Fatalf("ArtifactBytes(%q) unexpectedly succeeded", bad)
			}
			if _, err := rd.SpanBytes(escaping); err == nil {
				t.Fatalf("SpanBytes(%q) unexpectedly succeeded", bad)
			}
		})
	}

	// The outside file itself must be untouched throughout.
	after, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatalf("read outside file: %v", err)
	}
	if !bytes.Equal(after, []byte(outsideContent)) {
		t.Fatalf("outside file was modified: %q", after)
	}
}
