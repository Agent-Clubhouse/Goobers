// Package webhook authenticates GitHub webhook deliveries and translates them
// into the existing scheduler signal namespace.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

const (
	// Path is the daemon endpoint for GitHub webhook deliveries.
	Path = "/webhooks/github"

	signatureHeader = "X-Hub-Signature-256"
	eventHeader     = "X-GitHub-Event"
	deliveryHeader  = "X-GitHub-Delivery"
	signalPrefix    = "github-webhook:"
	maxBodyBytes    = 25 << 20
	maxHeaderBytes  = 256
	maxDeliveries   = 10000
)

// Signaler is the existing scheduler signal delivery seam.
type Signaler interface {
	Signal(context.Context, string, time.Time) []string
}

// InstanceJournal records rejected deliveries without creating a run.
type InstanceJournal interface {
	Append(journal.Event) error
}

// SignalName returns the internal signal subscription for a GitHub event.
func SignalName(event string) string {
	return signalPrefix + strings.TrimSpace(event)
}

// ValidSignature reports whether signature authenticates body with secret using
// GitHub's X-Hub-Signature-256 format.
func ValidSignature(secret, body []byte, signature string) bool {
	algorithm, encoded, ok := strings.Cut(signature, "=")
	if !ok || algorithm != "sha256" {
		return false
	}
	provided, err := hex.DecodeString(encoded)
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}

// Handler verifies and routes GitHub deliveries. Delivery IDs are retained in a
// bounded in-memory set so an identical delivery is acknowledged without
// waking workflows twice during one daemon lifetime.
type Handler struct {
	ctx      context.Context
	secret   []byte
	signaler Signaler
	journal  InstanceJournal
	now      func() time.Time

	mu         sync.Mutex
	deliveries map[string]struct{}
	order      []string
}

// NewHandler constructs the authenticated GitHub webhook endpoint.
func NewHandler(ctx context.Context, secret []byte, signaler Signaler, instanceJournal InstanceJournal) (*Handler, error) {
	if ctx == nil {
		return nil, errors.New("webhook context is required")
	}
	if len(secret) == 0 {
		return nil, errors.New("webhook secret is required")
	}
	if signaler == nil {
		return nil, errors.New("webhook signaler is required")
	}
	if instanceJournal == nil {
		return nil, errors.New("webhook instance journal is required")
	}
	copiedSecret := append([]byte(nil), secret...)
	return &Handler{
		ctx:        ctx,
		secret:     copiedSecret,
		signaler:   signaler,
		journal:    instanceJournal,
		now:        time.Now,
		deliveries: make(map[string]struct{}),
	}, nil
}

// ServeHTTP accepts only POST deliveries at Path.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != Path {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !ValidSignature(h.secret, body, r.Header.Get(signatureHeader)) {
		if err := h.journal.Append(journal.Event{
			Type:  journal.EventError,
			Error: &journal.ErrorDetail{Code: "webhook_signature_invalid", Message: "GitHub webhook signature verification failed"},
		}); err != nil {
			http.Error(w, "record webhook rejection", http.StatusInternalServerError)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	event := strings.TrimSpace(r.Header.Get(eventHeader))
	delivery := strings.TrimSpace(r.Header.Get(deliveryHeader))
	if event == "" || len(event) > maxHeaderBytes {
		http.Error(w, fmt.Sprintf("%s must be present and no longer than %d bytes", eventHeader, maxHeaderBytes), http.StatusBadRequest)
		return
	}
	if delivery == "" || len(delivery) > maxHeaderBytes {
		http.Error(w, fmt.Sprintf("%s must be present and no longer than %d bytes", deliveryHeader, maxHeaderBytes), http.StatusBadRequest)
		return
	}
	if h.seen(delivery) {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	h.signaler.Signal(h.ctx, SignalName(event), h.now())
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) seen(delivery string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.deliveries[delivery]; ok {
		return true
	}
	h.deliveries[delivery] = struct{}{}
	h.order = append(h.order, delivery)
	if len(h.order) > maxDeliveries {
		delete(h.deliveries, h.order[0])
		h.order = h.order[1:]
	}
	return false
}
