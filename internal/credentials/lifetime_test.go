package credentials

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWithMinimumLifetimeRefreshesAShortLivedValue pins #5905: a value whose
// stated expiry is closer than MinDeliveredLifetime is replaced by a refreshed
// one before it is handed out, and a value with enough life, or no stated
// expiry, is returned without a refresh.
func TestWithMinimumLifetimeRefreshesAShortLivedValue(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	for _, tc := range []struct {
		name        string
		heldExpiry  time.Time
		freshExpiry time.Time
		freshErr    error
		wantValue   string
		wantExpiry  time.Time
		wantRefresh int
		wantErr     bool
	}{
		{name: "enough life left", heldExpiry: now.Add(MinDeliveredLifetime), wantValue: "held", wantExpiry: now.Add(MinDeliveredLifetime)},
		{name: "no stated expiry", wantValue: "held"},
		{name: "short-lived value is refreshed", heldExpiry: now.Add(6 * time.Minute), freshExpiry: now.Add(time.Hour), wantValue: "fresh", wantExpiry: now.Add(time.Hour), wantRefresh: 1},
		{name: "refresh returns the same token", heldExpiry: now.Add(6 * time.Minute), freshExpiry: now.Add(6 * time.Minute), wantValue: "fresh", wantExpiry: now.Add(6 * time.Minute), wantRefresh: 1},
		{name: "refresh returns an older token", heldExpiry: now.Add(6 * time.Minute), freshExpiry: now.Add(time.Minute), wantValue: "held", wantExpiry: now.Add(6 * time.Minute), wantRefresh: 1},
		{name: "refresh failure keeps a still-valid value", heldExpiry: now.Add(6 * time.Minute), freshErr: errors.New("mint failed"), wantValue: "held", wantExpiry: now.Add(6 * time.Minute), wantRefresh: 1},
		{name: "refresh failure with an expired value fails", heldExpiry: now.Add(-time.Minute), freshErr: errors.New("mint failed"), wantRefresh: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refreshes := 0
			held := ExpiringResolveFunc(func(context.Context) (string, time.Time, error) {
				return "held", tc.heldExpiry, nil
			})
			refresh := ExpiringResolveFunc(func(context.Context) (string, time.Time, error) {
				refreshes++
				if tc.freshErr != nil {
					return "", time.Time{}, tc.freshErr
				}
				return "fresh", tc.freshExpiry, nil
			})
			value, expiresAt, err := held.WithLifetimeFloor(LifetimeFloor{Refresh: refresh, Now: clock})(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if value != tc.wantValue || !expiresAt.Equal(tc.wantExpiry) {
				t.Fatalf("got (%q, %v), want (%q, %v)", value, expiresAt, tc.wantValue, tc.wantExpiry)
			}
			if refreshes != tc.wantRefresh {
				t.Fatalf("refreshes = %d, want %d", refreshes, tc.wantRefresh)
			}
		})
	}
}

func TestWithMinimumLifetimePassesSourceErrorsThrough(t *testing.T) {
	want := errors.New("source failed")
	f := ExpiringResolveFunc(func(context.Context) (string, time.Time, error) { return "", time.Time{}, want })
	refresh := ExpiringResolveFunc(func(context.Context) (string, time.Time, error) {
		t.Fatal("refresh called after a source error")
		return "", time.Time{}, nil
	})
	if _, _, err := f.WithMinimumLifetime(refresh)(context.Background()); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// TestWithLifetimeFloorDoesNotRepeatARefreshThatCannotLift pins that a
// refresh which succeeds but leaves the value below the floor (an upstream
// cache returning the same token) is not paid again on every resolve for that
// value, and that it is reported once, without the value. A new held value
// (the source's own cache moved on) is refreshed again.
func TestWithLifetimeFloorDoesNotRepeatARefreshThatCannotLift(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	heldExpiry := now.Add(6 * time.Minute)
	refreshes := 0
	var reports []error
	held := ExpiringResolveFunc(func(context.Context) (string, time.Time, error) {
		return "held", heldExpiry, nil
	})
	resolve := held.WithLifetimeFloor(LifetimeFloor{
		Refresh: func(context.Context) (string, time.Time, error) {
			refreshes++
			return "held", heldExpiry, nil
		},
		Now:             func() time.Time { return now },
		OnShortDelivery: func(_ time.Time, err error) { reports = append(reports, err) },
	})
	for i := 0; i < 5; i++ {
		value, expiresAt, err := resolve(context.Background())
		if err != nil || value != "held" || !expiresAt.Equal(heldExpiry) {
			t.Fatalf("resolve %d = (%q, %v, %v)", i, value, expiresAt, err)
		}
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1 for one held value", refreshes)
	}
	if len(reports) != 1 || reports[0] != nil {
		t.Fatalf("reports = %v, want one report with no error", reports)
	}
	heldExpiry = now.Add(7 * time.Minute)
	if _, _, err := resolve(context.Background()); err != nil {
		t.Fatalf("resolve after the held value moved on: %v", err)
	}
	if refreshes != 2 {
		t.Fatalf("refreshes = %d, want a new held value to be refreshed", refreshes)
	}
}

// TestWithLifetimeFloorReportsAFailedRefresh pins that a still-valid value
// delivered because its refresh failed is reported with the refresh error,
// and that the refresh is retried on the next resolve (an outage may end).
func TestWithLifetimeFloorReportsAFailedRefresh(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	outage := errors.New("identity endpoint unavailable")
	refreshes := 0
	var reports []error
	held := ExpiringResolveFunc(func(context.Context) (string, time.Time, error) {
		return "held", now.Add(10 * time.Minute), nil
	})
	resolve := held.WithLifetimeFloor(LifetimeFloor{
		Refresh: func(context.Context) (string, time.Time, error) {
			refreshes++
			return "", time.Time{}, outage
		},
		Now:             func() time.Time { return now },
		OnShortDelivery: func(_ time.Time, err error) { reports = append(reports, err) },
	})
	for i := 0; i < 2; i++ {
		if value, _, err := resolve(context.Background()); err != nil || value != "held" {
			t.Fatalf("resolve %d = (%q, %v), want the held value", i, value, err)
		}
	}
	if refreshes != 2 {
		t.Fatalf("refreshes = %d, want a failed refresh retried", refreshes)
	}
	if len(reports) != 2 || !errors.Is(reports[0], outage) || !errors.Is(reports[1], outage) {
		t.Fatalf("reports = %v, want the refresh error each time", reports)
	}
}
