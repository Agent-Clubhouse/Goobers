package startintent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

// Sources captures trusted scheduler observations directly from immutable entry
// pins. Acquire must not acquire the applied catalog or scheduler locks: the
// caller holds tickMu and may hold admissionMu while accepting worker starts.
type Sources struct {
	Queue   *triggerqueue.Store
	Acquire func(context.Context, Target) (func(), error)
}

// AcceptSignal commits a closed recipient set, including an empty match. Exact
// retries never rebind the delivery to a later configuration or additional gaggle.
func (s *Sources) AcceptSignal(ctx context.Context, entries []localscheduler.WorkflowEntry, key, name, ref string, delivery *webhookhttp.Delivery, now time.Time) ([]string, error) {
	if !text(key, 256, true) || !text(name, 256, true) || !text(ref, 512, true) || now.IsZero() {
		return nil, errors.New("startintent: invalid source signal")
	}
	actor := "local-signal"
	if delivery != nil {
		actor = "github-webhook"
	}
	sourceKey := sourceHash(actor, key)
	fingerprintRaw, err := json.Marshal(struct {
		Name, Ref string
		Delivery  *webhookhttp.Delivery
	}{name, ref, delivery})
	if err != nil {
		return nil, err
	}
	fingerprint := sourceHash(string(fingerprintRaw))
	if prior, err := s.Queue.SourceReceipt(ctx, sourceKey); err == nil {
		if prior.Actor != actor || prior.Fingerprint != fingerprint {
			return nil, triggerqueue.ErrConflict
		}
		return prior.AcceptanceIDs, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if len(entries) > triggerqueue.MaxSourceStarts {
		return nil, errors.New("startintent: signal recipient limit exceeded")
	}
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	batch := triggerqueue.SourceBatch{Key: sourceKey, Actor: actor, Fingerprint: fingerprint}
	for _, entry := range entries {
		raw, release, err := s.pin(ctx, entry, localscheduler.SourceTrigger{Signal: name, Ref: ref, Webhook: delivery != nil})
		if err != nil {
			return nil, err
		}
		releases = append(releases, release)
		batch.Starts = append(batch.Starts, triggerqueue.SourceStart{Payload: raw})
	}
	receipt, _, err := s.Queue.AcceptSource(ctx, batch, now)
	return receipt.AcceptanceIDs, err
}

func (s *Sources) pin(ctx context.Context, entry localscheduler.WorkflowEntry, source localscheduler.SourceTrigger) ([]byte, func(), error) {
	target := Target{Gaggle: entry.Gaggle, Workflow: entry.Workflow, ConfigGeneration: entry.ConfigGeneration, WorkflowDigest: entry.WorkflowDigest, GooberDigest: entry.GooberDigest}
	raw, err := (Envelope{Kind: Kind, Request: Request{Workflow: entry.Workflow, Gaggle: entry.Gaggle}, Target: target, Source: &source}).Marshal()
	if err != nil {
		return nil, nil, err
	}
	if s.Acquire == nil {
		return nil, nil, errors.New("startintent: source archive lease unavailable")
	}
	release, err := s.Acquire(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	if release == nil {
		return nil, nil, errors.New("startintent: source archive lease missing")
	}
	return raw, release, nil
}
func sourceHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}
