package terminalreport

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/providers"
)

// Queue stores intents independently of run retention and active claim leases.
// Log appends daemon-owned outcomes to the instance journal, including after
// run.finished, without modifying an engine-owned run journal.
type Queue struct {
	Dir    string
	Gaggle string
	Now    func() time.Time
	Log    func(journal.Event) error
}

func (q Queue) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

func effectKey(e Effect) string {
	identity := string(e.Kind) + "\x00" + e.Repository.CanonicalKey() + "\x00" + e.ItemID
	if e.Kind == AbortLabel {
		identity += "\x00" + e.RunID
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}

// entry separates durable admission from the delivery lease. File locks protect
// only disk operations; remote calls never hold them. Generation and Lease guard
// acknowledgements against a newer intent or a reclaimed delivery lease.
type entry struct {
	Effect
	Generation   string    `json:"generation"`
	Lease        string    `json:"lease,omitempty"`
	LeaseUntil   time.Time `json:"leaseUntil,omitempty"`
	Body         string    `json:"body,omitempty"`
	callDeadline time.Time // Acquisition-time monotonic deadline; never serialized.
}

var errDeliveryPending = errors.New("terminal report delivery already in progress")

func readEntry(path string) (entry, error) {
	var e entry
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &e)
	}
	return e, err
}

// Publish durably admits the newest projection even while an older generation
// is in flight. Admission does not wait for provider calls or the delivery lease.
func (q Queue) Publish(ctx context.Context, e Effect, apply func(context.Context, Effect) error) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	key := effectKey(e)
	err := q.withEntry(persistCtx, key, func(path string) error {
		previous, err := readEntry(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		next := entry{Effect: e, Generation: crand.Text(), Lease: previous.Lease, LeaseUntil: previous.LeaseUntil}
		if q.now().Before(previous.RetryAt) {
			next.RetryAt, next.Attempts = previous.RetryAt, previous.Attempts
		}
		// Re-admission of the same intent must retain its first rendered body.
		if reportIdentity(previous.Effect) == reportIdentity(e) {
			next.Body = previous.Body
		}
		return save(path, next)
	})
	if err != nil {
		return err
	}
	replay, entered, err := q.enterReplay()
	if err != nil {
		return err
	}
	if !entered {
		return errDeliveryPending
	}
	defer func() { _ = replay.Release() }()
	_, err = q.deliver(ctx, key, apply, true)
	return err
}

func reportIdentity(e Effect) string {
	identity, _ := json.Marshal([]any{effectKey(e), e.RunID, e.Stage, e.Count, e.Reset})
	return string(identity)
}

