// Package webhookintake accepts authenticated GitHub webhook deliveries on an
// API replica and records them durably in PostgreSQL. It never schedules or
// dispatches: the delivery is acknowledged once its source receipt and every
// recipient acceptance are committed, and the single-active controller starts
// them from control.acceptance.
package webhookintake

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/webhook"
)

// ErrPinUnavailable means workflow digests cannot be pinned, so no start can be
// admitted. Intake fails closed (the delivery is retried) rather than queue a
// start the controller cannot resolve.
var ErrPinUnavailable = errors.New("webhook intake: workflow digest pinning is not available")

// Subscriptions resolves the applied generation's webhook recipients.
type Subscriptions interface {
	WebhookRecipients(event, owner, name string) (generation string, subs []cloudconfig.WebhookSubscription, err error)
}

// Pin returns the immutable digests a start is admitted against.
type Pin func(ctx context.Context, generation, gaggle, workflow string) (workflowDigest, gooberDigest string, err error)

// SourceStore durably records a source batch without a controller lease.
type SourceStore interface {
	AcceptSourceAPI(ctx context.Context, batch triggerqueue.SourceBatch, now time.Time) (triggerqueue.SourceReceipt, bool, error)
}

// Intake is the webhook handler's durable acknowledgement.
type Intake struct {
	Store SourceStore
	Subs  Subscriptions
	Pin   Pin
}

// AcceptWebhook implements webhook.DurableSignaler. The delivery ID owns the
// recipient set: a redelivery, to any replica, returns the receipt already
// committed and never rebinds to a later configuration.
func (i *Intake) AcceptWebhook(ctx context.Context, d webhook.Delivery, now time.Time) ([]string, error) {
	if i.Store == nil || i.Subs == nil || i.Pin == nil {
		return nil, ErrPinUnavailable
	}
	name, ref := webhook.SignalName(d.Event), webhook.TriggerRef(d)
	key, fingerprint, actor, err := startintent.SignalReceiptIdentity(d.ID, name, ref, &d)
	if err != nil {
		return nil, err
	}
	generation, subs, err := i.Subs.WebhookRecipients(d.Event, d.RepositoryOwner, d.RepositoryName)
	if err != nil {
		return nil, err
	}
	batch := triggerqueue.SourceBatch{Key: key, Actor: actor, Fingerprint: fingerprint}
	for _, sub := range subs {
		workflowDigest, gooberDigest, err := i.Pin(ctx, generation, sub.Gaggle, sub.Workflow)
		if err != nil {
			return nil, fmt.Errorf("pin %s/%s: %w", sub.Gaggle, sub.Workflow, err)
		}
		raw, err := (startintent.Envelope{
			Kind:    startintent.Kind,
			Request: startintent.Request{Workflow: sub.Workflow, Gaggle: sub.Gaggle},
			Target: startintent.Target{
				Gaggle: sub.Gaggle, Workflow: sub.Workflow, ConfigGeneration: generation,
				WorkflowDigest: workflowDigest, GooberDigest: gooberDigest,
			},
			Source:   &localscheduler.SourceTrigger{Signal: name, Ref: ref, Webhook: true},
			Deadline: now.Add(startintent.DefaultPendingAge).UTC(),
		}).Marshal()
		if err != nil {
			return nil, err
		}
		batch.Starts = append(batch.Starts, triggerqueue.SourceStart{Payload: raw})
	}
	receipt, _, err := i.Store.AcceptSourceAPI(ctx, batch, now)
	return receipt.AcceptanceIDs, err
}

// noSignaler satisfies the handler's legacy seam; the durable path is always set.
type noSignaler struct{}

func (noSignaler) SignalWebhook(context.Context, webhook.Delivery, time.Time) []string { return nil }

// logJournal records rejected deliveries to the process log only: a delivery
// that fails authentication must leave no durable receipt or acceptance.
type logJournal struct{ logger *log.Logger }

func (j logJournal) Append(ev journal.Event) error {
	if ev.Error != nil {
		j.logger.Printf("webhook rejected: %s: %s", ev.Error.Code, ev.Error.Message)
	}
	return nil
}

// NewHandler returns the authenticated GitHub webhook endpoint (webhook.Path)
// and a stop func that refuses further deliveries. The HMAC is verified by the
// handler before the intake runs.
func NewHandler(ctx context.Context, secret []byte, intake *Intake, logger *log.Logger) (http.Handler, func(), error) {
	if intake == nil {
		return nil, nil, errors.New("webhook intake is required")
	}
	gate, err := webhook.NewDispatchGate(ctx)
	if err != nil {
		return nil, nil, err
	}
	handler, err := webhook.NewHandler(secret, noSignaler{}, logJournal{logger}, gate, webhook.WithDurableSignaler(intake))
	if err != nil {
		return nil, nil, err
	}
	if !gate.Start() {
		return nil, nil, errors.New("webhook dispatch gate did not start")
	}
	return handler, gate.Stop, nil
}
