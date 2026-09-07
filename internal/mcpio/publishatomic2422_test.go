package mcpio

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPublishOutputLeavesNoPartialArtifact is #2422's regression pin.
//
// publish_output used os.WriteFile, which truncates the existing artifact
// before writing its replacement. A second publish that died mid-write —
// ENOSPC, a kill, a crash — left the target holding partial bytes of the new
// content, and the harness lifts and journals whatever is on disk at
// completion. A truncated report that still parses is worse than a missing
// one: nothing downstream can tell it from a short report.
//
// The observable guarantee is stated as an invariant over the target rather
// than as "the temp file is named X": at no point does the artifact hold
// anything but one complete generation.
func TestPublishOutputLeavesNoPartialArtifact(t *testing.T) {
	ws := t.TempDir()
	tool := NewToolset(Config{Workspace: ws, ArtifactFile: "report.md"})
	target := filepath.Join(ws, "report.md")

	first := strings.Repeat("complete first report\n", 500)
	if _, _, err := tool.PublishOutput(first); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	second := "second"
	if _, _, err := tool.PublishOutput(second); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != second {
		t.Fatalf("artifact = %q, want the second complete generation", data)
	}
	// The staging file must not survive a successful publish either: a
	// leftover sibling in a repo workspace lands in the stage's diff.
	assertNoStagingLitter(t, ws, "report.md")
}

// TestPublishOutputReplacesTheFileRatherThanRewritingItInPlace is the test that
// actually separates an atomic publish from the truncate-then-write it
// replaced, and it exists because the obvious tests do not.
//
// Asserting "the artifact holds the new content" passes under both
// implementations, because os.WriteFile also produces complete content when it
// is not interrupted — and interrupting it deterministically is exactly what a
// unit test cannot do. What distinguishes the two is file identity: a
// truncate-and-rewrite keeps the same file, while staging into a sibling and
// renaming over the target produces a different one. That is the property the
// durability guarantee rests on, and it is observable without any timing.
func TestPublishOutputReplacesTheFileRatherThanRewritingItInPlace(t *testing.T) {
	ws := t.TempDir()
	tool := NewToolset(Config{Workspace: ws, ArtifactFile: "report.md"})
	target := filepath.Join(ws, "report.md")

	if _, _, err := tool.PublishOutput("first"); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tool.PublishOutput("second"); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	if os.SameFile(before, after) {
		t.Fatal("the artifact is the same file across two publishes: it was truncated and rewritten in " +
			"place, so a publish that dies mid-write leaves partial bytes the harness will journal as valid")
	}
}

// TestPublishOutputFailureLeavesThePriorArtifactIntact covers the failure the
// issue names: the publish cannot complete, and what must survive is the
// previous COMPLETE artifact rather than a truncated one.
//
// A read-only workspace directory is the deterministic way to fail a publish
// after the target already exists. It also separates the two implementations,
// which is the point: staging needs to create a sibling and fails, while
// truncate-and-rewrite opens the existing file — whose own permissions still
// allow writing — and destroys it.
func TestPublishOutputFailureLeavesThePriorArtifactIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not deny the write")
	}
	ws := t.TempDir()
	tool := NewToolset(Config{Workspace: ws, ArtifactFile: "report.md"})
	target := filepath.Join(ws, "report.md")

	first := "complete first report"
	if _, _, err := tool.PublishOutput(first); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if err := os.Chmod(ws, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ws, 0o755) })

	if _, _, err := tool.PublishOutput("replacement that cannot land"); err == nil {
		t.Fatal("publish err = nil, want the failed publish reported rather than the artifact destroyed")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("prior artifact unreadable after a failed publish: %v", err)
	}
	if string(data) != first {
		t.Fatalf("artifact = %q, want the prior complete content %q preserved", data, first)
	}
}

// TestPublishOutputReturnsTheDigestOfWhatItWrote pins the token a caller uses
// to tell two successful publishes apart, and to confirm which one it is
// looking at.
func TestPublishOutputReturnsTheDigestOfWhatItWrote(t *testing.T) {
	ws := t.TempDir()
	tool := NewToolset(Config{Workspace: ws, ArtifactFile: "report.md"})

	content := "the published report"
	n, digest, err := tool.PublishOutput(content)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if n != len(content) {
		t.Fatalf("bytesWritten = %d, want %d", n, len(content))
	}
	sum := sha256.Sum256([]byte(content))
	if want := "sha256:" + hex.EncodeToString(sum[:]); digest != want {
		t.Fatalf("digest = %q, want %q", digest, want)
	}

	// A second, different publish must report a different digest — otherwise
	// the token cannot distinguish generations, which is the whole point.
	_, second, err := tool.PublishOutput("a different report")
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if second == digest {
		t.Fatalf("digest unchanged across two different publishes (%q)", digest)
	}
}

// assertNoStagingLitter fails if any file other than the artifact itself
// remains in the workspace directory. The staging file is uniquely named, so
// this asserts the property (nothing left behind) rather than a name.
func assertNoStagingLitter(t *testing.T, ws, artifact string) {
	t.Helper()
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != artifact {
			t.Fatalf("workspace holds %q after publish; a staging file left in a repo workspace "+
				"lands in the stage's diff", entry.Name())
		}
	}
}
