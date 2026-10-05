package workbenchservice

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

func TestSessionNativeWriterReusesExactActorCustody(t *testing.T) {
	s, f, g, p, request := writerFixture(t, "github")
	lease, err := s.ReadService.Permissions.BeginSessionExecution(t.Context(), p, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	actor := sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}
	w, err := s.ForSession(&g, lease, actor)
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.Patch(t.Context(), "items", "trusted-turn-command", request)
	if err != nil || first.State != "confirmed" || first.Actor.Issuer != actor.Issuer || first.Actor.Subject != actor.Subject {
		t.Fatal(first, err)
	}
	reads := f.reads
	t.Setenv("HUMAN_BACKLOG", "")
	replay, err := w.Patch(t.Context(), "items", "trusted-turn-command", request)
	if err != nil || !replay.Duplicate || replay.ID != first.ID {
		t.Fatal(replay, err)
	}
	receipt, err := w.Command(t.Context(), "items", first.ID)
	if err != nil || receipt.ID != first.ID || f.reads != reads || f.patches != 1 {
		t.Fatal(receipt, err)
	}
	actor.Subject = "someone-else"
	if _, err = s.ForSession(&g, lease, actor); !errors.Is(err, interactiveaccess.ErrDenied) {
		t.Fatal("foreign actor adopted lease", err)
	}
	changed := g.DeepCopy()
	changed.Spec.Workbench.Sources[0].Writes.Fields = []apiv1.WorkbenchField{"description"}
	actor.Subject = p.Subject
	if _, err = s.ForSession(changed, lease, actor); !errors.Is(err, interactiveaccess.ErrDenied) {
		t.Fatal("source policy retargeted inside lease", err)
	}
}

func TestSessionNativeWriterRevocationJoinsReceiptWithoutPolicyLockRecursion(t *testing.T) {
	s, f, g, p, request := writerFixture(t, "github")
	lease, err := s.ReadService.Permissions.BeginSessionExecution(t.Context(), p, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	w, err := s.ForSession(&g, lease, sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	f.lost = true
	f.patchHook = func(r *http.Request) { close(entered); <-r.Context().Done() }
	done := make(chan workbench.BacklogEditCommand, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := w.Patch(t.Context(), "items", "revoked-turn", request)
		lease.Close()
		done <- result
		errs <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider write did not begin")
	}
	changed := g.DeepCopy()
	changed.Spec.InteractiveAccess.Actions = nil
	applied := make(chan error, 1)
	go func() { applied <- s.ReadService.Permissions.Apply([]apiv1.Gaggle{*changed}, nil) }()
	var result workbench.BacklogEditCommand
	select {
	case result = <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("session command deadlocked against policy cancellation")
	}
	if err = <-errs; err != nil || result.State != "unknown" {
		t.Fatal(result, err)
	}
	select {
	case err = <-applied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("policy failed to join completed command")
	}
	record, err := s.Queue.WorkbenchCommand(t.Context(), writeScope(p, g.Name, "items"), result.ID)
	if err != nil || record.State != "unknown" || record.Receipt == nil || f.patches != 1 {
		t.Fatal("lost actual outcome during revocation", record, err)
	}
	if _, err = w.Command(context.Background(), "items", result.ID); err == nil {
		t.Fatal("revoked lease read a receipt")
	}
	if _, err = w.Patch(context.Background(), "items", "new-key", request); err == nil {
		t.Fatal("revoked lease accepted another effect")
	}
}
