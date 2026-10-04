package workbenchservice

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
)

func TestNativeWriteServiceConcurrentReplayJoinsNoSecondAttempt(t *testing.T) {
	s, f, g, p, request := writerFixture(t, "github")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f.patchHook = func(*http.Request) { close(entered); <-release }
	done := make(chan workbench.BacklogEditCommand, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := s.Patch(t.Context(), p, g.Name, "items", "concurrent", request)
		done <- result
		errs <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not start")
	}
	replay, err := s.Patch(t.Context(), p, g.Name, "items", "concurrent", request)
	if err != nil || !replay.Duplicate || replay.State != "attempting" {
		t.Fatal(replay, err)
	}
	releaseOnce.Do(func() { close(release) })
	result := <-done
	if err = <-errs; err != nil || result.State != "confirmed" || result.ID != replay.ID {
		t.Fatal(result, err)
	}
	if f.patches != 1 {
		t.Fatal("duplicate provider call", f.patches)
	}
}

func TestNativeWriteServiceCancellationPersistsAmbiguityBeforePolicyChanges(t *testing.T) {
	s, f, g, p, request := writerFixture(t, "github")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f.lost = true
	f.patchHook = func(*http.Request) { close(entered); <-release; cancel() }
	done := make(chan workbench.BacklogEditCommand, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := s.Patch(ctx, p, g.Name, "items", "canceled", request)
		done <- result
		errs <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not start")
	}
	changed := g.DeepCopy()
	changed.Spec.InteractiveAccess.Actions = nil
	published := make(chan error, 1)
	go func() { published <- s.ReadService.Permissions.Apply([]apiv1.Gaggle{*changed}, nil) }()
	select {
	case err := <-published:
		t.Fatal("policy overtook active mutation", err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	result := <-done
	if err := <-errs; err != nil || result.State != "unknown" {
		t.Fatal(result, err)
	}
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	retained, err := s.Queue.WorkbenchCommand(t.Context(), writeScope(p, g.Name, "items"), result.ID)
	if err != nil || retained.State != "unknown" || retained.Receipt == nil {
		t.Fatal("cancellation lost the retained outcome", retained, err)
	}
	_, err = s.Command(t.Context(), p, g.Name, "items", result.ID)
	expectWriteStatus(t, err, 403)
	if err = s.ReadService.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	replay, err := s.Patch(t.Context(), p, g.Name, "items", "canceled", request)
	if err != nil || replay.State != "unknown" || f.patches != 1 {
		t.Fatal(replay, err)
	}
}

func TestNativeWriteServiceCompactReceiptExpiryUsesTargetAndActorOnly(t *testing.T) {
	s, f, g, p, request := writerFixture(t, "github")
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	first, err := s.Patch(t.Context(), p, g.Name, "items", "expired", request)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(triggerqueue.WorkbenchCommandRetention + time.Hour)
	if n, err := s.Queue.PruneWorkbenchCommands(t.Context(), now, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	// The field no longer exists in a compact tombstone and cannot be inferred.
	g.Spec.Workbench.Sources[0].Writes.Fields = []apiv1.WorkbenchField{"description"}
	if err = s.ReadService.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = s.Command(t.Context(), p, g.Name, "items", first.ID)
	expectWriteStatus(t, err, 410)
	foreign := p
	foreign.Subject = "other"
	_, err = s.Command(t.Context(), foreign, g.Name, "items", first.ID)
	expectWriteStatus(t, err, 403)
	g.Spec.Backlog.Project = "acme/other"
	if err = s.ReadService.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Command(t.Context(), p, g.Name, "items", first.ID); err == nil {
		t.Fatal("expired metadata crossed target")
	}
	if f.patches != 1 {
		t.Fatal("receipt inspection mutated provider")
	}
}
