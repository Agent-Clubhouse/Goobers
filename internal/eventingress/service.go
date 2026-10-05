// Package eventingress admits explicitly bound machine producers through the
// existing gaggle event ledger. It does not authenticate HTTP credentials.
package eventingress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Authorization fences the current applied catalog and archive through accept.
// Binding is resolved from configuration and never supplied by an event body.
type Authorization func(context.Context, httpapi.Principal, string, string, func(apiv1.EventIngressBinding, *eventing.Catalog) error) error

// ViewAuthorization is the existing explicit human gaggle visibility contract.
type ViewAuthorization interface {
	WithSourceView(context.Context, httpapi.Principal, string, func(context.Context, *apiv1.Gaggle) error) error
}

// Service shares the durable event ledger and a bounded per-binding rate budget.
type Service struct {
	Queue     *triggerqueue.Store
	Authorize Authorization
	View      ViewAuthorization
	Scrubber  journal.Scrubber
	Now       func() time.Time
	mu        sync.Mutex
	rates     map[string]*rate.Limiter
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}
func denied() error {
	return httpapi.NewInterventionError(http.StatusForbidden, "event_scope_denied", "Event producer binding is not authorized.", nil)
}
func authenticated(p httpapi.Principal) bool {
	return p.Issuer != "" && p.Subject != "" && !strings.HasPrefix(p.Issuer, "goobers/") && p.HasRole(httpapi.RoleOperate) && p.ChildWorkflow == nil && !p.GeneratedChild && !p.WorkflowParent && len(p.Scopes) == 0
}

// PublishEvent accepts a closed structured envelope after current binding and
// type checks. Retrying an unknown outcome must reuse source and event ID.
func (s *Service) PublishEvent(ctx context.Context, p httpapi.Principal, gaggle, binding string, raw []byte) (apicontract.GaggleEventReceipt, error) {
	if !authenticated(p) {
		return apicontract.GaggleEventReceipt{}, denied()
	}
	envelope, err := parseIngress(raw)
	if err != nil {
		return apicontract.GaggleEventReceipt{}, httpapi.NewInterventionError(http.StatusBadRequest, "invalid_event", "Invalid bounded CloudEvents envelope.", nil)
	}
	if s.Queue == nil || s.Authorize == nil || s.Scrubber == nil {
		return apicontract.GaggleEventReceipt{}, ingressError(errors.New("event ingress unavailable"))
	}
	if !bytes.Equal(envelope.JSON, s.Scrubber.Scrub(envelope.JSON)) {
		return apicontract.GaggleEventReceipt{}, httpapi.NewInterventionError(http.StatusBadRequest, "event_contains_redacted_data", "Event contains content that cannot be persisted.", nil)
	}
	var result apicontract.GaggleEventReceipt
	err = s.Authorize(ctx, p, gaggle, binding, func(b apiv1.EventIngressBinding, catalog *eventing.Catalog) error {
		if b.Name != binding || b.Issuer != p.Issuer || b.Subject != p.Subject || b.Source != envelope.Source || !slices.Contains(b.AllowedTypes, envelope.Type) || catalog == nil {
			return denied()
		}
		if !s.allow(gaggle, binding) {
			return httpapi.NewInterventionError(http.StatusTooManyRequests, "event_rate_limited", "Event ingress rate limit exceeded; retry the same event.", nil)
		}
		_, plan, err := catalog.Match(gaggle, envelope.JSON)
		if err != nil {
			return err
		}
		identity, _ := json.Marshal([]string{p.Issuer, p.Subject})
		producer := triggerqueue.EventProducer{Gaggle: gaggle, Binding: "ingress:" + binding, Actor: fmt.Sprintf("ingress:%x", sha256.Sum256(identity))}
		receipt, duplicate, err := s.Queue.AcceptEvent(ctx, triggerqueue.EventAcceptance{Producer: producer, Envelope: envelope.JSON, Plan: plan}, s.now())
		if err != nil {
			return ingressError(err)
		}
		result = receiptView(receipt, duplicate)
		return nil
	})
	if errors.Is(err, interactiveaccess.ErrDenied) {
		err = denied()
	}
	if err != nil {
		var apiError *httpapi.InterventionError
		if !errors.As(err, &apiError) {
			err = ingressError(err)
		}
	}
	return result, err
}

