package terminalreport

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	identity := string(e.Kind) + "\x00" + string(e.Repository.Provider) + "\x00" + e.Repository.Owner + "\x00" + e.Repository.Project + "\x00" + e.Repository.Name + "\x00" + e.ItemID
	if e.Kind == AbortLabel {
		identity += "\x00" + e.RunID
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}

// Publish persists before attempting the provider write. A quota rejection
// retains the intent with its advertised retry time; the error still reaches
// the original caller's journal.
func (q Queue) Publish(ctx context.Context, e Effect, apply func(context.Context, Effect) error) error {
	// A terminal handler often arrives with an expired stage context. Its
	// publication may be canceled, but the durable intent must survive that.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return q.withEntry(persistCtx, effectKey(e), func(path string) error {
		// Repeated finalization or a newer projection must respect an already
		// advertised quota window for this same provider target.
		previous, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			var pending Effect
			if err := json.Unmarshal(previous, &pending); err != nil {
				return err
			}
			if q.now().Before(pending.RetryAt) {
				e.RetryAt, e.Attempts = pending.RetryAt, pending.Attempts
			}
		}
		if err := save(path, e); err != nil {
			return err
		}
		if q.now().Before(e.RetryAt) {
			return errors.Join(&providers.RateLimitError{Provider: e.Repository.Provider, Reset: e.RetryAt}, q.record(e, "deferred"))
		}
		return q.attempt(ctx, path, e, apply)
	})
}

// Reconcile retries at most budget due intents. Future quota windows make no
// provider calls. Per-intent locks serialize concurrent finalizers/reconcilers
// without blocking unrelated items behind a slow provider.
func (q Queue) Reconcile(ctx context.Context, budget int, apply func(context.Context, Effect) error) error {
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
		err := q.withEntry(ctx, strings.TrimSuffix(entry.Name(), ".json"), func(path string) error {
			data, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			var e Effect
			if err := json.Unmarshal(data, &e); err != nil {
				return err
			}
			if q.now().Before(e.RetryAt) {
				return nil
			}
			budget--
			attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return q.attempt(attemptCtx, path, e, apply)
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (q Queue) attempt(ctx context.Context, path string, e Effect, apply func(context.Context, Effect) error) error {
	err := ctx.Err()
	if err == nil {
		err = apply(ctx, e)
	}
	if err == nil || errors.Is(err, ErrSuperseded) {
		outcome := "delivered"
		if err != nil {
			outcome = "superseded"
		}
		if err := q.record(e, outcome); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		return durability.SyncDir(q.Dir)
	}
	e.Attempts++
	delay := time.Minute
	if limit, ok := providers.AsRateLimitError(err); ok {
		delay = limit.RetryDelay(q.now())
	}
	if delay < time.Duration(1<<63-1)-3*time.Second {
		delay += 2*time.Second + time.Duration(rand.Int64N(int64(time.Second)))
	}
	e.RetryAt = q.now().Add(delay)
	if saveErr := save(path, e); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	return errors.Join(err, q.record(e, "deferred"))
}

func (q Queue) record(e Effect, outcome string) error {
	if q.Log == nil {
		return nil
	}
	return q.Log(journal.Event{
		Type: journal.EventRunnerAnnotation, RunID: e.RunID, Stage: e.Stage,
		Reason: "terminal provider report " + outcome,
		Runner: map[string]any{"kind": "terminal_report_" + outcome, "reportKind": string(e.Kind), "itemId": e.ItemID,
			"retryAt": e.RetryAt.UTC().Format(time.RFC3339Nano), "attempts": e.Attempts},
	})
}

func save(path string, e Effect) error {
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
