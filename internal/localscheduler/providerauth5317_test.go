package localscheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// providerAuthServer answers the GitHub read probe according to the bearer
// token, so classification runs over real provider errors.
func providerAuthServer(t *testing.T) (ProviderAuthVerifier, *int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("health probe issued mutating %s %s", r.Method, r.URL.Path)
		}
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "good-token":
			if strings.HasSuffix(r.URL.Path, "/issues") {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`{"permissions":{"pull":true}}`))
		case "revoked-token":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		case "narrow-token":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(server.Close)
	verify := func(ctx context.Context, token string, repo providers.RepositoryRef) error {
		return providers.NewGitHubProvider(token, func(p *providers.GitHubProvider) { p.BaseURL = server.URL }).
			VerifyRepositoryReadAccess(ctx, repo)
	}
	return verify, &calls
}

func githubTarget(owner, name, ref string) ProviderAuthTarget {
	return ProviderAuthTarget{
		Repository:    providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: owner, Name: name},
		CredentialRef: ref,
	}
}

func staticTokens(tokens map[string]string) func(context.Context, string) (string, error) {
	return func(_ context.Context, ref string) (string, error) {
		token, ok := tokens[ref]
		if !ok {
			return "", errors.New("no credential bound")
		}
		return token, nil
	}
}

func TestProviderAuthHealthClassifiesEachIdentityIndependently(t *testing.T) {
	verify, _ := providerAuthServer(t)
	var registered []string
	health := NewProviderAuthHealth("rev-1", staticTokens(map[string]string{
		"acme/web":   "good-token",
		"acme/old":   "revoked-token",
		"acme/scope": "narrow-token",
		"acme/gone":  "hidden-token",
	}), func(secret []byte) { registered = append(registered, string(secret)) }, verify)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	cases := []struct {
		target ProviderAuthTarget
		code   string
	}{
		{githubTarget("acme", "web", "acme/web"), ""},
		{githubTarget("acme", "old", "acme/old"), ProviderAuthRejected},
		{githubTarget("acme", "scope", "acme/scope"), ProviderAuthInsufficient},
		{githubTarget("acme", "gone", "acme/gone"), ProviderAuthInsufficient},
		{githubTarget("acme", "unbound", "acme/unbound"), ProviderAuthMissing},
		{ProviderAuthTarget{Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Name: "web"}, CredentialRef: "acme/web"}, ProviderAuthUnsupported},
	}
	for _, tc := range cases {
		status := health.Gate(tc.target).ProviderAuthStatus(context.Background(), now)
		if status.Healthy != (tc.code == "") || status.Code != tc.code {
			t.Errorf("%s: status = %+v, want code %q", tc.target.Repository.Name, status, tc.code)
		}
		for _, token := range []string{"good-token", "revoked-token", "narrow-token", "hidden-token"} {
			if strings.Contains(status.Detail, token) {
				t.Errorf("%s: detail exposes token: %q", tc.target.Repository.Name, status.Detail)
			}
		}
	}
	if len(registered) != 4 {
		t.Errorf("resolved tokens registered for scrubbing = %d, want 4", len(registered))
	}
}

func TestProviderAuthHealthCacheInvalidatesOnCredentialChangeAndExpiry(t *testing.T) {
	verify, calls := providerAuthServer(t)
	tokens := map[string]string{"acme/web": "good-token"}
	health := NewProviderAuthHealth("rev-1", staticTokens(tokens), nil, verify)
	gate := health.Gate(githubTarget("acme", "web", "acme/web"))
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	if status := gate.ProviderAuthStatus(context.Background(), now); !status.Healthy {
		t.Fatalf("initial status = %+v", status)
	}
	probes := *calls
	if status := gate.ProviderAuthStatus(context.Background(), now.Add(time.Minute)); !status.Healthy || *calls != probes {
		t.Fatalf("cached status = %+v after %d probes, want reuse of %d", status, *calls, probes)
	}

	tokens["acme/web"] = "revoked-token"
	if status := gate.ProviderAuthStatus(context.Background(), now.Add(2*time.Minute)); status.Code != ProviderAuthRejected {
		t.Fatalf("rotated credential reused healthy evidence: %+v", status)
	}

	tokens["acme/web"] = "good-token"
	probes = *calls
	if status := gate.ProviderAuthStatus(context.Background(), now.Add(providerAuthHealthyTTL+time.Second)); !status.Healthy || *calls == probes {
		t.Fatalf("expired evidence was not re-verified: %+v", status)
	}

	reloaded := NewProviderAuthHealth("rev-2", staticTokens(tokens), nil, verify)
	probes = *calls
	reloaded.Gate(githubTarget("acme", "web", "acme/web")).ProviderAuthStatus(context.Background(), now)
	if *calls == probes {
		t.Fatal("a new config revision reused prior evidence")
	}
}

