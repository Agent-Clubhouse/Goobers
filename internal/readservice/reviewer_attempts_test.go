package readservice

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestReviewerAttemptsReuseCanonicalIdentityAcrossRecovery(t *testing.T) {
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	var records []journal.EventRecord
	add := func(kind journal.EventType, number int, class journal.AttemptClass, status string) {
		e := journal.ReviewerAttemptEvent(kind, "review", number, class)
		e.Schema = journal.EventSchema
		e.Seq = uint64(len(records) + 1)
		e.Time = at.Add(time.Duration(e.Seq) * time.Second)
		e.Status = status
		records = append(records, journal.EventRecord{Event: e})
	}
	add(journal.EventReviewerStarted, 1, "", "")
	live := collectStageAttempts("run", records, indexArtifacts(records), "review")["review"]
	if len(live) != 1 || live[0].ID != journal.StageAttemptID("run", 0, "review", 1) {
		t.Fatalf("live=%+v", live)
	}
	// No finish survives the crash. A continuation starts at attempt 2.
	add(journal.EventReviewerStarted, 2, journal.AttemptInfra, "")
	add(journal.EventReviewerFinished, 2, journal.AttemptInfra, "success")
	add(journal.EventReviewerStarted, 1, "", "")
	add(journal.EventReviewerFinished, 1, "", "success")
	got := collectStageAttempts("run", records, indexArtifacts(records), "review")["review"]
	if len(got) != 3 {
		t.Fatal(got)
	}
	if got[0].ID != live[0].ID || got[0].ErrorCode != "reviewer_interrupted" || got[0].Class != "initial" {
		t.Fatalf("interrupted identity drifted: %+v", got[0])
	}
	if got[0].Visit != 1 || got[1].Visit != 1 || got[2].Visit != 2 || got[1].Class != "infra" {
		t.Fatalf("visit/retry lineage=%+v", got)
	}
	if got[0].ID == got[1].ID || got[1].ID == got[2].ID {
		t.Fatal("dispatch identity reused")
	}
	// A distinct continuation run must not borrow its source's attempt identity.
	continued := collectStageAttempts("continued-run", records, indexArtifacts(records), "review")["review"]
	if continued[0].ID == got[0].ID {
		t.Fatal("run continuation reused source identity")
	}
}
