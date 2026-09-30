package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/providers"
)

// credentialrefresh_test.go proves mid-stage credential refresh (Goobers#6120
// phase 1): the daemon's refresh service, its negative cases, and — end to
// end — a deterministic stage whose delivered token dies mid-stage completing
// through the re-resolve path (distributed-state-and-coordination.md
// acceptance item 8).

// mintSequence is a daemon-side expiring source that mints a new value per
// call: value-1, value-2, ... each expiring ttl after its mint.
type mintSequence struct {
	mu    sync.Mutex
	calls int
	ttl   time.Duration
}

func (m *mintSequence) next(context.Context) (string, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return fmt.Sprintf("ghs_minted%02d%s", m.calls, strings.Repeat("x", 36)), time.Now().Add(m.ttl).UTC().Truncate(time.Second), nil
}

func (m *mintSequence) value(n int) string {
	return fmt.Sprintf("ghs_minted%02d%s", n, strings.Repeat("x", 36))
}

// newRefreshFixture is the credential-plane fixture with a minting source and
// grants enabled at endpoint.
func newRefreshFixture(t *testing.T, endpoint string) (*daemonCredentialService, *mintSequence, string) {
	t.Helper()
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, _, runID := newCredentialPlaneFixture(t, machine)
	minted := &mintSequence{ttl: time.Hour}
	service.buildSources = func(credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
		resolver, err := credentials.NewResolverWithExpiring(nil, nil,
			map[string]credentials.ResolveFunc{"issues-ref": func(context.Context) (string, error) { return "static-issues-token-9876543210", nil }},
			map[string]credentials.ExpiringResolveFunc{"repo-ref": minted.next},
		)
		return resolver, []credentials.Grant{{Capability: "repo:push", Ref: "repo-ref"}, {Capability: "github:issues:write", Ref: "issues-ref"}}, err
	}
	issuer, err := newStageGrantIssuer(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	service.grants = issuer
	return service, minted, runID
}

func pushBranchEnvelope(runID string, attempt int32) apiv1.InvocationEnvelope {
	return apiv1.InvocationEnvelope{RunID: runID, TaskID: "push-branch", Attempt: attempt}
}

func mintTestGrant(t *testing.T, service *daemonCredentialService, grant podauth.CredentialGrant, ttl time.Duration) string {
	t.Helper()
	token, _, err := service.grants.key.MintCredentialGrant(grant, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestCredentialRefreshReResolvesTheGrantedCapabilityAndJournalsIt(t *testing.T) {
	service, minted, runID := newRefreshFixture(t, "http://127.0.0.1:1")
	grant, err := service.MintStageGrant(pushBranchEnvelope(runID, 2), []string{"repo:push"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Refresh(context.Background(), grant.Token, httpapi.CredentialRefreshRequest{Capability: "repo:push"})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(response.Credentials) != 1 || response.Credentials[0].Value != minted.value(1) || response.Credentials[0].ExpiresAt == nil {
		t.Fatalf("credentials = %+v, want one fresh expiring repo:push value", response.Credentials)
	}
	if !bytes.Contains(service.shared.Scrub([]byte("leak "+grant.Token)), []byte(journal.Redacted)) {
		t.Fatal("the grant was not registered with the shared scrubber")
	}
	events := instanceLogEvents(t, service.layout)
	found := false
	for _, event := range events {
		if event.Runner["kind"] == credentialRefreshMarker {
			found = true
			if event.Stage != "push-branch" || fmt.Sprint(event.Runner["attempt"]) != "2" {
				t.Fatalf("refresh audit = %+v, want stage push-branch attempt 2", event)
			}
			if strings.Contains(fmt.Sprint(event.Runner), minted.value(1)) {
				t.Fatal("the audit record carries the value")
			}
		}
	}
	if !found {
		t.Fatal("the re-resolve was not journaled")
	}
}

// TestCredentialRefreshNegativeCases pins what a grant can NOT do.
func TestCredentialRefreshNegativeCases(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		grant    func(t *testing.T, service *daemonCredentialService, runID string) string
		ask      string
		advance  time.Duration
		wantCode string
	}{
		{
			name: "capability outside the grant",
			grant: func(t *testing.T, service *daemonCredentialService, runID string) string {
				return mintTestGrant(t, service, podauth.CredentialGrant{RunID: runID, Stage: "push-branch", Capabilities: []string{"repo:push"}}, time.Hour)
			},
			ask: "github:issues:write", wantCode: "capability_not_granted",
		},
		{
			name: "capability the pinned stage never declared",
			grant: func(t *testing.T, service *daemonCredentialService, runID string) string {
				return mintTestGrant(t, service, podauth.CredentialGrant{RunID: runID, Stage: "push-branch", Capabilities: []string{"repo:push", "github:issues:write"}}, time.Hour)
			},
			ask: "github:issues:write", wantCode: "capability_undeclared",
		},
		{
			name: "another run's grant",
			grant: func(t *testing.T, service *daemonCredentialService, _ string) string {
				return mintTestGrant(t, service, podauth.CredentialGrant{RunID: "run-someone-else", Stage: "push-branch", Capabilities: []string{"repo:push"}}, time.Hour)
			},
			ask: "repo:push", wantCode: "run_not_found",
		},
		{
			name: "another daemon's grant",
			grant: func(t *testing.T, _ *daemonCredentialService, runID string) string {
				other, err := newStageGrantIssuer("http://127.0.0.1:2")
				if err != nil {
					t.Fatal(err)
				}
				token, _, err := other.key.MintCredentialGrant(podauth.CredentialGrant{RunID: runID, Stage: "push-branch", Capabilities: []string{"repo:push"}}, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				return token
			},
			ask: "repo:push", wantCode: "credential_grant_invalid",
		},
		{
			name: "expired grant",
			grant: func(t *testing.T, service *daemonCredentialService, runID string) string {
				return mintTestGrant(t, service, podauth.CredentialGrant{RunID: runID, Stage: "push-branch", Capabilities: []string{"repo:push"}}, time.Minute)
			},
			ask: "repo:push", advance: 2 * time.Minute, wantCode: "credential_grant_expired",
		},
		{
			name: "agentic stage",
			grant: func(t *testing.T, service *daemonCredentialService, runID string) string {
				return mintTestGrant(t, service, podauth.CredentialGrant{RunID: runID, Stage: "implement", Capabilities: []string{"repo:push"}}, time.Hour)
			},
			ask: "repo:push", wantCode: "credential_refresh_agentic_stage",
		},
		{
			name: "reviewer gate",
			grant: func(t *testing.T, service *daemonCredentialService, runID string) string {
				return mintTestGrant(t, service, podauth.CredentialGrant{RunID: runID, Stage: "review", Capabilities: []string{"github:issues:write"}}, time.Hour)
			},
			ask: "github:issues:write", wantCode: "credential_refresh_agentic_stage",
		},
		{
			name: "revoked when the attempt finished",
			grant: func(t *testing.T, service *daemonCredentialService, runID string) string {
				grant, err := service.MintStageGrant(pushBranchEnvelope(runID, 1), []string{"repo:push"}, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				grant.Revoke()
				return grant.Token
			},
			ask: "repo:push", wantCode: "credential_grant_revoked",
		},
		{
			name: "not a grant",
			grant: func(*testing.T, *daemonCredentialService, string) string {
				return "goobers-grant.bm90LWEtZ3JhbnQ.forged-mac-value"
			},
			ask: "repo:push", wantCode: "credential_grant_invalid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, minted, runID := newRefreshFixture(t, "http://127.0.0.1:1")
			clock := now
			service.grants.key.WithClock(func() time.Time { return clock })
			token := tc.grant(t, service, runID)
			clock = clock.Add(tc.advance)
			_, err := service.Refresh(context.Background(), token, httpapi.CredentialRefreshRequest{Capability: tc.ask})
			if err == nil {
				t.Fatal("Refresh succeeded")
			}
			if got := planeErrorOf(t, err).Code; got != tc.wantCode {
				t.Fatalf("code = %q (%v), want %q", got, err, tc.wantCode)
			}
			if minted.calls != 0 {
				t.Fatalf("a refused refresh minted %d values", minted.calls)
			}
		})
	}
}

func TestCredentialRefreshIsRateLimitedPerGrant(t *testing.T) {
	service, _, runID := newRefreshFixture(t, "http://127.0.0.1:1")
	grant, err := service.MintStageGrant(pushBranchEnvelope(runID, 1), []string{"repo:push"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := range grantRefreshBurst {
		if _, err := service.Refresh(context.Background(), grant.Token, httpapi.CredentialRefreshRequest{Capability: "repo:push"}); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	_, err = service.Refresh(context.Background(), grant.Token, httpapi.CredentialRefreshRequest{Capability: "repo:push"})
	if err == nil || planeErrorOf(t, err).Code != "credential_refresh_rate_limited" || planeErrorOf(t, err).Status != http.StatusTooManyRequests {
		t.Fatalf("burst+1 refresh = %v, want 429 credential_refresh_rate_limited", err)
	}
	other, err := service.MintStageGrant(pushBranchEnvelope(runID, 2), []string{"repo:push"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(context.Background(), other.Token, httpapi.CredentialRefreshRequest{Capability: "repo:push"}); err != nil {
		t.Fatalf("another grant's budget was charged: %v", err)
	}
}

// TestCredentialPlaneMintsAPodGrantOnlyForDeterministicExpiringStages: the
// pod's stage-start resolve gets a grant for a deterministic task's expiring
// declared capabilities, and none for an agentic stage or without asking.
func TestCredentialPlaneMintsAPodGrantOnlyForDeterministicExpiringStages(t *testing.T) {
	service, _, runID := newRefreshFixture(t, "http://127.0.0.1:1")
	response, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "push-branch", Capabilities: []string{"repo:push"}, Grant: true, Attempt: 4, TimeoutSeconds: 1800,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if response.Grant == nil {
		t.Fatal("deterministic pod stage got no grant")
	}
	claims, err := service.grants.key.VerifyCredentialGrant(response.Grant.Token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Stage != "push-branch" || claims.Attempt != 4 || strings.Join(claims.Capabilities, ",") != "repo:push" {
		t.Fatalf("claims = %+v", claims)
	}
	if ttl := time.Until(claims.ExpiresAt); ttl < 35*time.Minute || ttl > 41*time.Minute {
		t.Fatalf("grant ttl = %s, want the stage timeout plus margin", ttl)
	}
	for name, request := range map[string]httpapi.CredentialResolveRequest{
		"not asked":     {RunID: runID, Stage: "push-branch", Capabilities: []string{"repo:push"}},
		"agentic stage": {RunID: runID, Stage: "implement", Capabilities: []string{"repo:push"}, Grant: true},
		"no expiry":     {RunID: runID, Stage: "implement", Capabilities: []string{"github:issues:write"}, Grant: true},
	} {
		response, err := service.Resolve(context.Background(), request)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if response.Grant != nil {
			t.Fatalf("%s: got a grant", name)
		}
	}
}

// resetStageRefreshingTokens clears the process-wide refreshing-token cache
// around a test that plays a stage.
func resetStageRefreshingTokens(t *testing.T) {
	t.Helper()
	clear := func() {
		stageRefreshingTokens.Range(func(key, _ any) bool {
			stageRefreshingTokens.Delete(key)
			return true
		})
	}
	clear()
	t.Cleanup(clear)
}

// fakeExpiringForge is a GitHub API that honors only the credential plane's
// latest mint: every older value has "expired".
type fakeExpiringForge struct {
	mu      sync.Mutex
	valid   string
	headers []string
}

func (f *fakeExpiringForge) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = append(f.headers, r.Header.Get("Authorization"))
	if r.Header.Get("Authorization") != "Bearer "+f.valid {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
		return
	}
	_, _ = io.WriteString(w, `{"login":"goobers-bot"}`)
}

func (f *fakeExpiringForge) expireAllBut(value string) {
	f.mu.Lock()
	f.valid = value
	f.mu.Unlock()
}

// playStageWithGrant stands up the daemon's loopback API over service, mints
// the local grant exactly as the executor does, and sets the environment the
// executor hands a goobers-CLI stage: the delivered value, its expiry, and the
// grant.
func playStageWithGrant(t *testing.T, delivered string, expiresAt time.Time) (*daemonCredentialService, *mintSequence, *fakeExpiringForge) {
	t.Helper()
	resetStageRefreshingTokens(t)
	api := &switchingHandler{}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	service, minted, runID := newRefreshFixture(t, server.URL)
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.AllowAll, stdlog.New(io.Discard, "", 0), httpapi.WithCredentialService(service))
	if err != nil {
		t.Fatal(err)
	}
	api.set(handler)
	grant, err := service.MintStageGrant(pushBranchEnvelope(runID, 1), []string{"repo:push"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	forge := &fakeExpiringForge{valid: delivered}
	forgeServer := httptest.NewServer(http.HandlerFunc(forge.handler))
	t.Cleanup(forgeServer.Close)
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return newTelemetryGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = forgeServer.URL })...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })

	t.Setenv(executor.InstanceRootEnvVar, t.TempDir())
	t.Setenv(executor.CredentialEnvVar("repo:push"), delivered)
	t.Setenv(capability.CredentialExpiryEnvVar("repo:push"), capability.FormatCredentialExpiry(expiresAt))
	t.Setenv(executor.CredentialEndpointEnvVar, grant.Endpoint)
	t.Setenv(executor.CredentialGrantEnvVar, grant.Token)
	return service, minted, forge
}

type switchingHandler struct {
	mu      sync.Mutex
	handler http.Handler
}

func (s *switchingHandler) set(handler http.Handler) {
	s.mu.Lock()
	s.handler = handler
	s.mu.Unlock()
}

func (s *switchingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	handler := s.handler
	s.mu.Unlock()
	handler.ServeHTTP(w, r)
}

func stageGitHubProvider(t *testing.T) *providers.GitHubProvider {
	t.Helper()
	provider, err := newProviderForStageAs[*providers.GitHubProvider](os.Getenv(executor.InstanceRootEnvVar),
		providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}, true,
		withStageProviderCapability(capability.RepoPush))
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

// TestDeterministicStageCompletesAfterItsDeliveredTokenExpiresMidStage is
// acceptance item 8: a deterministic stage starts with a valid delivered
// token, the token dies partway through, and the stage completes through the
// re-resolve path — the grant, the loopback refresh route, the pinned-stage
// check, a fresh daemon-side mint — instead of failing on the 401.
func TestDeterministicStageCompletesAfterItsDeliveredTokenExpiresMidStage(t *testing.T) {
	const delivered = "ghs_delivered" + "yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"
	_, minted, forge := playStageWithGrant(t, delivered, time.Now().Add(40*time.Minute))
	provider := stageGitHubProvider(t)

	if login, err := provider.AuthenticatedLogin(context.Background()); err != nil || login != "goobers-bot" {
		t.Fatalf("first call with the delivered token = %q, %v", login, err)
	}
	// Mid-stage the delivered token dies. The next mint the daemon makes is
	// the only value the forge will accept.
	forge.expireAllBut(minted.value(1))
	if login, err := provider.AuthenticatedLogin(context.Background()); err != nil || login != "goobers-bot" {
		t.Fatalf("call after expiry = %q, %v; want the stage to complete via re-resolve", login, err)
	}
	if minted.calls != 1 {
		t.Fatalf("daemon minted %d values, want exactly one re-resolve", minted.calls)
	}
	want := []string{"Bearer " + delivered, "Bearer " + delivered, "Bearer " + minted.value(1)}
	if strings.Join(forge.headers, ",") != strings.Join(want, ",") {
		t.Fatalf("forge saw %v, want %v", forge.headers, want)
	}
}

// TestDeterministicStageRefreshesProactivelyNearExpiry: a value delivered
// inside the refresh window is replaced before it is sent, so the forge never
// sees it fail.
func TestDeterministicStageRefreshesProactivelyNearExpiry(t *testing.T) {
	const delivered = "ghs_delivered" + "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	_, minted, forge := playStageWithGrant(t, delivered, time.Now().Add(3*time.Minute))
	forge.expireAllBut(minted.value(1))
	if login, err := stageGitHubProvider(t).AuthenticatedLogin(context.Background()); err != nil || login != "goobers-bot" {
		t.Fatalf("AuthenticatedLogin = %q, %v", login, err)
	}
	if len(forge.headers) != 1 || forge.headers[0] != "Bearer "+minted.value(1) {
		t.Fatalf("forge saw %v, want only the proactively refreshed value", forge.headers)
	}
	// The git environment built for the next push reads the same refreshed
	// value, per invocation.
	header := strings.Join(gitAuthEnv(delivered), "\n")
	wantAuth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + minted.value(1)))
	if !strings.Contains(header, wantAuth) {
		t.Fatal("the git auth environment still carries the delivered value")
	}
}

// TestStageWithoutAGrantOrWithAPATBehavesAsBefore: no grant, or a delivered
// value with no stated expiry, keeps the static value — one request, the 401
// surfaces, nothing is re-resolved.
func TestStageWithoutAGrantOrWithAPATBehavesAsBefore(t *testing.T) {
	const delivered = "ghp_patpatpatpatpatpatpatpatpatpatpatpatpat"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{name: "no grant", setup: func(t *testing.T) {
			t.Setenv(executor.CredentialGrantEnvVar, "")
			t.Setenv(executor.CredentialEndpointEnvVar, "")
		}},
		{name: "pat", setup: func(t *testing.T) { t.Setenv(capability.CredentialExpiryEnvVar("repo:push"), "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, minted, forge := playStageWithGrant(t, delivered, time.Now().Add(time.Minute))
			tc.setup(t)
			forge.expireAllBut("nothing")
			if stageRefreshingToken(capability.RepoPush, delivered) != nil {
				t.Fatal("a refreshing source was built")
			}
			_, err := stageGitHubProvider(t).AuthenticatedLogin(context.Background())
			if err == nil || !providers.IsAuthenticationError(err) {
				t.Fatalf("err = %v, want the 401", err)
			}
			if minted.calls != 0 || len(forge.headers) != 1 {
				t.Fatalf("minted=%d requests=%d; want no refresh and no retry", minted.calls, len(forge.headers))
			}
			source, err := stageADOCredentialSource(capability.RepoPush, delivered)
			if err != nil {
				t.Fatal(err)
			}
			if _, refreshable := source.(interface{ Invalidate() }); refreshable {
				t.Fatal("the ADO source became refreshable")
			}
		})
	}
}

// TestStageADOCredentialSourceRefreshesWithAGrant: with a grant, the ADO
// delivered source is refreshable, so send()'s 401 path re-resolves it.
func TestStageADOCredentialSourceRefreshesWithAGrant(t *testing.T) {
	const delivered = "ado-delivered-bearer-value-0123456789"
	_, minted, _ := playStageWithGrant(t, delivered, time.Now().Add(40*time.Minute))
	t.Setenv(executor.RepoAuthSchemeEnvVar, "bearer")
	source, err := stageADOCredentialSource(capability.RepoPush, delivered)
	if err != nil {
		t.Fatal(err)
	}
	refreshable, ok := source.(interface{ Invalidate() })
	if !ok {
		t.Fatalf("source %T is not refreshable", source)
	}
	refreshable.Invalidate()
	credential, err := source.Credential(context.Background())
	if err != nil || credential.Secret != minted.value(1) || credential.Kind != providers.ADOCredentialKindBearer {
		t.Fatalf("credential after invalidate = %+v, %v", credential, err)
	}
}

// TestClassifyProviderErrorGivesADORejectionANeutralCode: an Azure DevOps
// delivered-credential rejection is provider_auth_failed, typed or as text;
// a GitHub 401 keeps github_auth_failed; both are auth-failure codes.
func TestClassifyProviderErrorGivesADORejectionANeutralCode(t *testing.T) {
	source, err := providers.NewADODeliveredCredentialSource(providers.ADOCredentialKindBearer, "value-value-value", "repo:push")
	if err != nil {
		t.Fatal(err)
	}
	provider := providers.NewADOProvider("org", "project", "", providers.WithADOCredentialSource(source), func(p *providers.ADOProvider) {
		p.Client = httpClientFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("TF400813"))}, nil
		})
	})
	_, rejected := provider.AuthenticatedLogin(context.Background())
	if !errors.Is(rejected, providers.ErrADODeliveredCredentialRejected) {
		t.Fatalf("setup: %v", rejected)
	}
	for name, err := range map[string]error{"typed": rejected, "as text": errors.New(rejected.Error())} {
		code, retryable, _ := classifyProviderError(err)
		if code != providers.ErrorCodeProviderAuthFailed || retryable || !providers.IsAuthFailureCode(code) {
			t.Fatalf("%s: code = %q retryable=%v", name, code, retryable)
		}
	}
	code, _, _ := classifyProviderError(errors.New("GET https://api.github.com/user: status 401: Bad credentials"))
	if code != providers.ErrorCodeAuthFailed || !providers.IsAuthFailureCode(code) {
		t.Fatalf("github 401 code = %q", code)
	}
}

type httpClientFunc func(*http.Request) (*http.Response, error)

func (f httpClientFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

// TestCredentialRefreshThroughTheAuthenticatedDaemonAPI is the non-loopback
// posture (a daemon serving stage pods behind podauth + RequireRoles): the
// grant authenticates as a grant principal and refreshes; the run's own pod
// token cannot use the route.
func TestCredentialRefreshThroughTheAuthenticatedDaemonAPI(t *testing.T) {
	api := &switchingHandler{}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	service, minted, runID := newRefreshFixture(t, server.URL)
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), stdlog.New(io.Discard, "", 0),
		httpapi.WithCredentialService(service), httpapi.WithAuthenticator(authenticator.WithCredentialGrants(service.grantKey())))
	if err != nil {
		t.Fatal(err)
	}
	api.set(handler)
	grant, err := service.MintStageGrant(pushBranchEnvelope(runID, 1), []string{"repo:push"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := (&dispatcher.CredentialRefreshClient{BaseURL: server.URL, Grant: grant.Token}).Refresh(context.Background(), "repo:push")
	if err != nil || fresh.Value != minted.value(1) {
		t.Fatalf("refresh with the grant = %+v, %v", fresh, err)
	}
	podToken, err := registry.Mint(runID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&dispatcher.CredentialRefreshClient{BaseURL: server.URL, Grant: podToken}).Refresh(context.Background(), "repo:push")
	var refusal *dispatcher.CredentialResolveRefusal
	if !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden {
		t.Fatalf("refresh with the pod token = %v, want 403", err)
	}
	if minted.calls != 1 {
		t.Fatalf("minted %d values, want only the grant's refresh", minted.calls)
	}
}
