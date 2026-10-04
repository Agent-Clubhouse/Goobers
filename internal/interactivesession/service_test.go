package interactivesession

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func sessionPrincipal(subject string) httpapi.Principal {
	return httpapi.Principal{Issuer: "https://identity.example", Subject: subject, Roles: []httpapi.Role{httpapi.RoleOperate}}
}
func serviceFixture(t *testing.T) (*Service, *journal.RegistryScrubber, apiv1.Gaggle) {
	t.Helper()
	reg := journal.NewRegistryScrubber()
	grants := apiv1.InteractiveHumanGrants{Viewers: []apiv1.InteractiveHumanGrant{{Issuer: "https://identity.example", Subject: "viewer"}}, Operators: []apiv1.InteractiveHumanGrant{{Issuer: "https://identity.example", Subject: "alice"}, {Issuer: "https://identity.example", Subject: "bob"}}}
	g := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "gaggle"}, Spec: apiv1.GaggleSpec{InteractiveAccess: &apiv1.InteractiveAccessPolicy{Humans: grants, Actions: []apiv1.InteractiveAction{"session.create", "session.message"}}}}
	access, err := interactiveaccess.New([]apiv1.Gaggle{g}, nil, interactiveaccess.Dependencies{Registrar: reg})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	s := &Service{Queue: queue, Permissions: access, Scrubber: reg, Now: func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }, Pin: func(_ context.Context, _, name string) (sessioning.Profile, error) {
		return sessioning.Profile{Goober: name, ConfigGeneration: "sha256:" + strings.Repeat("a", 64), GooberDigest: "sha256:" + strings.Repeat("b", 64)}, nil
	}}
	return s, reg, g
}

