package startcontrol

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func fixture(t *testing.T) (*Service, *interactiveaccess.Service, apiv1.Gaggle, httpapi.Principal, *startintent.Service) {
	t.Helper()
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	gaggle := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: apiv1.GaggleSpec{InteractiveAccess: &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Viewers: []apiv1.InteractiveHumanGrant{{Issuer: "https://identity", Subject: "reader"}}, Operators: []apiv1.InteractiveHumanGrant{{Issuer: "https://identity", Subject: "alice"}}}, Actions: []apiv1.InteractiveAction{"queue.cancel"}}}}
	access, err := interactiveaccess.New([]apiv1.Gaggle{gaggle}, nil, interactiveaccess.Dependencies{Registrar: journal.NewRegistryScrubber()})
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC) }
	controls := &Coordinator{Queue: queue, Now: now, Archive: func(_ context.Context, m Metadata) (*apiv1.Gaggle, error) {
		if m.Scope.Generation != "pinned" || m.ArchiveWorkflow != "repair" {
			return nil, errors.New("wrong archive")
		}
		return gaggle.DeepCopy(), nil
	}}
	service := &Service{Controls: controls, Access: access, Scrubber: journal.NewPatternScrubber()}
	ordinary := &startintent.Service{Queue: queue, Now: now, Capture: func(context.Context, startintent.Request) (startintent.Target, func(), error) {
		return startintent.Target{Gaggle: "web", Workflow: "repair", ConfigGeneration: "pinned", WorkflowDigest: "workflow", GooberDigest: "goober"}, func() {}, nil
	}}
	return service, access, gaggle, httpapi.Principal{Issuer: "https://identity", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}, ordinary
}
func accept(t *testing.T, s *Service, ordinary *startintent.Service, key string) triggerqueue.StartControl {
	t.Helper()
	r, _, err := ordinary.Accept(t.Context(), key, "automation", startintent.Request{Workflow: "repair"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Controls.Ensure(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestCurrentPolicyGuardsInspectionCancellationAndLaterEffect(t *testing.T) {
	s, access, g, p, ordinary := fixture(t)
	c := accept(t, s, ordinary, "request")
	reader := httpapi.Principal{Issuer: p.Issuer, Subject: "reader", Roles: []httpapi.Role{httpapi.RoleView}}
	if page, err := s.StartQueue(t.Context(), reader, "web", "", 25); err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	input := apicontract.StartQueueCancelInput{RequestID: "cancel-1", Reason: "No longer needed"}
	for _, who := range []httpapi.Principal{reader, {}, {Issuer: p.Issuer, Subject: "unlisted", Roles: []httpapi.Role{httpapi.RoleAdmin}}} {
		if _, err := s.CancelQueuedStart(t.Context(), who, "web", c.Record.ID, input); err == nil {
			t.Fatal("ungranted cancellation")
		}
	}
	if _, err := s.StartQueueItem(t.Context(), p, "other", c.Record.ID); err == nil {
		t.Fatal("foreign receipt exposed")
	}
	if err := ordinary.Queue.BeginDispatchAt(t.Context(), c.Record.ID, s.Controls.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.CancelQueuedStart(t.Context(), p, "web", c.Record.ID, input)
	if err != nil || got.State != "dispatching" || got.RunID != "" || got.Cancellation.State != "requested" {
		t.Fatal(got, err)
	}
	c, err = ordinary.Queue.StartControl(t.Context(), "web", c.Record.ID)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.Stop = func(context.Context, triggerqueue.StartControl) (CancellationObservation, error) {
		calls++
		return CancellationObservation{State: CancellationConfirmed}, nil
	}
	g.Spec.InteractiveAccess.Humans.Operators = nil
	if err = access.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileCancellation(t.Context(), c); err == nil || calls != 0 {
		t.Fatal("revoked command performed effect", err, calls)
	}
	g.Spec.InteractiveAccess.Humans.Operators = []apiv1.InteractiveHumanGrant{{Issuer: p.Issuer, Subject: p.Subject}}
	if err = access.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileCancellation(t.Context(), c); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	got, err = s.StartQueueItem(t.Context(), p, "web", c.Record.ID)
	if err != nil || got.Cancellation.State != "confirmed" || got.State != "dispatching" || got.RunID != "" {
		t.Fatal("confirmation released uncertain launch", got, err)
	}
}
func TestCancelledReceiptReplayIsImmutableAndDoesNotRun(t *testing.T) {
	s, _, _, p, ordinary := fixture(t)
	c := accept(t, s, ordinary, "request")
	input := apicontract.StartQueueCancelInput{RequestID: "cancel", Reason: "Changed plan"}
	first, err := s.CancelQueuedStart(t.Context(), p, "web", c.Record.ID, input)
	if err != nil || first.Disposition != "cancelled" || first.RunID != "" {
		t.Fatal(first, err)
	}
	p.Groups = []string{"new-group"}
	again, err := s.CancelQueuedStart(t.Context(), p, "web", c.Record.ID, input)
	if err != nil || again.Cancellation.RequestedAt != first.Cancellation.RequestedAt {
		t.Fatal(again, err)
	}
	input.Reason = "Different"
	if _, err = s.CancelQueuedStart(t.Context(), p, "web", c.Record.ID, input); err == nil {
		t.Fatal("overwrote command")
	}
	if ready, err := s.Controls.BeforeDispatch(t.Context(), c.Record); err != nil || ready {
		t.Fatal("cancelled execution admitted", ready, err)
	}
}
func TestControlsPinArchivedDeadlinesAndWindowWithoutRetarget(t *testing.T) {
	s, _, _, p, ordinary := fixture(t)
	for i := range 4 {
		accept(t, s, ordinary, fmt.Sprint(i))
	}
	page, err := s.StartQueue(t.Context(), p, "web", "", 2)
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	next, err := s.StartQueue(t.Context(), p, "web", page.NextCursor, 2)
	if err != nil || len(next.Items) != 2 || next.NextCursor != "" {
		t.Fatal(next, err)
	}
	first := page.Items[0]
	if first.Deadline == nil || first.Deadline.Sub(first.AcceptedAt) != 7*24*time.Hour {
		t.Fatal(first)
	}
	s.Controls.Archive = func(context.Context, Metadata) (*apiv1.Gaggle, error) {
		t.Fatal("repinned accepted policy")
		return nil, nil
	}
	s.Controls.Now = func() time.Time { return *first.Deadline }
	r, err := ordinary.Queue.Get(t.Context(), first.AcceptanceID, "automation")
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := s.Controls.BeforeDispatch(t.Context(), r); err != nil || ready {
		t.Fatal(ready, err)
	}
	got, err := s.StartQueueItem(t.Context(), p, "web", r.ID)
	if err != nil || got.Disposition != "expired" || got.RunID != "" {
		t.Fatal(got, err)
	}
	if _, err = s.CancelQueuedStart(t.Context(), p, "web", r.ID, apicontract.StartQueueCancelInput{RequestID: "cancel", Reason: strings.Repeat("x", 513)}); err == nil {
		t.Fatal("unbounded reason")
	}
}
func TestDeadlineDefaultsAndEarlierExplicitBound(t *testing.T) {
	now := time.Now().UTC()
	short := int32(10)
	policy := &apiv1.StartQueuePolicy{PendingDeadlineSeconds: &short, ScheduledDeadlineSeconds: &short}
	for _, tc := range []struct {
		source   string
		policy   *apiv1.StartQueuePolicy
		explicit time.Time
		want     time.Duration
	}{{"manual", nil, time.Time{}, 7 * 24 * time.Hour}, {"schedule", nil, time.Time{}, time.Hour}, {"manual", policy, time.Time{}, 10 * time.Second}, {"manual", policy, now.Add(time.Second), time.Second}, {"manual", policy, now.Add(-time.Second), 0}} {
		got, err := deadline(tc.policy, tc.source, now, tc.explicit)
		if err != nil || got.Sub(now) != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	if got, err := deadline(policy, "child", now, now.Add(time.Second)); err != nil || !got.IsZero() {
		t.Fatal("child got implicit deadline", got, err)
	}
}
