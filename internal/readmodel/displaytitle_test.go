package readmodel

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func displayTitleFinish(seq uint64, status string, outputs map[string]any) journal.Event {
	return ev(seq, time.Duration(seq)*time.Second, journal.EventStageFinished, func(e *journal.Event) {
		e.Stage, e.Status, e.Outputs = "claim", status, outputs
	})
}

// TestProjectionDisplayTitle pins #5429's display-title contract: present,
// absent, malformed, and re-emitted by a retried claim stage — and that every
// incremental split projects the same operator facts as the whole journal.
func TestProjectionDisplayTitle(t *testing.T) {
	const canonical = "Investigate #123456789: Example incident title"
	claimed := map[string]any{"id": "123456789", "title": "Example incident title"}
	withTitle := func(title any) map[string]any {
		outputs := map[string]any{OutputDisplayTitle: title}
		for k, v := range claimed {
			outputs[k] = v
		}
		return outputs
	}
	cases := []struct {
		name   string
		events []journal.Event
		want   string
	}{
		{name: "present", want: canonical, events: []journal.Event{
			displayTitleFinish(1, "success", withTitle("  "+canonical+"\t")),
		}},
		{name: "absent", want: "", events: []journal.Event{
			displayTitleFinish(1, "success", claimed),
		}},
		{name: "malformed non-string", want: "", events: []journal.Event{
			displayTitleFinish(1, "success", withTitle(float64(42))),
		}},
		{name: "malformed blank", want: "", events: []journal.Event{
			displayTitleFinish(1, "success", withTitle("   ")),
		}},
		{name: "malformed multi-line", want: "", events: []journal.Event{
			displayTitleFinish(1, "success", withTitle("Investigate\n#1")),
		}},
		{name: "malformed over bound", want: "", events: []journal.Event{
			displayTitleFinish(1, "success", withTitle(strings.Repeat("é", MaxDisplayTitleRunes+1))),
		}},
		{name: "at bound", want: strings.Repeat("é", MaxDisplayTitleRunes), events: []journal.Event{
			displayTitleFinish(1, "success", withTitle(strings.Repeat("é", MaxDisplayTitleRunes))),
		}},
		{name: "malformed later emission keeps earlier valid title", want: canonical, events: []journal.Event{
			displayTitleFinish(1, "success", withTitle(canonical)),
			displayTitleFinish(2, "success", withTitle("bad\x00title")),
			displayTitleFinish(3, "success", withTitle("")),
		}},
		{name: "retried claim stage", want: "Investigate #123456789: Refined incident title", events: []journal.Event{
			ev(1, time.Second, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "claim" }),
			displayTitleFinish(2, "failure", withTitle("Failed attempt title")),
			ev(3, 3*time.Second, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "claim" }),
			displayTitleFinish(4, "success", withTitle(canonical)),
			ev(5, 5*time.Second, journal.EventStageRerunRequested, func(e *journal.Event) { e.Stage = "claim" }),
			ev(6, 6*time.Second, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "claim" }),
			displayTitleFinish(7, "success", withTitle("Investigate #123456789: Refined incident title")),
		}},
		{name: "failed attempt only", want: "", events: []journal.Event{
			displayTitleFinish(1, "failure", withTitle(canonical)),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity := testIdentity()
			whole := ProjectRun(identity, Projection{}, tc.events)
			got := whole.Run.Operator
			if got.DisplayTitle != tc.want {
				t.Fatalf("display title = %q, want %q", got.DisplayTitle, tc.want)
			}
			if got.IssueNumber != "123456789" || got.IssueTitle != "Example incident title" {
				t.Fatalf("issue fallback = #%s %q, want it projected independently", got.IssueNumber, got.IssueTitle)
			}
			for split := 0; split <= len(tc.events); split++ {
				first := ProjectRun(identity, Projection{}, tc.events[:split])
				incremental := ProjectRun(identity, first, tc.events[split:])
				if !reflect.DeepEqual(incremental.Run.Operator, got) {
					t.Fatalf("split %d operator = %+v, whole = %+v", split, incremental.Run.Operator, got)
				}
			}
		})
	}
}
