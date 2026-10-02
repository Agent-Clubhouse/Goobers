package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRefreshableToken is a delivered value that re-resolves to next after
// Invalidate (credentials.RefreshingToken's contract, minus the clock).
type fakeRefreshableToken struct {
	mu          sync.Mutex
	current     string
	next        string
	expiresAt   time.Time
	invalidated int
}

func (f *fakeRefreshableToken) Token(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, nil
}

func (f *fakeRefreshableToken) Expiry() time.Time { return f.expiresAt }

func (f *fakeRefreshableToken) Invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated++
	f.current = f.next
}

// TestGitHubProviderReResolvesARefreshableTokenOnce401 (Goobers#6120): a 401
// answered to a refreshable token source invalidates it and resends once with
// the re-resolved value; a static token keeps failing on the first 401.
func TestGitHubProviderReResolvesARefreshableTokenOn401(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seen = append(seen, auth)
		if auth != "Bearer fresh-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
			return
		}
		_, _ = io.WriteString(w, `{"login":"goobers-bot"}`)
	}))
	defer server.Close()

	source := &fakeRefreshableToken{current: "expired-token", next: "fresh-token"}
	provider := NewGitHubProvider("expired-token", WithTokenSource(source), func(p *GitHubProvider) { p.BaseURL = server.URL })
	login, err := provider.AuthenticatedLogin(context.Background())
	if err != nil || login != "goobers-bot" {
		t.Fatalf("AuthenticatedLogin = %q, %v; want the call to complete via one re-resolve", login, err)
	}
	if source.invalidated != 1 || strings.Join(seen, ",") != "Bearer expired-token,Bearer fresh-token" {
		t.Fatalf("invalidated=%d headers=%v", source.invalidated, seen)
	}

	// Both values rejected: exactly one retry, then the 401 surfaces.
	seen = nil
	stuck := &fakeRefreshableToken{current: "expired-token", next: "also-rejected"}
	provider = NewGitHubProvider("expired-token", WithTokenSource(stuck), func(p *GitHubProvider) { p.BaseURL = server.URL })
	if _, err := provider.AuthenticatedLogin(context.Background()); err == nil || !IsAuthenticationError(err) {
		t.Fatalf("err = %v, want an authentication failure after one retry", err)
	}
	if len(seen) != 2 {
		t.Fatalf("requests = %d, want the original and exactly one retry", len(seen))
	}

	// A static token (a PAT, a stage without a grant): no retry, as before.
	seen = nil
	provider = NewGitHubProvider("expired-token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	if _, err := provider.AuthenticatedLogin(context.Background()); err == nil {
		t.Fatal("a static rejected token succeeded")
	}
	if len(seen) != 1 {
		t.Fatalf("static token requests = %d, want 1 (no refresh path)", len(seen))
	}
}

// TestADORefreshingDeliveredCredentialReResolvesOn401 is the ADO half: the
// existing send() 401 path invalidates the refreshing delivered source and
// resends with the re-resolved value, including after the sign-in redirect
// (#6111) that means the same thing.
func TestADORefreshingDeliveredCredentialReResolvesOn401(t *testing.T) {
	for _, rejection := range []string{"401", "sign-in 203"} {
		t.Run(rejection, func(t *testing.T) {
			token := &fakeRefreshableToken{current: "expired-token", next: "fresh-token", expiresAt: time.Now().Add(time.Hour)}
			source, err := NewADORefreshingDeliveredCredentialSource(ADOCredentialKindBearer, "github:pr:write", token)
			if err != nil {
				t.Fatal(err)
			}
			var headers []string
			provider := NewADOProvider("org", "project", "", WithADOCredentialSource(source), func(p *ADOProvider) {
				p.Client = adoHTTPClientFunc(func(req *http.Request) (*http.Response, error) {
					headers = append(headers, req.Header.Get("Authorization"))
					if req.Header.Get("Authorization") != "Bearer fresh-token" {
						return adoRejection(rejection), nil
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"authenticatedUser":{"providerDisplayName":"goobers-bot"}}`))}, nil
				})
			})
			if login, err := provider.AuthenticatedLogin(context.Background()); err != nil || login != "goobers-bot" {
				t.Fatalf("AuthenticatedLogin = %q, %v; want it to complete via one re-resolve", login, err)
			}
			if token.invalidated != 1 || strings.Join(headers, ",") != "Bearer expired-token,Bearer fresh-token" {
				t.Fatalf("invalidated=%d headers=%v", token.invalidated, headers)
			}
		})
	}
}

// TestADORefreshingDeliveredCredentialFailsClearlyWhenTheFreshValueIsRejected:
// one re-resolve only; a second 401 ends the request with the delivered
// rejection, saying it was re-resolved, and the sign-in page is summarized.
func TestADORefreshingDeliveredCredentialFailsClearlyWhenTheFreshValueIsRejected(t *testing.T) {
	token := &fakeRefreshableToken{current: "expired-token", next: "also-rejected", expiresAt: time.Now().Add(time.Hour)}
	source, err := NewADORefreshingDeliveredCredentialSource(ADOCredentialKindBearer, "github:pr:write", token)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	provider := NewADOProvider("org", "project", "", WithADOCredentialSource(source), func(p *ADOProvider) {
		p.Client = adoHTTPClientFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return adoRejection("sign-in 203"), nil
		})
	})
	_, err = provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project", Name: "repo"}, "42")
	if !errors.Is(err, ErrADODeliveredCredentialRejected) || !IsAuthenticationError(err) {
		t.Fatalf("err = %v, want the delivered rejection", err)
	}
	msg := err.Error()
	if calls != 2 || !strings.Contains(msg, "re-resolved once") {
		t.Fatalf("calls=%d msg=%q; want one retry and a message saying so", calls, msg)
	}
	if strings.Contains(msg, "<html") || strings.Contains(msg, "window.location") || !strings.Contains(msg, "HTML sign-in page") || !strings.Contains(msg, `"Azure DevOps Services | Sign In"`) {
		t.Fatalf("error %q embeds or omits the sign-in page summary", msg)
	}
	if strings.Contains(msg, "expired-token") || strings.Contains(msg, "also-rejected") {
		t.Fatalf("error leaks a credential: %q", msg)
	}
}

func adoRejection(kind string) *http.Response {
	if kind == "401" {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("TF400813: not authorized"))}
	}
	page := "<!DOCTYPE html><html><head><title>Azure DevOps Services | Sign In</title></head><body>" +
		strings.Repeat("<script>window.location='https://app.vssps.visualstudio.com/_signin';</script>", 40) + "</body></html>"
	header := make(http.Header)
	header.Set("Content-Type", "text/html")
	return &http.Response{StatusCode: http.StatusNonAuthoritativeInfo, Header: header, Body: io.NopCloser(strings.NewReader(page))}
}
