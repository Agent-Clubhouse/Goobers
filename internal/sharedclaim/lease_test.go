package sharedclaim

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu          sync.Mutex
	observation Observation
	writes      int
	read        chan struct{}
	proceed     chan struct{}
}

func (s *memoryStore) Read(context.Context, string) (Observation, error) {
	s.mu.Lock()
	got := s.observation
	s.mu.Unlock()
	if s.read != nil {
		s.read <- struct{}{}
		<-s.proceed
	}
	return got, nil
}

func (s *memoryStore) CompareAndSwap(_ context.Context, _ string, revision string, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.observation.Revision != revision {
		return ErrConflict
	}
	s.writes++
	s.observation.Record = record
	s.observation.Revision = fmt.Sprint(s.writes)
	return nil
}

func TestConcurrentAcquisitionHasOneWinner(t *testing.T) {
	s := &memoryStore{observation: Observation{Now: time.Now().UTC()}, read: make(chan struct{}, 2), proceed: make(chan struct{})}
	results := make(chan error, 2)
	for _, instance := range []string{"one", "two"} {
		go func() {
			results <- Acquire(t.Context(), s, "issue:42", Owner{instance, "run", "incarnation"}, time.Minute)
		}()
	}
	<-s.read
	<-s.read
	close(s.proceed)
	winners := 0
	for _, err := range []error{<-results, <-results} {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatalf("concurrent admission: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent admission produced %d winners", winners)
	}
}

func TestOwnerScopedRenewalExpiryAndRelease(t *testing.T) {
	now := time.Now().UTC()
	s := &memoryStore{observation: Observation{Now: now}}
	owner := Owner{"instance", "run", "first-incarnation"}
	other := Owner{"instance", "run", "second-incarnation"}
	if err := Acquire(t.Context(), s, "issue:42", owner, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := Acquire(t.Context(), s, "issue:42", other, time.Minute); !errors.Is(err, ErrHeld) {
		t.Fatalf("incarnation replaced active owner: %v", err)
	}
	if err := Acquire(t.Context(), s, "issue:42", owner, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if !s.observation.Record.ExpiresAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatal("renewal did not use provider clock")
	}
	if err := Acquire(t.Context(), s, "issue:42", owner, time.Minute); err != nil {
		t.Fatal(err)
	}
	if !s.observation.Record.ExpiresAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatal("late renewal shortened the acknowledged lease")
	}
	if err := Release(t.Context(), s, "issue:42", other); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("foreign release: %v", err)
	}
	s.observation.Now = now.Add(3 * time.Minute)
	if err := Acquire(t.Context(), s, "issue:42", other, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := Release(t.Context(), s, "issue:42", owner); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale owner released successor: %v", err)
	}
	if err := Release(t.Context(), s, "issue:42", other); err != nil {
		t.Fatal(err)
	}
	if s.observation.Record != (Record{Version: 1}) || s.observation.Revision == "" {
		t.Fatal("release lost its revisioned tombstone")
	}
}

func TestMalformedObservationCannotAdmitOrRelease(t *testing.T) {
	for _, observation := range []Observation{
		{},
		{Now: time.Now(), Record: Record{Version: 1}},
		{Now: time.Now(), Revision: "v1", Record: Record{Version: 9}},
		{Now: time.Now(), Revision: "v1", Record: Record{Version: 1, Owner: Owner{Run: "partial"}}},
	} {
		s := &memoryStore{observation: observation}
		owner := Owner{"instance", "run", "token"}
		if err := Acquire(t.Context(), s, "issue:42", owner, time.Minute); err == nil {
			t.Fatal("malformed record admitted work")
		}
		if err := Release(t.Context(), s, "issue:42", owner); err == nil {
			t.Fatal("malformed record reported released")
		}
		if s.writes != 0 {
			t.Fatal("malformed record was overwritten")
		}
	}
}
