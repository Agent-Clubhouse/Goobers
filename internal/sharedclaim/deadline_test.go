package sharedclaim

import (
	"context"
	"testing"
	"time"
)

func TestAdmissionDeadlineIncludesProviderRoundTrip(t *testing.T) {
	start := time.Now()
	for _, delay := range []time.Duration{0, 20 * time.Second, 59 * time.Second, 2 * time.Minute} {
		t.Run(delay.String(), func(t *testing.T) {
			// Provider time intentionally differs from the local clock.
			s := &memoryStore{observation: Observation{Now: start.Add(7 * time.Hour)}}
			calls := 0
			now := func() time.Time {
				calls++
				if calls == 1 {
					return start
				}
				return start.Add(delay)
			}
			deadline, err := acquireUntil(t.Context(), s, "issue:42", Owner{"instance", "run", "token"}, time.Minute, now)
			if delay >= 59*time.Second {
				if err == nil || !deadline.IsZero() {
					t.Fatalf("late acknowledgment admitted execution: %v, %v", deadline, err)
				}
				return
			}
			if err != nil || !deadline.Equal(start.Add(59*time.Second)) {
				t.Fatalf("deadline reset after round trip: %v, %v", deadline, err)
			}
		})
	}
}

func TestAdmissionDeadlineRejectsInsufficientTTL(t *testing.T) {
	s := &memoryStore{observation: Observation{Now: time.Now()}}
	for _, ttl := range []time.Duration{-time.Second, 0, time.Second} {
		if _, err := AcquireUntil(t.Context(), s, "issue:42", Owner{"instance", "run", "token"}, ttl); err == nil {
			t.Fatalf("accepted insufficient TTL %v", ttl)
		}
	}
	if s.writes != 0 {
		t.Fatal("invalid TTL wrote a remote lease")
	}
}

func TestAdmissionDeadlineRejectsCanceledAcknowledgment(t *testing.T) {
	s := &memoryStore{observation: Observation{Now: time.Now()}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	deadline, err := AcquireUntil(ctx, s, "issue:42", Owner{"instance", "run", "token"}, time.Minute)
	if err == nil || !deadline.IsZero() {
		t.Fatalf("canceled acquisition admitted execution: %v, %v", deadline, err)
	}
}
