package providers

import (
	"context"
	"net/http"
	"testing"
	"time"
)

type providerConstructorState struct {
	client              HTTPClient
	maxRetries          int
	maxRateLimitRetries int
	maxRateLimitWait    time.Duration
	now                 func() time.Time
	sleep               func(context.Context, time.Duration) error
	jitter              func(time.Duration) time.Duration
}

func TestNewProviderConstructorDefaults(t *testing.T) {
	tests := []struct {
		name  string
		state providerConstructorState
	}{
		{
			name: "GitHub",
			state: func() providerConstructorState {
				p := NewGitHubProvider("token", WithHTTPClient(nil), func(p *GitHubProvider) {
					p.now = nil
					p.sleep = nil
					p.jitter = nil
				})
				return providerConstructorState{
					client:              p.Client,
					maxRetries:          p.maxRetries,
					maxRateLimitRetries: p.maxRateLimitRetries,
					maxRateLimitWait:    p.maxRateLimitWait,
					now:                 p.now,
					sleep:               p.sleep,
					jitter:              p.jitter,
				}
			}(),
		},
		{
			name: "Gitea",
			state: func() providerConstructorState {
				p := NewGiteaProvider("https://gitea.example.com", "token", WithGiteaHTTPClient(nil), func(p *GiteaProvider) {
					p.now = nil
					p.sleep = nil
					p.jitter = nil
				})
				return providerConstructorState{
					client:              p.Client,
					maxRetries:          p.maxRetries,
					maxRateLimitRetries: p.maxRateLimitRetries,
					maxRateLimitWait:    p.maxRateLimitWait,
					now:                 p.now,
					sleep:               p.sleep,
					jitter:              p.jitter,
				}
			}(),
		},
		{
			name: "ADO",
			state: func() providerConstructorState {
				p := NewADOProvider("org", "project", "token", func(p *ADOProvider) {
					p.Client = nil
					p.now = nil
					p.sleep = nil
					p.jitter = nil
				})
				return providerConstructorState{
					client:           p.Client,
					maxRetries:       p.maxRetries,
					maxRateLimitWait: p.maxRateLimitWait,
					now:              p.now,
					sleep:            p.sleep,
					jitter:           p.jitter,
				}
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, ok := tc.state.client.(*http.Client)
			if !ok {
				t.Fatalf("default client = %T, want *http.Client", tc.state.client)
			}
			if client.Timeout != defaultProviderHTTPTimeout {
				t.Errorf("default client timeout = %s, want %s", client.Timeout, defaultProviderHTTPTimeout)
			}
			if tc.state.maxRetries != defaultRateLimitRetries {
				t.Errorf("max retries = %d, want %d", tc.state.maxRetries, defaultRateLimitRetries)
			}
			if tc.name != "ADO" && tc.state.maxRateLimitRetries != defaultRateLimitRetries {
				t.Errorf("max rate-limit retries = %d, want %d", tc.state.maxRateLimitRetries, defaultRateLimitRetries)
			}
			if tc.state.maxRateLimitWait != defaultRateLimitMaxWait {
				t.Errorf("max rate-limit wait = %s, want %s", tc.state.maxRateLimitWait, defaultRateLimitMaxWait)
			}
			if tc.state.now == nil || tc.state.sleep == nil || tc.state.jitter == nil {
				t.Errorf("runtime defaults not recovered: now=%v sleep=%v jitter=%v",
					tc.state.now != nil, tc.state.sleep != nil, tc.state.jitter != nil)
			}
		})
	}
}

func TestNewGiteaProviderDefersMissingBaseURLError(t *testing.T) {
	p := NewGiteaProvider("", "token", func(p *GiteaProvider) {
		p.now = nil
		p.sleep = nil
		p.jitter = nil
	})

	if p == nil {
		t.Fatal("NewGiteaProvider returned nil")
	}
	if err := p.ready(); err == nil {
		t.Fatal("ready() = nil, want deferred missing base URL error")
	}
}
