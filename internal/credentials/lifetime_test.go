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
			value, expiresAt, err := withMinimumLifetime(held, refresh, clock)(context.Background())
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
