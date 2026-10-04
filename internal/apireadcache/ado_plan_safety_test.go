package apireadcache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

type adoPlanRateObserver struct{ events []providers.RateLimitEvent }

func (o *adoPlanRateObserver) ObserveRateLimit(_ context.Context, event providers.RateLimitEvent) {
	o.events = append(o.events, event)
}

func TestADOPlanFollowerCancellationAndLeaderLeaseRevocation(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "team", Binding: "interactive:planning", Generation: "generation-a"}
	entered := make(chan struct{})
	stopped := make(chan struct{})
	var transportCalls atomic.Int32
	fixture := &adoPlanFixture{before: func(req *http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/wiql") && transportCalls.Add(1) == 1 {
			close(entered)
			<-req.Context().Done()
			close(stopped)
			return req.Context().Err()
		}
		return nil
	}}
	read := func(ctx context.Context) <-chan error {
		result := make(chan error, 1)
		go func() {
			_, _, err := adoPlanRead(ctx, adoPlanProvider(dir, "evaluation", scope, "token", fixture))
			result <- err
		}()
		return result
	}
	leaderContext, revoke := context.WithCancel(t.Context())
	defer revoke()
	leader := read(leaderContext)
	<-entered
	followerContext, cancel := context.WithCancel(t.Context())
	follower := read(followerContext)
	if err := waitADOPlanCondition(t.Context(), func() bool { return len(adoReadWaiters) == 2 }); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-follower:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("cancelled follower retained the policy callback")
	}
	select {
	case <-stopped:
		t.Fatal("follower cancelled the owning transport")
	default:
	}
	liveFollower := read(t.Context())
	if err := waitADOPlanCondition(t.Context(), func() bool { return len(adoReadWaiters) == 2 }); err != nil {
		t.Fatal(err)
	}
	revoke()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("leader returned before its transport stopped")
	}
	if err := <-liveFollower; err != nil {
		t.Fatal(err)
	}
	if queries, batches := fixture.counts(); queries != 2 || batches != 1 {
		t.Fatalf("cancelled result reused or follower dropped: %d %d", queries, batches)
	}
}

func TestADOPlanFailuresAndExplicitFreshRequestsAreNeverReplayed(t *testing.T) {
	for _, failure := range []struct {
		name   string
		status int
		body   string
	}{{"rate-limit", 429, `{"message":"slow down"}`}, {"unauthorized", 403, `{"message":"denied"}`}, {"malformed", 200, `not json`}} {
		t.Run(failure.name, func(t *testing.T) {
			dir := t.TempDir()
			scope := Scope{Gaggle: "team", Binding: "automation:backlog", Generation: "generation-a"}
			fixture := &adoPlanFixture{}
			var queries atomic.Int32
			client := scopedTransport(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/wiql") && queries.Add(1) <= 2 {
					return &http.Response{StatusCode: failure.status, Header: http.Header{"Retry-After": {"120"}}, Body: io.NopCloser(strings.NewReader(failure.body))}, nil
				}
				return fixture.Do(req)
			})
			p := adoPlanProvider(dir, "evaluation", scope, "token", client)
			providers.WithADOMaxRateLimitRetries(0)(p)
			observer := &adoPlanRateObserver{}
			providers.WithADORateLimitObserver(observer)(p)
			for range 2 {
				if _, _, err := adoPlanRead(t.Context(), p); err == nil {
					t.Fatal("failed response accepted")
				}
			}
			if queries.Load() != 2 {
				t.Fatalf("failure replayed: %d live reads", queries.Load())
			}
			if failure.status == 429 && (len(observer.events) != 2 || observer.events[0].RetryAfter != 120*time.Second || observer.events[0].Outcome != providers.RateLimitOutcomeExhausted) {
				t.Fatalf("provider Retry-After feedback changed: %+v", observer.events)
			}
			items, info, err := adoPlanRead(t.Context(), p)
			assertADOPlanItems(t, items, info, err)
		})
	}
	t.Run("explicit-fresh", func(t *testing.T) {
		fixture := &adoPlanFixture{}
		p := adoPlanProvider(t.TempDir(), "evaluation", Scope{Gaggle: "team", Binding: "backlog", Generation: "a"}, "token", fixture)
		inner := p.Client
		p.Client = scopedTransport(func(req *http.Request) (*http.Response, error) {
			req.Header.Set("Cache-Control", "no-cache")
			return inner.Do(req)
		})
		for range 2 {
			items, info, err := adoPlanRead(t.Context(), p)
			assertADOPlanItems(t, items, info, err)
		}
		if queries, batches := fixture.counts(); queries != 2 || batches != 2 {
			t.Fatalf("fresh read cached: %d %d", queries, batches)
		}
	})
}

func TestADOPlanArbitraryPOSTAndMissingScopeBypass(t *testing.T) {
	for _, path := range []string{"wiql", "workitemsbatch", "workitems/$Task"} {
		t.Run(path, func(t *testing.T) {
			calls := 0
			client := ScopedClient(t.TempDir(), "evaluation", Scope{Gaggle: "team", Binding: "backlog", Generation: "a"}, providers.ProviderADO, scopedTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"value":[]}`))}, nil
			}))
			for range 2 {
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://dev.azure.com/org/project/_apis/wit/"+path, strings.NewReader(`{"query":"SELECT [System.Id] FROM WorkItems", "cacheable":true}`))
				req.Header.Set("Authorization", "Bearer token")
				req.Header.Set("X-Goobers-Read-Only", "true")
				response, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
			}
			if calls != 2 {
				t.Fatalf("untrusted POST cached: %d", calls)
			}
		})
	}
	fixture := &adoPlanFixture{}
	for range 2 {
		items, info, err := adoPlanRead(t.Context(), adoPlanProvider(t.TempDir(), "evaluation", Scope{}, "token", fixture))
		assertADOPlanItems(t, items, info, err)
	}
	if queries, batches := fixture.counts(); queries != 2 || batches != 2 {
		t.Fatalf("missing scope shared: %d %d", queries, batches)
	}
}
