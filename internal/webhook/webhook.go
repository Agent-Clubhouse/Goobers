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

// DispatchGate serializes webhook acceptance with daemon shutdown.
type DispatchGate struct {
	mu        sync.RWMutex
	ctx       context.Context
	cancel    context.CancelFunc
	accepting bool
	stopped   bool
}

// NewDispatchGate creates a stopped gate whose context is canceled only while
// holding the shutdown side of the gate.
func NewDispatchGate(parent context.Context) (*DispatchGate, error) {
	if parent == nil {
		return nil, errors.New("webhook dispatch context is required")
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	return &DispatchGate{ctx: ctx, cancel: cancel}, nil
}

// Context returns the daemon context governed by the gate.
func (g *DispatchGate) Context() context.Context {
	return g.ctx
}

// Start begins accepting webhook dispatches unless shutdown has already begun.
func (g *DispatchGate) Start() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return false
	}
	g.accepting = true
	return true
}

// Stop prevents new dispatches, waits for an accepted dispatch to return, and
// then cancels the daemon context before releasing the gate.
func (g *DispatchGate) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return
	}
	g.accepting = false
	g.stopped = true
	g.cancel()
}

func (g *DispatchGate) isAccepting() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.accepting
}

func (g *DispatchGate) dispatch(signal func(context.Context)) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.accepting {
		return false
	}
	signal(g.ctx)
	return true
}

// SignalName returns the internal signal subscription for a GitHub event.
func SignalName(event string) string {
	return signalPrefix + strings.TrimSpace(event)
}

// ValidSignature reports whether signature authenticates body with secret using
// GitHub's X-Hub-Signature-256 format.
func ValidSignature(secret, body []byte, signature string) bool {
	provided, ok := signatureDigest(signature)
	if !ok {
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
	secret   []byte
	signaler Signaler
	journal  InstanceJournal
	gate     *DispatchGate
	now      func() time.Time

	mu         sync.Mutex
	deliveries map[string]struct{}
	order      []string
}

// NewHandler constructs the authenticated GitHub webhook endpoint.
func NewHandler(secret []byte, signaler Signaler, instanceJournal InstanceJournal, gate *DispatchGate) (*Handler, error) {
	if len(secret) == 0 {
		return nil, errors.New("webhook secret is required")
	}
	if signaler == nil {
		return nil, errors.New("webhook signaler is required")
	}
	if instanceJournal == nil {
		return nil, errors.New("webhook instance journal is required")
	}
	if gate == nil {
		return nil, errors.New("webhook dispatch gate is required")
	}
	copiedSecret := append([]byte(nil), secret...)
	return &Handler{
		secret:     copiedSecret,
		signaler:   signaler,
		journal:    instanceJournal,
		gate:       gate,
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
	if !h.gate.isAccepting() {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	if r.ContentLength > maxBodyBytes {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	provided, ok := signatureDigest(r.Header.Get(signatureHeader))
	if !ok {
		h.rejectInvalidSignature(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	mac := hmac.New(sha256.New, h.secret)
	_, err := io.Copy(mac, r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !hmac.Equal(provided, mac.Sum(nil)) {
		h.rejectInvalidSignature(w)
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
	now := h.now()
	if !h.gate.dispatch(func(ctx context.Context) {
		if h.seen(delivery) {
			return
		}
		h.signaler.Signal(ctx, SignalName(event), now)
	}) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) rejectInvalidSignature(w http.ResponseWriter) {
	if err := h.journal.Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: "webhook_signature_invalid", Message: "GitHub webhook signature verification failed"},
	}); err != nil {
		http.Error(w, "record webhook rejection", http.StatusInternalServerError)
		return
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
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

func signatureDigest(signature string) ([]byte, bool) {
	algorithm, encoded, ok := strings.Cut(signature, "=")
	if !ok || algorithm != "sha256" {
		return nil, false
	}
	provided, err := hex.DecodeString(encoded)
	if err != nil || len(provided) != sha256.Size {
		return nil, false
	}
	return provided, true
}
