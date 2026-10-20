package retryutil

import (
	"testing"
	"time"
)

func TestClientPolicyBackoff(t *testing.T) {
	tests := []struct {
		name     string
		policy   ClientPolicy
		wantBase time.Duration
		wantMax  time.Duration
	}{
		{name: "defaults", wantBase: 500 * time.Millisecond, wantMax: 30 * time.Second},
		{name: "negative base", policy: ClientPolicy{BaseDelay: -time.Second}, wantBase: 500 * time.Millisecond, wantMax: 30 * time.Second},
		{name: "negative max", policy: ClientPolicy{MaxDelay: -time.Second}, wantBase: 500 * time.Millisecond, wantMax: 30 * time.Second},
		{name: "negative delays", policy: ClientPolicy{BaseDelay: -time.Second, MaxDelay: -time.Second}, wantBase: 500 * time.Millisecond, wantMax: 30 * time.Second},
		{name: "custom base", policy: ClientPolicy{BaseDelay: time.Second}, wantBase: time.Second, wantMax: 30 * time.Second},
		{name: "custom max", policy: ClientPolicy{MaxDelay: 5 * time.Second}, wantBase: 500 * time.Millisecond, wantMax: 5 * time.Second},
		{name: "custom delays", policy: ClientPolicy{BaseDelay: time.Second, MaxDelay: 5 * time.Second}, wantBase: time.Second, wantMax: 5 * time.Second},
		{name: "max below base", policy: ClientPolicy{BaseDelay: 2 * time.Second, MaxDelay: time.Second}, wantBase: 2 * time.Second, wantMax: 2 * time.Second},
		{name: "max below default base", policy: ClientPolicy{MaxDelay: time.Millisecond}, wantBase: 500 * time.Millisecond, wantMax: 500 * time.Millisecond},
		{name: "base above default max", policy: ClientPolicy{BaseDelay: time.Minute}, wantBase: time.Minute, wantMax: time.Minute},
		{name: "equal delays", policy: ClientPolicy{BaseDelay: time.Second, MaxDelay: time.Second}, wantBase: time.Second, wantMax: time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.policy.Backoff()
			if got.Base != tt.wantBase || got.Max != tt.wantMax {
				t.Fatalf("Backoff() = (%s, %s), want (%s, %s)", got.Base, got.Max, tt.wantBase, tt.wantMax)
			}
		})
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := map[int]bool{
		200: false,
		400: false,
		401: false,
		404: false,
		408: false,
		409: false,
		429: false,
		499: false,
		500: true,
		502: true,
		503: true,
		599: true,
		600: true,
	}
	for status, want := range cases {
		if got := RetryableStatus(status); got != want {
			t.Errorf("RetryableStatus(%d) = %v, want %v", status, got, want)
		}
	}
}
