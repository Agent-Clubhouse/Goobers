package configmirror

import (
	"path/filepath"
	"testing"
)

// Guards the contract PublishValidated/Open document: once PublishValidated
// returns success, the very next Open must observe that snapshot. #4670
// reported this breaking intermittently on Windows CI ("The system cannot
// find the file specified") immediately after a publish. Iterating exercises
// both the first-publication path (no prior snapshot to replace) on the
// first pass and the replace-existing path (Windows FileRenameInfoEx branch)
// on every pass after, localizing the race to whichever branch it recurs in.
func TestPublishThenOpenIsImmediatelyVisible(t *testing.T) {
	const iterations = 200
	config, mirror := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(config, "instructions.md"), "body")
	for i := 0; i < iterations; i++ {
		if err := Publish(t.Context(), mirror, config, []byte("generation")); err != nil {
			t.Fatalf("iteration %d: publish: %v", i, err)
		}
		s, err := Open(mirror)
		if err != nil {
			t.Fatalf("iteration %d: open just-published snapshot: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("iteration %d: close: %v", i, err)
		}
	}
}