// Reconcile retries at most budget due intents while holding the outbox-wide
// replay lock. Publishers still enqueue but do not deliver while another
// process owns this loop.
func (q Queue) Reconcile(ctx context.Context, budget int, apply func(context.Context, Effect) error) error {
	replay, entered, err := q.enterReplay()
	if err != nil {
		return err
	}
	if !entered {
		return nil
	}
	defer func() { _ = replay.Release() }()

	entries, err := os.ReadDir(q.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if budget <= 0 || ctx.Err() != nil {
			break
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		attempted, err := q.deliver(ctx, strings.TrimSuffix(entry.Name(), ".json"), apply, false)
		if attempted {
			budget--
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (q Queue) claim(ctx context.Context, key string) (entry, error) {
	var claimed entry
	err := q.withEntry(ctx, key, func(path string) error {
		e, err := readEntry(path)
		if err != nil {
			return err
		}
		if q.now().Before(e.RetryAt) {
			return &providers.RateLimitError{Provider: e.Repository.Provider, Reset: e.RetryAt}
		}
		if e.Lease != "" && q.now().Before(e.LeaseUntil) {
			return errDeliveryPending
		}
		// Capture wall and monotonic clocks together. The persisted lease is
		// portable across processes; the call deadline retains its acquisition-time
		// duration and cannot gain time from a later wall-clock adjustment.
		acquired := time.Now()
		leaseNow := q.now()
		e.Lease, e.LeaseUntil = crand.Text(), leaseNow.Add(reportLeaseDuration)
		remaining := e.LeaseUntil.Sub(leaseNow) - reportLeaseSkewMargin
		e.callDeadline = acquired.Add(min(reportCallTimeout, remaining))
		if e.Generation == "" {
			e.Generation = crand.Text()
		}
		if err := save(path, e); err != nil {
			return err
		}
		claimed = e
		return nil
	})
	return claimed, err
}

func (q Queue) deliver(ctx context.Context, key string, apply func(context.Context, Effect) error, publishing bool) (bool, error) {
	release, entered := enterFlight(key)
	if !entered {
		if publishing {
			return false, errDeliveryPending
		}
		return false, nil
	}
	defer release()
	e, err := q.claim(ctx, key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if !publishing {
			if _, limited := providers.AsRateLimitError(err); limited || errors.Is(err, errDeliveryPending) {
				return false, nil
			}
		}
		return false, err
	}
	attemptCtx, cancel := context.WithDeadline(ctx, e.callDeadline)
	defer cancel()
	attemptCtx = gate.WithFailureReportBody(attemptCtx, e.Body, func(body string) error {
		return q.freeze(attemptCtx, key, e, body)
	})
	err = attemptCtx.Err()
	if err == nil {
		err = apply(attemptCtx, e.Effect)
	}
	return true, q.acknowledge(ctx, key, e, err)
}

// freeze persists the rendered body before any provider write. If admission
// superseded this generation during preparation, the old write is skipped.
func (q Queue) freeze(ctx context.Context, key string, delivered entry, body string) error {
	return q.withEntry(ctx, key, func(path string) error {
		current, err := readEntry(path)
		if err != nil {
			return err
		}
		if current.Generation != delivered.Generation || current.Lease != delivered.Lease {
			return ErrSuperseded
		}
		current.Body = body
		return save(path, current)
	})
}

func (q Queue) acknowledge(ctx context.Context, key string, delivered entry, deliveryErr error) error {
	outcome := "delivered"
	var duplicates *gate.DuplicateReportCleanupUnsupported
	if errors.As(deliveryErr, &duplicates) {
		outcome = "duplicate_cleanup_unsupported"
	} else if errors.Is(deliveryErr, ErrSuperseded) {
		outcome = "superseded"
	} else if deliveryErr != nil {
		outcome = "deferred"
	}
	if outcome == "deferred" {
		delivered.Effect = q.deferred(delivered.Effect, deliveryErr)
	}
	logErr := q.record(delivered.Effect, outcome, duplicates)
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := q.withEntry(persistCtx, key, func(path string) error {
		current, err := readEntry(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Lease != delivered.Lease {
			return nil
		}
		current.Lease, current.LeaseUntil = "", time.Time{}
		if current.Generation != delivered.Generation || logErr != nil {
			// A quota response from the older call still constrains the same target.
			if _, limited := providers.AsRateLimitError(deliveryErr); limited && current.RetryAt.Before(delivered.RetryAt) {
				current.RetryAt = delivered.RetryAt
			}
			return save(path, current)
		}
		if outcome == "deferred" {
			current.RetryAt, current.Attempts = delivered.RetryAt, delivered.Attempts
			return save(path, current)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		return durability.SyncDir(q.Dir)
	})
	if errors.Is(deliveryErr, ErrSuperseded) || duplicates != nil {
		deliveryErr = nil
	}
	return errors.Join(deliveryErr, logErr, err)
}

func (q Queue) enterReplay() (*lock.Handle, bool, error) {
	if err := os.MkdirAll(q.Dir, 0o700); err != nil {
		return nil, false, err
	}
	held, err := lock.TryAcquire(filepath.Join(q.Dir, replayLockFileName))
	if errors.Is(err, lock.ErrHeld) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return held, true, nil
}

func (q Queue) deferred(e Effect, err error) Effect {
	e.Attempts++
	delay := time.Minute
	if limit, ok := providers.AsRateLimitError(err); ok {
		delay = limit.RetryDelay(q.now())
	}
	if delay < time.Duration(1<<63-1)-3*time.Second {
		delay += 2*time.Second + time.Duration(rand.Int64N(int64(time.Second)))
	}
	e.RetryAt = q.now().Add(delay)
	return e
}

func (q Queue) record(e Effect, outcome string, duplicates *gate.DuplicateReportCleanupUnsupported) error {
	if q.Log == nil {
		return nil
	}
	event := journal.Event{
		Type: journal.EventRunnerAnnotation, RunID: e.RunID, Stage: e.Stage,
		Reason: "terminal provider report " + outcome,
		Runner: map[string]any{"kind": "terminal_report_" + outcome, "reportKind": string(e.Kind), "itemId": e.ItemID,
			"retryAt": e.RetryAt.UTC().Format(time.RFC3339Nano), "attempts": e.Attempts},
	}
	if duplicates != nil {
		event.Reason = duplicates.Error()
		event.Runner["canonicalCommentId"] = duplicates.Canonical.ID
		event.Runner["canonicalCommentUrl"] = duplicates.Canonical.URL
		event.Runner["duplicateCommentIds"] = duplicates.DuplicateIDs
	}
	return q.Log(event)
}

func save(path string, e entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return durability.WriteFileAtomic(path, data, 0o600)
}

func (q Queue) withEntry(ctx context.Context, key string, fn func(string) error) error {
	if err := os.MkdirAll(q.Dir, 0o700); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		lease, err := lock.TryAcquire(filepath.Join(q.Dir, key+".lock"))
		if err == nil {
			defer func() { _ = lease.Release() }()
			return fn(filepath.Join(q.Dir, key+".json"))
		}
		if !errors.Is(err, lock.ErrHeld) {
			return err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
