package lease

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestTransientRenewFailureRetriesBeforeSelfFence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		m := &Manager{config: Config{RenewInterval: 5 * time.Second, SelfFenceAfter: 10 * time.Second, Now: time.Now, After: time.After}}
		h := &Handle{lastRenew: start}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		attempts := 0
		err := m.maintainRenewals(ctx, cancel, h, func(_ context.Context, handle *Handle) error {
			attempts++
			if attempts == 1 {
				return errors.New("transient connection failure")
			}
			handle.mu.Lock()
			handle.lastRenew = time.Now()
			handle.mu.Unlock()
			cancel()
			return nil
		})
		if err != nil || attempts != 2 || m.SelfFenced(h) {
			t.Fatalf("err=%v attempts=%d fenced=%v", err, attempts, m.SelfFenced(h))
		}
		if elapsed := time.Since(start); elapsed != 6*time.Second {
			t.Fatalf("virtual retry time=%v, want 6s", elapsed)
		}
	})
}

func TestRenewFailureStillFencesAtOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		m := &Manager{config: Config{RenewInterval: 5 * time.Second, SelfFenceAfter: 10 * time.Second, Now: time.Now, After: time.After}}
		h := &Handle{lastRenew: start}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		attempts := 0
		err := m.maintainRenewals(ctx, cancel, h, func(context.Context, *Handle) error { attempts++; return errors.New("unavailable") })
		if !errors.Is(err, ErrSelfFenced) || ctx.Err() == nil || attempts < 2 {
			t.Fatalf("err=%v ctx=%v attempts=%d", err, ctx.Err(), attempts)
		}
		if elapsed := time.Since(start); elapsed != 10*time.Second {
			t.Fatalf("virtual self-fence time=%v, want 10s", elapsed)
		}
	})
}

func TestStaleRenewalDoesNotRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &Manager{config: Config{RenewInterval: 5 * time.Second, SelfFenceAfter: 10 * time.Second, Now: time.Now, After: time.After}}
		h := &Handle{lastRenew: time.Now()}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		attempts := 0
		err := m.maintainRenewals(ctx, cancel, h, func(context.Context, *Handle) error { attempts++; return ErrStale })
		if !errors.Is(err, ErrStale) || attempts != 1 || ctx.Err() == nil {
			t.Fatalf("err=%v attempts=%d ctx=%v", err, attempts, ctx.Err())
		}
	})
}
