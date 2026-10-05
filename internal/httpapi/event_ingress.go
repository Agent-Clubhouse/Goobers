package httpapi

import (
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/eventing"
)

// GaggleEventService separates authenticated transport from applied producer
// policy, durable routing and explicit human receipt visibility.
type GaggleEventService interface {
	PublishEvent(context.Context, Principal, string, string, []byte) (apicontract.GaggleEventReceipt, error)
	EventReceipt(context.Context, Principal, string, string) (apicontract.GaggleEventReceipt, error)
}

// WithGaggleEvents installs the single-listener scoped event adapter.
func WithGaggleEvents(service GaggleEventService) HandlerOption {
	return func(c *handlerConfig) error {
		if service == nil {
			return errors.New("gaggle event service required")
		}
		c.gaggleEvents = service
		return nil
	}
}
func registerGaggleEventRoutes(router *Router, c handlerConfig, errorLog *log.Logger) {
	router.Handle(apicontract.RouteGaggleEventPublish, gaggleEventHandler(c.gaggleEvents, true, errorLog))
	router.Handle(apicontract.RouteGaggleEventReceipt, gaggleEventHandler(c.gaggleEvents, false, errorLog))
}
func gaggleEventHandler(service GaggleEventService, publish bool, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := PrincipalFromRequest(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "event_authentication_required", "Event access requires authentication.")
			return
		}
		if p.Issuer == "" || p.Subject == "" || strings.HasPrefix(p.Issuer, "goobers/") || p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) > 0 {
			writeError(w, http.StatusForbidden, "event_scope_denied", "Event access is not authorized.")
			return
		}
		if service == nil {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, "event_service_unavailable", "Event ingress is unavailable.")
			return
		}
		if r.URL.RawQuery != "" || len(r.PathValue("gaggle")) > 253 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "Invalid event scope.")
			return
		}
		var result apicontract.GaggleEventReceipt
		var err error
		status := http.StatusOK
		if publish {
			var raw []byte
			raw, err = eventIngressBody(w, r)
			if err != nil {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "A bounded structured CloudEvent and one binding header are required.")
				return
			}
			result, err = service.PublishEvent(r.Context(), p, r.PathValue("gaggle"), r.Header.Get(apicontract.EventBindingHeader), raw)
			status = http.StatusAccepted
		} else {
			id := r.PathValue("receipt")
			if len(id) != 38 || !strings.HasPrefix(id, "event-") {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "Invalid event receipt.")
				return
			}
			result, err = service.EventReceipt(r.Context(), p, r.PathValue("gaggle"), id)
		}
		if err != nil {
			var e *InterventionError
			if errors.As(err, &e) && (e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable) {
				w.Header().Set("Retry-After", "1")
			}
			writePlaneError(w, errorLog, "gaggle event", err)
			return
		}
		writeJSON(w, status, result)
	}
}
func eventIngressBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	values := r.Header.Values(apicontract.EventBindingHeader)
	if len(values) != 1 || values[0] == "" || len(values[0]) > 128 || strings.TrimSpace(values[0]) != values[0] {
		return nil, errors.New("invalid event binding")
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (media != "application/cloudevents+json" && media != "application/json") {
		return nil, errors.New("invalid event content type")
	}
	return io.ReadAll(http.MaxBytesReader(w, r.Body, eventing.MaxEnvelopeBytes))
}
