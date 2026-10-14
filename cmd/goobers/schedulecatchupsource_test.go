package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestScheduleCatchUpAllDaemonReopenPreservesAcceptedDefinitionsAcrossPolicyEdit(t *testing.T) {
	root, marker := initRunDurationDemo(t)
	layout := instance.NewLayout(root)
	path := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	authored := strings.Replace(string(raw), "    - type: manual", "    - type: schedule\n      schedule: \"@every 1m\"", 1)
	authored = strings.Replace(authored, "spec:\n", "spec:\n  scheduleCatchUp: all\n", 1)
	if authored == string(raw) {
		t.Fatal("fixture unchanged")
	}
	writeFileContent(t, path, authored)
	first := openSourceDaemonFixture(t, layout)
	if len(first.setup.Entries) != 1 || !first.setup.Entries[0].ScheduleCatchUpAll {
		t.Fatal("public policy missing from compiled entry")
	}
	base := time.Now().UTC().Truncate(time.Minute).Add(-3 * time.Minute)
	due := base.Add(3 * time.Minute)
	if err = first.scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	first.scheduler.Tick(t.Context(), due)
	pending, err := first.service.queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 3 {
		t.Fatal(len(pending), err)
	}
	seen := map[time.Time]bool{}
	captured := make([]triggerqueue.Record, 0, 3)
	for _, record := range pending {
		envelope, err := startintent.Parse(record.Payload)
		if err != nil || envelope.Source.ScheduleDeclaration != 1 {
			t.Fatal(envelope, err)
		}
		if seen[envelope.Source.ScheduledAt] {
			t.Fatal("duplicate nominal time")
		}
		seen[envelope.Source.ScheduledAt] = true
		captured = append(captured, record)
	}
	first.close()
	replacement := strings.Replace(authored, "scheduleCatchUp: all", "scheduleCatchUp: coalesce", 1)
	replacement = strings.Replace(replacement, strconv.Quote(marker), strconv.Quote(marker+".replacement"), 1)
	writeFileContent(t, path, replacement)
	restarted := openSourceDaemonFixture(t, layout)
	if restarted.setup.Entries[0].ScheduleCatchUpAll {
		t.Fatal("current policy edit not applied")
	}
	envelope, err := startintent.Parse(captured[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := restarted.setup.SourceStarts.Build(t.Context(), envelope.Target)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Entry.ScheduleCatchUpAll {
		t.Fatal("archived policy lost")
	}
	prepared.Release()
	if err = restarted.scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	restarted.scheduler.Tick(t.Context(), due)
	pending, err = restarted.service.queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 3 {
		t.Fatal("policy edit changed accepted custody", len(pending), err)
	}
	for range 5 {
		if err = restarted.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
		restarted.scheduler.Wait()
		restarted.workers.Wait()
	}
	runIDs := map[string]bool{}
	for _, original := range captured {
		record, err := restarted.service.queue.Get(t.Context(), original.ID, "scheduler")
		if err != nil || record.State != triggerqueue.Dispatched || record.RunID == "" || runIDs[record.RunID] {
			t.Fatal(record, err)
		}
		runIDs[record.RunID] = true
	}
	if _, err = os.Stat(marker); err != nil {
		t.Fatal("original stage never ran", err)
	}
	if _, err = os.Stat(marker + ".replacement"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("edited stage replaced accepted pins", err)
	}
}
