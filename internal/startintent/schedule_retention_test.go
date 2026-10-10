package startintent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestDemandScheduleRetentionPagesUnobservedGenerationsAndRejectsCorruption(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC().Truncate(time.Second)
	for i := range 101 {
		entry.Workflow = fmt.Sprintf("worker-%03d", i)
		entry.ConfigGeneration = fmt.Sprintf("generation-%03d", i)
		if _, _, err := source.ScheduleCursor(t.Context(), entry, base, false, base); err != nil {
			t.Fatal(err)
		}
		if err := source.CaptureDemand(t.Context(), entry, base, base.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	pins, err := RetainedGenerations(t.Context(), source.Queue)
	if err != nil || len(pins) != 101 {
		t.Fatal(len(pins), err)
	}
	for i := range 101 {
		if !pins[fmt.Sprintf("generation-%03d", i)] {
			t.Fatal("unobserved generation lost", i)
		}
	}
	if _, _, err := source.Queue.SourceCursorRevision(t.Context(), "corrupt", "test", base, base, false); err != nil {
		t.Fatal(err)
	}
	if err := source.Queue.CaptureScheduleDemand(t.Context(), "corrupt", triggerqueue.SourceAdvance{Scope: "corrupt", Revision: "test", Before: base, After: base.Add(time.Minute)}, []byte("corrupt envelope"), base); err != nil {
		t.Fatal(err)
	}
	if _, err := RetainedGenerations(t.Context(), source.Queue); err == nil {
		t.Fatal("corrupt custody allowed archive collection")
	}
}

func TestDemandScheduleCounterRequiresArchiveLease(t *testing.T) {
	source, entry, _, base := demandFixture(t)
	if _, _, err := source.ScheduleCursor(t.Context(), entry, base, false, base); err != nil {
		t.Fatal(err)
	}
	if err := source.CaptureDemand(t.Context(), entry, base, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	source.Build = func(context.Context, Target) (Prepared, error) { return Prepared{Entry: entry}, nil }
	if _, _, err := source.LoadDemand(t.Context(), entry); err == nil {
		t.Fatal("counter borrowed without archive lease")
	}
}
