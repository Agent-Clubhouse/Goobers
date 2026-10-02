package k8spreflight

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/clustercheck"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestRecordResultsPersistsOnlySafeOutcome(t *testing.T) {
	root := t.TempDir()
	report := Report{Target: "https://SECRET.example.test", Results: []Result{{ID: "overlay-image-contract", Status: StatusFail, Detail: "SECRET detail", Hint: "SECRET hint", Title: "SECRET title"}}}
	if err := RecordResults(root, report, time.Hour); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(instance.NewLayout(root).SchedulerDir())
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	encoded, err := json.Marshal(events[0])
	if err != nil || strings.Contains(string(encoded), "SECRET") {
		t.Fatalf("leaked report: %s, %v", encoded, err)
	}
	result, ok := clustercheck.FromEvent(events[0])
	if !ok || result.Check != "overlay-image-contract" || result.Outcome != "fail" || result.ExpiresAt.Sub(result.CheckedAt) != time.Hour {
		t.Fatalf("result = %+v, %v", result, ok)
	}
}

func TestRecordResultsRequiresPositiveFreshnessWhenEnabled(t *testing.T) {
	if err := RecordResults("", Report{}, 0); err != nil {
		t.Fatalf("disabled recording = %v", err)
	}
	for _, age := range []time.Duration{0, -time.Second} {
		if err := RecordResults(t.TempDir(), Report{}, age); err == nil {
			t.Fatalf("accepted age %s", age)
		}
	}
}
