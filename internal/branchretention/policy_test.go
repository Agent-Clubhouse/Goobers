package branchretention

import (
	"github.com/goobers/goobers/internal/journal"
	"testing"
	"time"
)

func TestTerminalAuthority(t *testing.T) {
	now := time.Now().UTC()
	identity := journal.RunIdentity{RunID: "run", StartedAt: now.Add(-48 * time.Hour)}
	for _, status := range []string{"completed", "failed", "aborted", "escalated"} {
		events := []journal.Event{{Type: journal.EventRunFinished, Status: status, Time: now.Add(-24 * time.Hour)}}
		got, err := TerminalAt(identity, events, now)
		if status == "escalated" {
			if !got.IsZero() || Settled(events) {
				t.Fatal("escalated run authorized")
			}
			continue
		}
		if err != nil || got != events[0].Time {
			t.Fatalf("%s: %v %v", status, got, err)
		}
		for _, bad := range []time.Time{{}, now.Add(time.Hour), now.Add(-72 * time.Hour)} {
			events[0].Time = bad
			if _, err := TerminalAt(identity, events, now); err == nil {
				t.Fatalf("accepted bad terminal time %v", bad)
			}
		}
		events = append(events, journal.Event{Type: journal.EventGatePaused})
		if Settled(events) {
			t.Fatal("parked run authorized")
		}
	}
}

func TestParkedItems(t *testing.T) {
	for _, value := range []string{"needs-human", "Escalated", "blocked", "goobers:needs-human", "status:blocked", "blocked_on_sibling", "Paused", "parked"} {
		if !Parked(value, nil) || !Parked("closed", []string{value}) {
			t.Fatalf("not protected: %s", value)
		}
	}
	if Parked("closed", []string{"bug", "done"}) {
		t.Fatal("ordinary item protected")
	}
}