func TestSharedSessionHumanAttributionAuthorizationAndDisconnectedAcceptance(t *testing.T) {
	s, reg, g := serviceFixture(t)
	reg.Register([]byte("secret-canary"))
	created, err := s.Create(t.Context(), sessionPrincipal("alice"), g.Name, sessioning.CreateRequest{RequestID: "create", Title: "Scope secret-canary", Goober: "planner"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(created.Session.Title, "secret-canary") {
		t.Fatal("unscrubbed title")
	}
	ctx, cancel := context.WithCancel(t.Context())
	first, err := s.SubmitMessage(ctx, sessionPrincipal("alice"), g.Name, created.Session.ID, sessioning.MessageRequest{RequestID: "one", Text: "first secret-canary"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	second, err := s.SubmitMessage(t.Context(), sessionPrincipal("bob"), g.Name, created.Session.ID, sessioning.MessageRequest{RequestID: "one", Text: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Message.Actor.Subject != "alice" || second.Message.Actor.Subject != "bob" || first.AcceptanceID == second.AcceptanceID {
		t.Fatal("human attribution or key scope", first, second)
	}
	page, err := s.Messages(t.Context(), sessionPrincipal("viewer"), g.Name, created.Session.ID, 0, 100)
	if err != nil || len(page.Items) != 2 {
		t.Fatal(page, err)
	}
	stored, err := s.Queue.SessionMessages(t.Context(), g.Name, created.Session.ID, 0, 100)
	if err != nil || strings.Contains(stored.Items[0].Text, "secret-canary") {
		t.Fatal("persisted secret", err)
	}
	turn, err := s.Queue.SessionTurn(t.Context(), first.AcceptanceID)
	if err != nil {
		t.Fatal("browser cancellation lost accepted work", err)
	}
	actor, err := turnPrincipal(turn.Authority, turn.Message.Actor)
	if err != nil || actor.Subject != "alice" || actor.Issuer != "https://identity.example" {
		t.Fatal(actor, err)
	}
	for _, p := range []httpapi.Principal{sessionPrincipal("viewer"), {Issuer: "https://identity.example", Subject: "outsider", Roles: []httpapi.Role{httpapi.RoleAdmin}}} {
		if _, err = s.SubmitMessage(t.Context(), p, g.Name, created.Session.ID, sessioning.MessageRequest{RequestID: "denied", Text: "no"}); !errors.Is(err, sessioning.ErrDenied) {
			t.Fatal("permission bypass", err)
		}
	}
	if _, err = s.Get(t.Context(), sessionPrincipal("alice"), "foreign", created.Session.ID); !errors.Is(err, sessioning.ErrDenied) {
		t.Fatal(err)
	}
	pod := sessionPrincipal("alice")
	pod.GeneratedChild = true
	if _, err = s.SubmitMessage(t.Context(), pod, g.Name, created.Session.ID, sessioning.MessageRequest{RequestID: "pod", Text: "no"}); !errors.Is(err, sessioning.ErrDenied) {
		t.Fatal(err)
	}
}

func TestSessionReplayUsesOriginalPinsAndSafeErrors(t *testing.T) {
	s, reg, g := serviceFixture(t)
	req := sessioning.CreateRequest{RequestID: "key", Title: "Session", Goober: "planner"}
	first, err := s.Create(t.Context(), sessionPrincipal("alice"), g.Name, req)
	if err != nil {
		t.Fatal(err)
	}
	s.Pin = func(context.Context, string, string) (sessioning.Profile, error) {
		return sessioning.Profile{}, errors.New("private-token-provider-error")
	}
	replay, err := s.Create(t.Context(), sessionPrincipal("alice"), g.Name, req)
	if err != nil || !replay.Duplicate || replay.Session.Profile != first.Session.Profile {
		t.Fatal(replay, err)
	}
	input := sessioning.MessageRequest{RequestID: "message", Text: "formerly-unregistered"}
	if _, err = s.SubmitMessage(t.Context(), sessionPrincipal("alice"), g.Name, first.Session.ID, input); err != nil {
		t.Fatal(err)
	}
	reg.Register([]byte(input.Text))
	replay, err = s.SubmitMessage(t.Context(), sessionPrincipal("alice"), g.Name, first.Session.ID, input)
	if err != nil || !replay.Duplicate || strings.Contains(replay.Message.Text, input.Text) {
		t.Fatal("replay redaction", replay, err)
	}
	req.RequestID = "other"
	_, err = s.Create(t.Context(), sessionPrincipal("alice"), g.Name, req)
	var safe *httpapi.InterventionError
	if !errors.As(err, &safe) || safe.Status != 503 || strings.Contains(safe.Message, "private-token") {
		t.Fatal(err)
	}
	_, err = s.SubmitMessage(t.Context(), sessionPrincipal("alice"), g.Name, first.Session.ID, sessioning.MessageRequest{RequestID: "big", Text: strings.Repeat("x", sessioning.MaxTextBytes+1)})
	if !errors.As(err, &safe) || safe.Status != 400 {
		t.Fatal(err)
	}
}

func TestSessionExecutionLeaseAndAvailabilityRemainSeparateFromAuthority(t *testing.T) {
	s, _, g := serviceFixture(t)
	permissions, err := s.Permissions.InteractiveCapabilities(t.Context(), sessionPrincipal("alice"), g.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range permissions.Actions {
		if p.Action == "session.message" && p.Available {
			t.Fatal("runtime advertised before installation")
		}
	}
	s.Permissions.SetSessionsAvailable(true)
	lease, err := s.Permissions.BeginSessionExecution(t.Context(), sessionPrincipal("bob"), g.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Permissions.BeginExecution(t.Context(), sessionPrincipal("bob"), g.Name); !errors.Is(err, interactiveaccess.ErrDenied) {
		t.Fatal("session action grants restart", err)
	}
	next := g.DeepCopy()
	next.Spec.InteractiveAccess.Actions = nil
	done := make(chan error, 1)
	go func() { done <- s.Permissions.Apply([]apiv1.Gaggle{*next}, nil) }()
	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel")
	}
	select {
	case err := <-done:
		t.Fatal("published before join", err)
	default:
	}
	lease.Close()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = s.Permissions.BeginSessionExecution(t.Context(), sessionPrincipal("bob"), g.Name); !errors.Is(err, interactiveaccess.ErrDenied) {
		t.Fatal(err)
	}
}
