package providers

import (
	"context"
	"testing"
)

// countingTokenSource is a non-refreshable TokenSource.
type countingTokenSource struct{ calls int }

func (s *countingTokenSource) Token(context.Context) (string, error) {
	s.calls++
	return "static", nil
}

// TestRefreshCIPollCredentialInvalidatesARefreshableSource: ci-poll's 401
// retry (#6154) invalidates a refreshable source (Goobers#6120) so the next
// poll carries a re-resolved value, instead of relying on per-request
// re-resolution; a plain source is asked again, and a static token reports
// that no refresh path exists.
func TestRefreshCIPollCredentialInvalidatesARefreshableSource(t *testing.T) {
	refreshable := &fakeRefreshableToken{current: "expired-token", next: "fresh-token"}
	refreshed, err := NewGitHubProvider("expired-token", WithTokenSource(refreshable)).RefreshCIPollCredential(context.Background())
	if err != nil || !refreshed {
		t.Fatalf("refreshable RefreshCIPollCredential = %v, %v; want true, nil", refreshed, err)
	}
	if refreshable.invalidated != 1 || refreshable.current != "fresh-token" {
		t.Fatalf("invalidated=%d current=%q; want one invalidation to the fresh value", refreshable.invalidated, refreshable.current)
	}

	plain := &countingTokenSource{}
	refreshed, err = NewGitHubProvider("", WithTokenSource(plain)).RefreshCIPollCredential(context.Background())
	if err != nil || !refreshed || plain.calls != 1 {
		t.Fatalf("plain source = %v, %v, calls=%d; want true, nil, 1", refreshed, err, plain.calls)
	}

	refreshed, err = NewGitHubProvider("pat").RefreshCIPollCredential(context.Background())
	if err != nil || refreshed {
		t.Fatalf("static token = %v, %v; want false, nil", refreshed, err)
	}
}
