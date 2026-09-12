package recovery

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestExplicitAbandonmentBindsExactSnapshot(t *testing.T) {
	record := storageTestRecord()
	event, err := AbandonedEvent(record)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(event); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ExplicitlyAbandoned(events, record); err != nil || !ok {
		t.Fatalf("durable abandonment lost: %v %v", ok, err)
	}
	equivalent := record
	equivalent.CreatedAt = equivalent.CreatedAt.In(time.FixedZone("fixture", -7*60*60))
	equivalent.RetainUntil = equivalent.RetainUntil.In(time.FixedZone("fixture", -7*60*60))
	if ok, err := ExplicitlyAbandoned(events, equivalent); err != nil || !ok {
		t.Fatalf("equivalent time zones lost abandonment: %v %v", ok, err)
	}
	renewed := record
	renewed.RetainUntil = renewed.RetainUntil.Add(time.Hour)
	if ok, err := ExplicitlyAbandoned(events, renewed); err != nil || ok {
		t.Fatalf("old abandonment authorized renewed record: %v %v", ok, err)
	}
	for _, mutate := range []func(*journal.Event){
		func(e *journal.Event) { e.RunID = "another-run" },
		func(e *journal.Event) { e.Runner["actor"] = "stage" },
		func(e *journal.Event) { e.Runner[livejournal.EmitKeyRunnerField] = "stage-emission" },
		func(e *journal.Event) { e.Runner["operation"] = "recovery-retained" },
	} {
		candidate, err := AbandonedEvent(record)
		if err != nil {
			t.Fatal(err)
		}
		mutate(&candidate)
		if ok, err := ExplicitlyAbandoned([]journal.Event{candidate}, record); err != nil || ok {
			t.Fatalf("untrusted abandonment accepted: %v %v", ok, err)
		}
	}
	broken, _ := AbandonedEvent(record)
	delete(broken.Runner, "recoveryPatchDigest")
	if ok, err := ExplicitlyAbandoned([]journal.Event{broken}, record); err == nil || ok {
		t.Fatalf("malformed abandonment accepted: %v %v", ok, err)
	}
}
