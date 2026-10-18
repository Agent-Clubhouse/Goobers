package childpublication

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestPublicationRejectsChangedExecutionBeforeProvider(t *testing.T) {
	q, target, _ := publicationFixture(t)
	publisher := Publisher{Queue: q, Git: publicationGitFake{}}
	if _, err := publisher.validateTarget(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"run", "acceptance", "gaggle", "occurrence", "parent", "restart"} {
		t.Run(field, func(t *testing.T) {
			changed := target
			lineage := *target.Identity.Child
			changed.Identity.Child = &lineage
			switch field {
			case "run":
				changed.Identity.RunID = strings.Repeat("d", 32)
			case "acceptance":
				changed.Child.AcceptanceID = "trigger-" + strings.Repeat("d", 32)
			case "gaggle":
				lineage.Gaggle = "foreign"
			case "occurrence":
				lineage.StageOccurrence = "other/1"
			case "parent":
				lineage.ParentRunID = strings.Repeat("e", 32)
			case "restart":
				changed.Identity.ContinuedFromRunID = strings.Repeat("f", 32)
			}
			if _, err := publisher.validateTarget(t.Context(), changed); err == nil {
				t.Fatal("changed custody accepted")
			}
		})
	}
	if err := q.SetChildState(t.Context(), target.Child.Identity, triggerqueue.ChildStateUpdate{Expected: target.Child.State, State: triggerqueue.ChildFailed, ResultRef: "sealed-result"}, target.Child.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.validateTarget(t.Context(), target); err == nil {
		t.Fatal("terminal child can publish")
	}
}