func parseIngress(raw []byte) (eventing.Envelope, error) {
	e, err := eventing.Parse(raw)
	if err != nil {
		return e, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(e.JSON, &fields); err != nil {
		return e, err
	}
	for key := range fields {
		if !slices.Contains([]string{"specversion", "id", "source", "type", "subject", "time", "dataschema", "datacontenttype", "data"}, key) {
			return e, errors.New("event ingress forbids envelope extensions")
		}
	}
	return e, nil
}
func (s *Service) allow(gaggle, binding string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rates == nil {
		s.rates = map[string]*rate.Limiter{}
	}
	key := gaggle + "\x00" + binding
	limit := s.rates[key]
	if limit == nil {
		if len(s.rates) >= 4096 {
			return false
		}
		limit = rate.NewLimiter(10, 50)
		s.rates[key] = limit
	}
	return limit.AllowN(s.now(), 1)
}
func ingressError(err error) error {
	if errors.Is(err, triggerqueue.ErrConflict) {
		return httpapi.NewInterventionError(http.StatusConflict, "event_identity_conflict", "Event source and ID already identify different content or producer.", nil)
	}
	return httpapi.NewInterventionError(http.StatusServiceUnavailable, "event_acceptance_unavailable", "Event acceptance is unconfirmed; retry with the same source and ID.", nil)
}
func receiptView(r triggerqueue.EventReceipt, duplicate bool) apicontract.GaggleEventReceipt {
	return apicontract.GaggleEventReceipt{ReceiptID: r.ID, Gaggle: r.Producer.Gaggle, Binding: r.Producer.Binding, Source: r.Source, EventID: r.EventID, Digest: r.Digest, AcceptedAt: r.AcceptedAt, Duplicate: duplicate, State: string(r.State), StatusURL: "/api/v1/gaggles/" + url.PathEscape(r.Producer.Gaggle) + "/events/" + url.PathEscape(r.ID), Tombstoned: !r.TombstonedAt.IsZero(), Deliveries: []apicontract.GaggleEventDelivery{}}
}

// EventReceipt uses current explicit human visibility, independent of producer
// grants, and never exposes payload bytes or raw authenticated provenance.
func (s *Service) EventReceipt(ctx context.Context, p httpapi.Principal, gaggle, id string) (apicontract.GaggleEventReceipt, error) {
	if s.View == nil || s.Queue == nil {
		return apicontract.GaggleEventReceipt{}, denied()
	}
	var result apicontract.GaggleEventReceipt
	err := s.View.WithSourceView(ctx, p, gaggle, func(ctx context.Context, _ *apiv1.Gaggle) error {
		r, err := s.Queue.EventInGaggle(ctx, gaggle, id)
		if errors.Is(err, sql.ErrNoRows) {
			return httpapi.NewInterventionError(http.StatusNotFound, "event_not_found", "Event receipt is unavailable.", nil)
		}
		if err != nil {
			return err
		}
		result = receiptView(r, false)
		deliveries, err := s.Queue.EventDeliveries(ctx, gaggle, id)
		if err != nil {
			return err
		}
		for _, d := range deliveries {
			view := apicontract.GaggleEventDelivery{Consumer: d.Consumer, GroupID: d.GroupID, Reason: d.Reason}
			if d.GroupID != "" {
				g, err := s.Queue.EventGroup(ctx, gaggle, d.GroupID)
				if err != nil {
					return err
				}
				view.State = g.State
				view.AcceptanceID = g.AcceptanceID
				if g.AcceptanceID != "" {
					record, _, readErr := s.Queue.VerifiedEventStart(ctx, gaggle, d.GroupID)
					if readErr != nil {
						return readErr
					}
					if record.State == triggerqueue.Dispatched {
						view.RunID = record.RunID
					}
				}
			}
			result.Deliveries = append(result.Deliveries, view)
		}
		return nil
	})
	if errors.Is(err, interactiveaccess.ErrDenied) {
		err = denied()
	}
	return result, err
}
