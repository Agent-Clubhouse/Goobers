// Package notification dispatches exact, provider-neutral operational messages
// through separately registered output sinks.
package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

const (
	MaxTitleBytes      = 512
	MaxBodyBytes       = 16 * 1024
	MaxSpeechTextBytes = 4096
	MaxFacts           = 64
	MaxEvidenceRefs    = 64
	MaxSinks           = 16
	maxErrorRunes      = 240
)

// Sink transports pre-rendered content without changing it.
type Sink interface {
	Kind() string
	Version() string
	Deliver(context.Context, apiv1.NotificationRequest) (externalReference string, err error)
}

// Recorder is the durable request/receipt boundary. Delivered is queried before
// dispatch so a process retry cannot repeat an already successful delivery.
type Recorder interface {
	RecordRequest(context.Context, apiv1.NotificationRequest) error
	RecordReceipt(context.Context, apiv1.NotificationReceipt) error
	Delivered(context.Context, string, string) (apiv1.NotificationReceipt, bool, error)
}

// Registry owns sink implementations independently of workflow definitions.
type Registry struct {
	mu    sync.RWMutex
	sinks map[string]Sink
}

func NewRegistry() *Registry {
	return &Registry{sinks: make(map[string]Sink)}
}

func (r *Registry) Register(sink Sink) error {
	if sink == nil {
		return errors.New("notification: sink is required")
	}
	kind := strings.TrimSpace(sink.Kind())
	if kind == "" || strings.TrimSpace(sink.Version()) == "" {
		return errors.New("notification: sink kind and version are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sinks[kind]; exists {
		return fmt.Errorf("notification: sink %q is already registered", kind)
	}
	r.sinks[kind] = sink
	return nil
}

func (r *Registry) lookup(kind string) (Sink, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sink, ok := r.sinks[kind]
	return sink, ok
}

// PartialDeliveryPolicy states when a multi-sink request is successful.
type PartialDeliveryPolicy string

const (
	RequireAll PartialDeliveryPolicy = "require-all"
	RequireAny PartialDeliveryPolicy = "require-any"
)

// Policy bounds each sink independently. MaxAttempts includes the first attempt.
type Policy struct {
	Timeout     time.Duration
	MaxAttempts int
	RetryDelay  time.Duration
	Partial     PartialDeliveryPolicy
}

func (p Policy) normalized() (Policy, error) {
	if p.Timeout == 0 {
		p.Timeout = 15 * time.Second
	}
	if p.MaxAttempts == 0 {
		p.MaxAttempts = 1
	}
	if p.Partial == "" {
		p.Partial = RequireAll
	}
	if p.Timeout <= 0 {
		return p, errors.New("notification: timeout must be positive")
	}
	if p.MaxAttempts < 1 || p.MaxAttempts > 5 {
		return p, errors.New("notification: max attempts must be between 1 and 5")
	}
	if p.RetryDelay < 0 || p.RetryDelay > time.Minute {
		return p, errors.New("notification: retry delay must be between 0 and 1m")
	}
	if p.Partial != RequireAll && p.Partial != RequireAny {
		return p, fmt.Errorf("notification: unknown partial-delivery policy %q", p.Partial)
	}
	return p, nil
}

// Result contains every durable receipt produced by Dispatch.
type Result struct {
	Receipts []apiv1.NotificationReceipt
}

// Dispatcher validates, journals, and delivers requests through registered sinks.
type Dispatcher struct {
	registry *Registry
	recorder Recorder
	policy   Policy
	now      func() time.Time
	mu       sync.Mutex
}

func NewDispatcher(registry *Registry, recorder Recorder, policy Policy) (*Dispatcher, error) {
	if registry == nil {
		return nil, errors.New("notification: registry is required")
	}
	if recorder == nil {
		return nil, errors.New("notification: durable recorder is required")
	}
	normalized, err := policy.normalized()
	if err != nil {
		return nil, err
	}
	return &Dispatcher{registry: registry, recorder: recorder, policy: normalized, now: time.Now}, nil
}

// Dispatch records the request before attempting any sink. The dispatcher
// serializes calls so the durable delivered check and delivery are atomic within
// a runner process.
func (d *Dispatcher) Dispatch(ctx context.Context, request apiv1.NotificationRequest) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ValidateRequest(request); err != nil {
		return Result{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.recorder.RecordRequest(context.WithoutCancel(ctx), request); err != nil {
		return Result{}, fmt.Errorf("notification: record request: %w", err)
	}
	result := Result{Receipts: make([]apiv1.NotificationReceipt, 0, len(request.Sinks))}
	delivered := 0
	for _, kind := range request.Sinks {
		receipts, ok := d.dispatchSink(ctx, request, kind)
		result.Receipts = append(result.Receipts, receipts...)
		if ok {
			delivered++
		}
	}
	if d.policy.Partial == RequireAny && delivered > 0 {
		return result, nil
	}
	if delivered != len(request.Sinks) {
		return result, fmt.Errorf("notification: delivered to %d of %d requested sinks", delivered, len(request.Sinks))
	}
	return result, nil
}

func (d *Dispatcher) dispatchSink(ctx context.Context, request apiv1.NotificationRequest, kind string) ([]apiv1.NotificationReceipt, bool) {
	sink, found := d.registry.lookup(kind)
	if !found {
		receipt := d.receipt(request, apiv1.NotificationSinkRef{Kind: kind, Version: "unknown"}, 0, d.now(), d.now(), apiv1.NotificationSkipped, "", errors.New("sink is not registered"))
		receipt, _ = d.persist(ctx, receipt)
		return []apiv1.NotificationReceipt{receipt}, false
	}
	previous, found, err := d.recorder.Delivered(context.WithoutCancel(ctx), request.IdempotencyKey, kind)
	if err != nil {
		receipt := d.receipt(request, sinkRef(sink), 0, d.now(), d.now(), apiv1.NotificationFailed, "", fmt.Errorf("check idempotency: %w", err))
		receipt, _ = d.persist(ctx, receipt)
		return []apiv1.NotificationReceipt{receipt}, false
	}
	if found {
		receipt := d.receipt(request, sinkRef(sink), 0, d.now(), d.now(), apiv1.NotificationSkipped, previous.ExternalReference, nil)
		receipt, recordErr := d.persist(ctx, receipt)
		return []apiv1.NotificationReceipt{receipt}, recordErr == nil
	}
	if err := ctx.Err(); err != nil {
		receipt := d.receipt(request, sinkRef(sink), 0, d.now(), d.now(), apiv1.NotificationSkipped, "", err)
		receipt, _ = d.persist(ctx, receipt)
		return []apiv1.NotificationReceipt{receipt}, false
	}
	if !d.now().Before(request.ExpiresAt) {
		receipt := d.receipt(request, sinkRef(sink), 0, d.now(), d.now(), apiv1.NotificationSkipped, "", errors.New("notification expired"))
		receipt, _ = d.persist(ctx, receipt)
		return []apiv1.NotificationReceipt{receipt}, false
	}

	receipts := make([]apiv1.NotificationReceipt, 0, d.policy.MaxAttempts)
	for attempt := 1; attempt <= d.policy.MaxAttempts; attempt++ {
		started := d.now().UTC()
		deadline := started.Add(d.policy.Timeout)
		if request.ExpiresAt.Before(deadline) {
			deadline = request.ExpiresAt
		}
		attemptCtx, cancel := context.WithDeadline(ctx, deadline)
		externalRef, deliverErr := sink.Deliver(attemptCtx, request)
		if attemptCtx.Err() != nil {
			deliverErr = attemptCtx.Err()
		}
		cancel()
		status := apiv1.NotificationDelivered
		if deliverErr != nil {
			status = apiv1.NotificationFailed
		}
		receipt := d.receipt(request, sinkRef(sink), attempt, started, d.now().UTC(), status, externalRef, deliverErr)
		var recordErr error
		receipt, recordErr = d.persist(ctx, receipt)
		receipts = append(receipts, receipt)
		if recordErr != nil {
			// Delivery may already have happened. Retrying without its durable
			// receipt could duplicate the external side effect.
			return receipts, false
		}
		if receipt.Status == apiv1.NotificationDelivered {
			return receipts, true
		}
		if ctx.Err() != nil || attempt == d.policy.MaxAttempts {
			break
		}
		if d.policy.RetryDelay > 0 {
			timer := time.NewTimer(d.policy.RetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return receipts, false
			case <-timer.C:
			}
		}
	}
	return receipts, false
}

func (d *Dispatcher) persist(ctx context.Context, receipt apiv1.NotificationReceipt) (apiv1.NotificationReceipt, error) {
	if err := d.recorder.RecordReceipt(context.WithoutCancel(ctx), receipt); err != nil {
		receipt.Status = apiv1.NotificationFailed
		receipt.Error = sanitizeError(fmt.Errorf("record receipt: %w", err))
		return receipt, err
	}
	return receipt, nil
}

func (d *Dispatcher) receipt(request apiv1.NotificationRequest, sink apiv1.NotificationSinkRef, attempt int, started, completed time.Time, status apiv1.NotificationDeliveryStatus, externalRef string, err error) apiv1.NotificationReceipt {
	return apiv1.NotificationReceipt{
		Schema: apiv1.NotificationReceiptSchema, NotificationID: request.NotificationID,
		IdempotencyKey: request.IdempotencyKey, Source: request.Source, Evidence: request.Evidence,
		Sink: sink, Attempt: attempt, StartedAt: started.UTC(), CompletedAt: completed.UTC(),
		Status: status, ExternalReference: externalRef, Error: sanitizeError(err),
	}
}

func sinkRef(sink Sink) apiv1.NotificationSinkRef {
	return apiv1.NotificationSinkRef{Kind: sink.Kind(), Version: sink.Version()}
}

func ValidateRequest(request apiv1.NotificationRequest) error {
	if request.Schema != apiv1.NotificationRequestSchema {
		return fmt.Errorf("notification: unknown request schema %q", request.Schema)
	}
	required := map[string]string{
		"notification id": request.NotificationID, "incident id": request.IncidentID,
		"event id": request.EventID, "transition": request.Transition,
		"title": request.Title, "body": request.Body, "source run id": request.Source.RunID,
		"source workflow": request.Source.Workflow, "source stage": request.Source.Stage,
		"idempotency key": request.IdempotencyKey,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("notification: %s is required", name)
		}
	}
	for name, value := range map[string]string{
		"notification id": request.NotificationID, "incident id": request.IncidentID,
		"event id": request.EventID, "idempotency key": request.IdempotencyKey,
	} {
		if len(value) > 256 {
			return fmt.Errorf("notification: %s exceeds 256 bytes", name)
		}
	}
	if len(request.Transition) > 128 {
		return errors.New("notification: transition exceeds 128 bytes")
	}
	switch request.Severity {
	case apiv1.NotificationSeverityInfo, apiv1.NotificationSeverityWarning,
		apiv1.NotificationSeverityError, apiv1.NotificationSeverityCritical:
	default:
		return fmt.Errorf("notification: invalid severity %q", request.Severity)
	}
	if len(request.Title) > MaxTitleBytes || len(request.Body) > MaxBodyBytes || len(request.SpeechText) > MaxSpeechTextBytes {
		return errors.New("notification: exact message payload exceeds size limits")
	}
	if request.ExpiresAt.IsZero() {
		return errors.New("notification: expiry is required")
	}
	if len(request.Sinks) == 0 || len(request.Sinks) > MaxSinks {
		return fmt.Errorf("notification: between 1 and %d sinks are required", MaxSinks)
	}
	if len(request.Facts) > MaxFacts || len(request.Evidence) > MaxEvidenceRefs {
		return errors.New("notification: fact or evidence limit exceeded")
	}
	seen := make(map[string]struct{}, len(request.Sinks))
	for _, sink := range request.Sinks {
		if strings.TrimSpace(sink) == "" || len(sink) > 64 {
			return errors.New("notification: sink kind is required")
		}
		if _, exists := seen[sink]; exists {
			return fmt.Errorf("notification: duplicate sink %q", sink)
		}
		seen[sink] = struct{}{}
	}
	for _, fact := range request.Facts {
		if strings.TrimSpace(fact.Name) == "" || len(fact.Name) > 128 || len(fact.Value) > 2048 {
			return errors.New("notification: invalid fact")
		}
	}
	for _, evidence := range request.Evidence {
		if strings.TrimSpace(evidence.Kind) == "" || len(evidence.Kind) > 64 ||
			strings.TrimSpace(evidence.ID) == "" || len(evidence.ID) > 512 {
			return errors.New("notification: invalid evidence reference")
		}
		if evidence.Digest != "" && !validSHA256Digest(evidence.Digest) {
			return errors.New("notification: invalid evidence digest")
		}
	}
	return nil
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, r := range value[len("sha256:"):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, err.Error())
	message = strings.Join(strings.Fields(message), " ")
	message = string(journal.NewPatternScrubber().Scrub([]byte(message)))
	if utf8.RuneCountInString(message) > maxErrorRunes {
		runes := []rune(message)
		message = string(runes[:maxErrorRunes]) + "..."
	}
	return message
}
