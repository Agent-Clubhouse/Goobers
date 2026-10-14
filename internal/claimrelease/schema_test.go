package claimrelease_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/journal"
)

// TestEveryDispositionIsAValidJournalEvent keeps the Go vocabulary and the
// journal-event schema's claimRelease.disposition enum from drifting: a
// disposition added on one side only fails here, not in a live daemon whose
// release events would stop validating.
func TestEveryDispositionIsAValidJournalEvent(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatalf("build validator: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	dispositions := claimrelease.KnownDispositions()
	for _, d := range dispositions {
		reason := d.Because("some_code", "a bounded summary").At("implement").By("cli").Normalize()
		if err := log.Append(journal.Event{
			Type: journal.EventClaimReleased, Name: "7", RunID: "run-" + string(d),
			Reason: string(reason.Disposition), ClaimRelease: &reason,
		}); err != nil {
			t.Fatalf("append %s: %v", d, err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(events, "\n"), []byte("\n"))
	if len(lines) != len(dispositions) {
		t.Fatalf("journaled %d events for %d dispositions", len(lines), len(dispositions))
	}
	for _, line := range lines {
		if err := v.ValidateJSON("journal-event.schema.json", line); err != nil {
			t.Errorf("release event fails the journal-event schema: %v\n%s", err, line)
		}
	}

	// The converse: a disposition the Go side does not know is refused.
	bogus := bytes.Replace(lines[0], []byte(`"disposition":"`), []byte(`"disposition":"bogus-`), 1)
	if err := v.ValidateJSON("journal-event.schema.json", bogus); err == nil {
		t.Fatalf("schema accepted an unknown disposition:\n%s", bogus)
	}
}
