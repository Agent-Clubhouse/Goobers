package triggerqueue

import (
	"path/filepath"
	"testing"
)

func TestCurrentChildBindsUnresolvedOccurrenceAndGaggle(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	req := childRequest("parent", "stage", "key")
	child := acceptChildTest(t, s, req, childTestTime)
	current, err := s.CurrentChild(t.Context(), req.Identity.ChildParent, req.Identity.StageOccurrence)
	if err != nil || current.RunID != child.RunID {
		t.Fatal(current, err)
	}
	other, err := s.CurrentChild(t.Context(), ChildParent{Gaggle: "other", ParentRunID: "parent"}, "stage")
	if err != nil || other.RunID != "" {
		t.Fatal("cross-gaggle selection", other, err)
	}
	failChildTest(t, s, child, childTestTime, true)
	current, err = s.CurrentChild(t.Context(), req.Identity.ChildParent, req.Identity.StageOccurrence)
	if err != nil || current.RunID != "" {
		t.Fatal("acknowledged slot still selected", current, err)
	}
}