func TestProviderAuthHealthFailsClosedWhenUnverifiable(t *testing.T) {
	health := NewProviderAuthHealth("rev-1", staticTokens(map[string]string{"acme/web": "good-token"}), nil,
		func(context.Context, string, providers.RepositoryRef) error {
			return errors.New("dial tcp: connection refused")
		})
	status := health.Gate(githubTarget("acme", "web", "acme/web")).ProviderAuthStatus(context.Background(), time.Now())
	if status.Healthy || status.Code != ProviderAuthUnverified {
		t.Fatalf("status = %+v, want fail-closed %s", status, ProviderAuthUnverified)
	}
}

type staticProviderAuthGate struct {
	mu     sync.Mutex
	status ProviderAuthStatus
	calls  int
}

func (g *staticProviderAuthGate) ProviderAuthStatus(context.Context, time.Time) ProviderAuthStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.status
}

func TestRequireProviderAuthorizationBlocksAutonomousDispatchWithoutClaim(t *testing.T) {
	unhealthy := &staticProviderAuthGate{status: ProviderAuthStatus{Code: ProviderAuthRejected, Detail: "GET /repos/acme/web failed: status 401"}}
	healthy := &staticProviderAuthGate{status: ProviderAuthStatus{Healthy: true}}
	blockedStarter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	healthyStarter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	ungatedStarter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	readiness := apiv1.ReadinessConditions{MaxConcurrentRuns: 1, RequireProviderAuthorization: true}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{Workflow: "blocked", Gaggle: "example", Readiness: readiness, Signals: []string{"go"}, Starter: blockedStarter, ProviderAuth: unhealthy},
		{Workflow: "healthy", Gaggle: "example", Readiness: readiness, Signals: []string{"go"}, Starter: healthyStarter, ProviderAuth: healthy},
		{Workflow: "ungated", Gaggle: "example", Readiness: readiness, Signals: []string{"go"}, Starter: ungatedStarter},
	})

	for _, workflow := range []string{"blocked", "ungated"} {
		_, err := sched.TriggerSignal(context.Background(), workflow, "go", "", time.Now())
		var rejected *TriggerRejectedError
		if !errors.As(err, &rejected) || !strings.HasPrefix(rejected.Reason, ReasonProviderAuthUnhealthy) {
			t.Fatalf("%s: signal dispatch err = %v, want %s refusal", workflow, err, ReasonProviderAuthUnhealthy)
		}
	}
	if blockedStarter.count() != 0 || ungatedStarter.count() != 0 {
		t.Fatalf("unhealthy provider auth started runs: blocked=%d ungated=%d", blockedStarter.count(), ungatedStarter.count())
	}
	if _, err := sched.TriggerSignal(context.Background(), "healthy", "go", "", time.Now()); err != nil {
		t.Fatalf("healthy provider auth refused: %v", err)
	}
	waitForCount(t, healthyStarter.count, 1)

	if _, err := sched.Trigger(context.Background(), "blocked", time.Now()); err != nil {
		t.Fatalf("manual diagnosis run refused: %v", err)
	}
	waitForCount(t, blockedStarter.count, 1)
	if unhealthy.calls != 1 {
		t.Fatalf("manual trigger evaluated provider auth gate: %d calls", unhealthy.calls)
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]string{}
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped && strings.HasPrefix(ev.Reason, ReasonProviderAuthUnhealthy) && ev.Error != nil {
			codes[ev.Workflow] = ev.Error.Code
		}
	}
	if codes["blocked"] != ProviderAuthRejected || codes["ungated"] != ProviderAuthUnsupported {
		t.Fatalf("tick.skipped provider-auth codes = %v", codes)
	}
}
