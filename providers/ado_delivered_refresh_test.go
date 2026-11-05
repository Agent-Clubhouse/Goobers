package providers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type deliveredRefusal struct{}

func (deliveredRefusal) Error() string       { return "broker refused" }
func (deliveredRefusal) Deterministic() bool { return true }

func deliveredSource(t *testing.T, now *time.Time, initial ADOCredential, resolve func(context.Context) (ADOCredential, error), registrar SecretRegistrar) *adoRefreshingCredentialSource {
	t.Helper()
	source, err := NewADORefreshingCredentialSource("provider:pr:write", initial, resolve, registrar)
	if err != nil {
		t.Fatal(err)
	}
	s := source.(*adoRefreshingCredentialSource)
	s.now = func() time.Time { return *now }
	return s
}

func TestADODeliveredRefreshRefusesInvalidSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name string
		next ADOCredential
		err  error
	}{
		{name: "scheme changed", next: ADOCredential{Kind: ADOCredentialKindPAT, Secret: "new"}},
		{name: "empty value", next: ADOCredential{Kind: ADOCredentialKindBearer}},
		{name: "missing scheme", next: ADOCredential{Secret: "new"}},
		{name: "expired response", next: ADOCredential{Kind: ADOCredentialKindBearer, Secret: "new", ExpiresAt: time.Unix(100, 0)}},
		{name: "broker refusal", err: deliveredRefusal{}},
		{name: "malformed delivery", err: ErrInvalidADOCredentialDelivery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			calls := 0
			s := deliveredSource(t, &now, ADOCredential{Kind: ADOCredentialKindBearer, Secret: "still-valid", ExpiresAt: now.Add(time.Minute)}, func(context.Context) (ADOCredential, error) { calls++; return tc.next, tc.err }, nil)
			for range 2 {
				got, err := s.Credential(context.Background())
				if !errors.Is(err, ErrADODeliveredCredentialRejected) || got.Secret != "" {
					t.Fatalf("got %+v, %v", got, err)
				}
			}
			if calls != 1 {
				t.Fatalf("permanent refusal retried %d times", calls)
			}
		})
	}
}

func TestADODeliveredRefreshKeepsOnlyUnexpiredCache(t *testing.T) {
	now := time.Unix(1000, 0)
	calls := 0
	s := deliveredSource(t, &now, ADOCredential{Kind: ADOCredentialKindBearer, Secret: "cached", ExpiresAt: now.Add(30 * time.Second)}, func(context.Context) (ADOCredential, error) {
		calls++
		return ADOCredential{}, errors.New("broker transport unavailable")
	}, nil)
	for range 2 {
		if got, err := s.Credential(context.Background()); err != nil || got.Secret != "cached" {
			t.Fatalf("valid cache: %+v %v", got, err)
		}
	}
	if calls != 1 {
		t.Fatalf("proactive calls=%d", calls)
	}
	now = now.Add(30 * time.Second)
	if got, err := s.Credential(context.Background()); err == nil || got.Secret != "" {
		t.Fatalf("expired cache served: %+v %v", got, err)
	}
	if calls != 2 {
		t.Fatalf("expiry must bypass proactive throttle, calls=%d", calls)
	}
}

func TestADODeliveredRefreshExpiresWhileResolverIsRunning(t *testing.T) {
	now := time.Unix(1000, 0)
	s := deliveredSource(t, &now, ADOCredential{Kind: ADOCredentialKindBearer, Secret: "cached", ExpiresAt: now.Add(time.Second)}, func(context.Context) (ADOCredential, error) {
		now = now.Add(time.Second)
		return ADOCredential{}, errors.New("timeout")
	}, nil)
	if got, err := s.Credential(context.Background()); err == nil || got.Secret != "" {
		t.Fatalf("expired during refresh: %+v %v", got, err)
	}
}

func TestADODeliveredRefreshRegistersAtomicRotation(t *testing.T) {
	for _, kind := range []string{ADOCredentialKindPAT, ADOCredentialKindBearer} {
		t.Run(kind, func(t *testing.T) {
			now := time.Unix(1000, 0)
			next := ADOCredential{Kind: kind, Secret: "fresh-secret", ExpiresAt: now.Add(time.Hour)}
			reg := &spyGitRegistrar{}
			calls := 0
			s := deliveredSource(t, &now, ADOCredential{Kind: kind, Secret: "old-secret"}, func(context.Context) (ADOCredential, error) { calls++; return next, nil }, reg)
			for range 2 {
				if _, err := s.Credential(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 0 {
				t.Fatal("expiry-free credential refreshed proactively")
			}
			s.Invalidate()
			var wg sync.WaitGroup
			for range 20 {
				wg.Go(func() {
					got, err := s.Credential(context.Background())
					if err != nil || got != next {
						t.Errorf("incoherent snapshot: %+v %v", got, err)
					}
				})
			}
			wg.Wait()
			if calls != 1 {
				t.Fatalf("rotation calls=%d", calls)
			}
			for _, form := range next.ScrubForms() {
				if !reg.saw(form) {
					t.Errorf("unregistered form %q", form)
				}
			}
		})
	}
}

func TestADODeliveredRefreshInvalidationAndCancellation(t *testing.T) {
	now := time.Unix(1000, 0)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	s := deliveredSource(t, &now, ADOCredential{Kind: ADOCredentialKindPAT, Secret: "cached"}, func(context.Context) (ADOCredential, error) {
		calls++
		cancel()
		return ADOCredential{Kind: ADOCredentialKindPAT, Secret: "fresh"}, nil
	}, nil)
	s.Invalidate()
	if got, err := s.Credential(ctx); !errors.Is(err, context.Canceled) || got.Secret != "" {
		t.Fatalf("canceled: %+v %v", got, err)
	}
	if _, err := s.Credential(ctx); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("canceled retry: %v calls=%d", err, calls)
	}
	s.resolve = func(context.Context) (ADOCredential, error) { return ADOCredential{}, fmt.Errorf("broker unavailable") }
	if got, err := s.Credential(context.Background()); err == nil || got.Secret != "" {
		t.Fatalf("rejected cache: %+v %v", got, err)
	}
}
