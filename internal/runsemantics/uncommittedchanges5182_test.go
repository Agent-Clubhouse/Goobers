package runsemantics

import (
	"testing"

	"github.com/goobers/goobers/internal/harness"
)

// The harness produces the failure; the runner decides its retry class. The
// two spellings must never drift apart.
func TestUncommittedChangesCodeMatchesHarness(t *testing.T) {
	if UncommittedChangesCode != harness.ErrorCodeUncommittedChanges {
		t.Fatalf("runner %q != harness %q", UncommittedChangesCode, harness.ErrorCodeUncommittedChanges)
	}
}
