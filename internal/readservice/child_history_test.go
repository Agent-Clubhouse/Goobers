package readservice

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildHistoryPagesOnlyRecordedParentGaggle(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, _ := createFixtureRun(t, layout, machine, "parent", machine.Def.Name, "goobers", time.Now().UTC(), journal.Trigger{Kind: journal.TriggerManual}, true)
	defer func() { _ = run.Close() }()
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "starts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	now := time.Now().UTC()
	for i := 0; i < 53; i++ {
		parent := triggerqueue.ChildParent{Gaggle: "goobers", ParentRunID: "parent"}
		if i == 51 {
			parent.Gaggle = "foreign"
		}
		if i == 52 {
			parent.ParentRunID = "other"
		}
		_, _, err = queue.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: triggerqueue.ChildIdentity{ChildParent: parent, StageOccurrence: fmt.Sprintf("stage-%d", i), InvocationKey: "inspect"}, Actor: "parent-stage", Payload: []byte(`{"request":"child"}`), MaxChildren: 1}, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	service.sources.ChildHistory = queue.Children
	publicationReads := 0
	service.sources.ChildPublications = func(_ context.Context, child triggerqueue.ChildIdentity) ([]ChildPublicationObservation, error) {
		if child.Gaggle != "goobers" || child.ParentRunID != "parent" {
			t.Fatal("foreign publication queried")
		}
		publicationReads++
		return nil, nil
	}
	first, err := service.RunChildren(t.Context(), "parent", "")
	if err != nil || first.Status != "recorded" || len(first.Items) != 50 || first.NextCursor == "" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if publicationReads != 50 {
		t.Fatal("publication lookup exceeded the displayed page", publicationReads)
	}
	last, err := service.RunChildren(t.Context(), "parent", first.NextCursor)
	if err != nil || len(last.Items) != 1 || last.NextCursor != "" || last.Items[0].ChildID <= first.Items[49].ChildID {
		t.Fatalf("last = %+v, %v", last, err)
	}
	if publicationReads != 51 {
		t.Fatal("publication pagination read the wrong children", publicationReads)
	}
	if first.Items[0].State != triggerqueue.ChildQueued || first.Items[0].TerminalAt != nil {
		t.Fatal("queued child presented as terminal")
	}
	for _, cursor := range []string{"malformed", strings.Repeat("x", 2049), encodeCursor(pageCursor{Collection: "run-children", Scope: "foreign/parent", After: "child"}), encodeCursor(pageCursor{Collection: "run-children", Scope: "goobers/other", After: "child"})} {
		if _, err = service.RunChildren(t.Context(), "parent", cursor); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("cursor accepted: %v", err)
		}
	}
	service.sources.ChildHistory = func(context.Context, triggerqueue.ChildParent, string, int) ([]triggerqueue.ChildRecord, error) {
		t.Fatal("unknown parent queried custody")
		return nil, nil
	}
	if _, err = service.RunChildren(t.Context(), "missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestChildHistoryAvailabilityAndCancellationEvidence(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, _ := createFixtureRun(t, layout, machine, "parent", machine.Def.Name, "goobers", time.Now().UTC(), journal.Trigger{Kind: journal.TriggerManual}, true)
	defer func() { _ = run.Close() }()
	page, err := service.RunChildren(t.Context(), "parent", "")
	if err != nil || page.Status != "unavailable" || page.Items == nil {
		t.Fatalf("offline = %+v, %v", page, err)
	}
	now := time.Now().UTC()
	record := triggerqueue.ChildRecord{Identity: triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "goobers", ParentRunID: "parent"}}, ChildID: "child-one", RunID: "child-run", State: triggerqueue.ChildRunning, AcceptedAt: now, UpdatedAt: now, CancellationRequested: true}
	service.sources.ChildHistory = func(_ context.Context, parent triggerqueue.ChildParent, _ string, limit int) ([]triggerqueue.ChildRecord, error) {
		if parent != record.Identity.ChildParent || limit != 51 {
			t.Fatal("unbounded or wrong parent lookup")
		}
		return []triggerqueue.ChildRecord{record}, nil
	}
	page, err = service.RunChildren(t.Context(), "parent", "")
	if err != nil || page.Items[0].State != triggerqueue.ChildRunning || page.Items[0].TerminalAt != nil || !page.Items[0].CancellationRequested {
		t.Fatalf("cancel request became final result: %+v, %v", page, err)
	}
	record.State, record.TerminalAt, record.AcknowledgedAt, record.TombstonedAt = triggerqueue.ChildCancelled, now, now, now
	page, err = service.RunChildren(t.Context(), "parent", "")
	if err != nil || page.Items[0].ExpiredAt == nil || page.Items[0].AcknowledgedAt == nil || page.Items[0].TerminalAt == nil {
		t.Fatalf("expired evidence lost: %+v, %v", page, err)
	}
	service.sources.ChildHistory = func(context.Context, triggerqueue.ChildParent, string, int) ([]triggerqueue.ChildRecord, error) {
		return nil, errors.New("storage failure")
	}
	if _, err = service.RunChildren(t.Context(), "parent", ""); err == nil {
		t.Fatal("storage failure became empty success")
	}
}
