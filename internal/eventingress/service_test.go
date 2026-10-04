package eventingress

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func ingressFixture(t *testing.T) (*Service, httpapi.Principal) {
	t.Helper()
	q, err := triggerqueue.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	catalog, err := eventing.CompileCatalog("web", "catalog-one", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Issuer: "https://identity.example", Subject: "producer", Roles: []httpapi.Role{httpapi.RoleOperate}}
	binding := apiv1.EventIngressBinding{Name: "builds", Issuer: p.Issuer, Subject: p.Subject, Source: "urn:builds", AllowedTypes: []string{"build.finished"}}
	now := time.Now().UTC()
	s := &Service{Queue: q, Scrubber: journal.NewPatternScrubber(), Now: func() time.Time { return now }, Authorize: func(_ context.Context, p httpapi.Principal, g, b string, accept func(apiv1.EventIngressBinding, *eventing.Catalog) error) error {
		if g != "web" || b != "builds" {
			return denied()
		}
		return accept(binding, catalog)
	}}
	return s, p
}
func eventBody(id string) []byte {
	return []byte(fmt.Sprintf(`{"specversion":"1.0","id":%q,"source":"urn:builds","type":"build.finished","data":{"rootId":"untrusted"}}`, id))
}
func expectStatus(t *testing.T, err error, status int) {
	t.Helper()
	var e *httpapi.InterventionError
	if !errors.As(err, &e) || e.Status != status {
		t.Fatalf("expected status %d, got %v", status, err)
	}
}

func TestIngressExactRetryScopeAndClosedEnvelope(t *testing.T) {
	s, p := ingressFixture(t)
	first, err := s.PublishEvent(t.Context(), p, "web", "builds", eventBody("one"))
	if err != nil || first.State != "accepted_unmatched" || first.Duplicate {
		t.Fatal(first, err)
	}
	retry, err := s.PublishEvent(t.Context(), p, "web", "builds", eventBody("one"))
	if err != nil || !retry.Duplicate || retry.ReceiptID != first.ReceiptID {
		t.Fatal(retry, err)
	}
	retained, err := s.Queue.EventInGaggle(t.Context(), "web", first.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Producer.RootID != "" || retained.Producer.RunID != "" || retained.Producer.Stage != "" || retained.Producer.Binding != "ingress:builds" || strings.Contains(retained.Producer.Actor, p.Subject) {
		t.Fatal("untrusted ancestry or raw identity", retained.Producer)
	}
	_, err = s.PublishEvent(t.Context(), p, "web", "builds", []byte(strings.Replace(string(eventBody("one")), "untrusted", "changed", 1)))
	expectStatus(t, err, http.StatusConflict)
	for _, raw := range []string{
		`{"specversion":"1.0","id":"two","id":"three","source":"urn:builds","type":"build.finished"}`,
		`{"specversion":"1.0","id":"two","source":"urn:builds","type":"build.finished","rootId":"forged"}`,
		`{"specversion":"1.0","id":"two","source":"urn:builds","type":"build.finished","data":{"a":1,"a":2}}`,
		string(eventBody("two")) + ` {}`,
		strings.Repeat(" ", eventing.MaxEnvelopeBytes+1),
	} {
		_, err = s.PublishEvent(t.Context(), p, "web", "builds", []byte(raw))
		expectStatus(t, err, 400)
	}
	for _, edit := range []func(*httpapi.Principal){
		func(p *httpapi.Principal) { p.Subject = "other" },
		func(p *httpapi.Principal) { p.Issuer = "goobers/pod" },
		func(p *httpapi.Principal) { p.WorkflowParent = true },
		func(p *httpapi.Principal) { p.GeneratedChild = true },
		func(p *httpapi.Principal) { p.Scopes = []string{"run:1"} },
		func(p *httpapi.Principal) { p.Roles = []httpapi.Role{httpapi.RoleView} },
	} {
		changed := p
		edit(&changed)
		_, err = s.PublishEvent(t.Context(), changed, "web", "builds", eventBody("two"))
		expectStatus(t, err, 403)
	}
	for _, scope := range [][2]string{{"other", "builds"}, {"web", "other"}} {
		_, err = s.PublishEvent(t.Context(), p, scope[0], scope[1], eventBody("two"))
		expectStatus(t, err, 403)
	}
	for _, raw := range []string{strings.Replace(string(eventBody("two")), "urn:builds", "urn:other", 1), strings.Replace(string(eventBody("two")), "build.finished", "other", 1)} {
		_, err = s.PublishEvent(t.Context(), p, "web", "builds", []byte(raw))
		expectStatus(t, err, 403)
	}
}

func TestIngressRateBudgetAndScrubRefusal(t *testing.T) {
	s, p := ingressFixture(t)
	for i := range 50 {
		if _, err := s.PublishEvent(t.Context(), p, "web", "builds", eventBody(fmt.Sprint(i))); err != nil {
			t.Fatal(i, err)
		}
	}
	_, err := s.PublishEvent(t.Context(), p, "web", "builds", eventBody("overflow"))
	expectStatus(t, err, 429)
	at := s.Now().Add(time.Second)
	s.Now = func() time.Time { return at }
	if _, err = s.PublishEvent(t.Context(), p, "web", "builds", eventBody("overflow")); err != nil {
		t.Fatal(err)
	}
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte("secret-known-value"))
	s.Scrubber = registry
	_, err = s.PublishEvent(t.Context(), p, "web", "builds", []byte(strings.Replace(string(eventBody("secret")), "untrusted", "secret-known-value", 1)))
	expectStatus(t, err, 400)
}

func TestIngressReceiptRequiresCurrentExplicitHumanView(t *testing.T) {
	s, p := ingressFixture(t)
	first, err := s.PublishEvent(t.Context(), p, "web", "builds", eventBody("one"))
	if err != nil {
		t.Fatal(err)
	}
	g := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: apiv1.GaggleSpec{InteractiveAccess: &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Viewers: []apiv1.InteractiveHumanGrant{{Issuer: p.Issuer, Subject: "alice"}}}}}}
	s.View, err = interactiveaccess.New([]apiv1.Gaggle{g}, nil, interactiveaccess.Dependencies{Registrar: journal.NewRegistryScrubber()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.EventReceipt(t.Context(), p, "web", first.ReceiptID)
	expectStatus(t, err, 403)
	human := httpapi.Principal{Issuer: p.Issuer, Subject: "alice", Roles: []httpapi.Role{httpapi.RoleView}}
	got, err := s.EventReceipt(t.Context(), human, "web", first.ReceiptID)
	if err != nil || got.ReceiptID != first.ReceiptID {
		t.Fatal(got, err)
	}
	_, err = s.EventReceipt(t.Context(), human, "other", first.ReceiptID)
	expectStatus(t, err, 403)
	human.Subject = "admin"
	human.Roles = []httpapi.Role{httpapi.RoleAdmin}
	_, err = s.EventReceipt(t.Context(), human, "web", first.ReceiptID)
	expectStatus(t, err, 403)
	g.Spec.InteractiveAccess = nil
	s.View, err = interactiveaccess.New([]apiv1.Gaggle{g}, nil, interactiveaccess.Dependencies{Registrar: journal.NewRegistryScrubber()})
	if err != nil {
		t.Fatal(err)
	}
	human.Subject = "alice"
	_, err = s.EventReceipt(t.Context(), human, "web", first.ReceiptID)
	expectStatus(t, err, 403)
}
