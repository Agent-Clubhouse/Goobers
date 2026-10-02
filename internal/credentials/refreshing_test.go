package credentials

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type scriptedRefresh struct {
	mu     sync.Mutex
	calls  int
	values []string
	expiry []time.Time
	err    error
}

func (s *scriptedRefresh) refresh(context.Context) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return "", time.Time{}, s.err
	}
	i := min(s.calls-1, len(s.values)-1)
	return s.values[i], s.expiry[i], nil
}

type refreshSpyRegistrar struct{ values []string }

func (s *refreshSpyRegistrar) Register(secret []byte) { s.values = append(s.values, string(secret)) }

func newScriptedToken(t *testing.T, now *time.Time, deliveredExpiry time.Time, script *scriptedRefresh, registrar SecretRegistrar) *RefreshingToken {
	t.Helper()
	token, err := NewRefreshingToken("repo:push", "delivered", deliveredExpiry, script.refresh, registrar)
	if err != nil {
		t.Fatal(err)
	}
	return token.WithClock(func() time.Time { return *now })
}

// TestRefreshingTokenRefreshesProactivelyNearExpiry: outside the window the
// delivered value is served with no daemon call; inside it the value is
// re-resolved before it is used, and the fresh value is registered.
func TestRefreshingTokenRefreshesProactivelyNearExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	script := &scriptedRefresh{values: []string{"fresh"}, expiry: []time.Time{now.Add(time.Hour)}}
	registrar := &refreshSpyRegistrar{}
	token := newScriptedToken(t, &now, now.Add(20*time.Minute), script, registrar)

	if got, err := token.Token(context.Background()); err != nil || got != "delivered" {
		t.Fatalf("far from expiry: %q, %v", got, err)
	}
	if script.calls != 0 {
		t.Fatalf("refreshed %d times with 20 minutes left", script.calls)
	}
	now = now.Add(16 * time.Minute) // four minutes left: inside RefreshWindow
	if got, err := token.Token(context.Background()); err != nil || got != "fresh" {
		t.Fatalf("near expiry: %q, %v; want the re-resolved value", got, err)
	}
	if script.calls != 1 || token.Refreshes() != 1 || !token.Expiry().Equal(now.Add(44*time.Minute)) {
		t.Fatalf("calls=%d refreshes=%d expiry=%s", script.calls, token.Refreshes(), token.Expiry())
	}
	if len(registrar.values) != 1 || registrar.values[0] != "fresh" {
		t.Fatalf("registered %v, want the fresh value", registrar.values)
	}
}

// TestRefreshingTokenKeepsTheValueWhenAProactiveRefreshFails, and does not
// hammer the daemon: the next proactive attempt waits proactiveRetryInterval.
func TestRefreshingTokenKeepsTheValueWhenAProactiveRefreshFails(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	script := &scriptedRefresh{err: errors.New("plane down")}
	token := newScriptedToken(t, &now, now.Add(4*time.Minute), script, nil)
	for range 3 {
		if got, err := token.Token(context.Background()); err != nil || got != "delivered" {
			t.Fatalf("proactive failure: %q, %v; want the still-valid delivered value", got, err)
		}
	}
	if script.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1 within the retry interval", script.calls)
	}
	now = now.Add(proactiveRetryInterval)
	_, _ = token.Token(context.Background())
	if script.calls != 2 {
		t.Fatalf("refresh calls = %d after the retry interval, want 2", script.calls)
	}
}

// TestRefreshingTokenReResolvesAfterInvalidate is the 401 path: a rejected
// value is re-resolved on the next use even far from its stated expiry, and a
// re-resolve that fails is an error rather than the known-bad value.
func TestRefreshingTokenReResolvesAfterInvalidate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	script := &scriptedRefresh{values: []string{"fresh"}, expiry: []time.Time{now.Add(time.Hour)}}
	token := newScriptedToken(t, &now, now.Add(time.Hour), script, nil)
	token.Invalidate()
	if got, err := token.Token(context.Background()); err != nil || got != "fresh" {
		t.Fatalf("after invalidate: %q, %v", got, err)
	}
	failing := &scriptedRefresh{err: errors.New("grant expired")}
	rejected := newScriptedToken(t, &now, now.Add(time.Hour), failing, nil)
	rejected.Invalidate()
	if got, err := rejected.Token(context.Background()); err == nil {
		t.Fatalf("a failed re-resolve after a 401 returned %q, want an error", got)
	}
}

func TestRefreshingTokenRequiresAnExpiry(t *testing.T) {
	if _, err := NewRefreshingToken("repo:push", "pat", time.Time{}, (&scriptedRefresh{}).refresh, nil); err == nil {
		t.Fatal("a value without a stated expiry (a PAT) became refreshable")
	}
}
